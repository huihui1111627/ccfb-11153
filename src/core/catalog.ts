import type { DB } from './db.js';
import { all, get, nextGlobalVersion, run, tx } from './db.js';
import type { ChannelCode, ChannelShop, Sku } from './types.js';

export function getSku(db: DB, skuId: string): Sku | undefined {
  const row = get<{ id: string; title: string; is_bundle: number; safety_stock: number }>(
    db,
    'SELECT id, title, is_bundle, safety_stock FROM skus WHERE id = ?',
    skuId,
  );
  if (!row) return undefined;
  return { id: row.id, title: row.title, isBundle: row.is_bundle === 1, safetyStock: Number(row.safety_stock) };
}

export function getOnHand(db: DB, skuId: string): number {
  const row = get<{ on_hand: number }>(db, 'SELECT on_hand FROM stock WHERE sku_id = ?', skuId);
  return row ? Number(row.on_hand) : 0;
}

export function setOnHand(db: DB, skuId: string, qty: number): void {
  run(
    db,
    'INSERT INTO stock(sku_id, on_hand) VALUES(?, ?) ON CONFLICT(sku_id) DO UPDATE SET on_hand = excluded.on_hand',
    skuId,
    Math.max(0, Math.floor(qty)),
  );
}

export function upsertSku(
  db: DB,
  input: { id: string; title: string; safetyStock?: number; components?: Array<{ skuId: string; qty: number }> },
): void {
  tx(db, () => {
    const isBundle = !!input.components && input.components.length > 0;
    run(
      db,
      `INSERT INTO skus(id, title, is_bundle, safety_stock, created_at) VALUES(?,?,?,?,?)
       ON CONFLICT(id) DO UPDATE SET title=excluded.title, is_bundle=excluded.is_bundle, safety_stock=excluded.safety_stock`,
      input.id,
      input.title,
      isBundle ? 1 : 0,
      input.safetyStock ?? 0,
      Date.now(),
    );
    run(db, 'INSERT INTO stock(sku_id, on_hand) VALUES(?, 0) ON CONFLICT(sku_id) DO NOTHING', input.id);
    if (input.components) {
      const comps = input.components.filter((c) => c.skuId !== input.id);
      run(db, 'DELETE FROM bundle_components WHERE bundle_sku_id = ?', input.id);
      for (const c of comps) {
        if (!getSku(db, c.skuId)) throw new Error(`子件 SKU 不存在: ${c.skuId}`);
        if (c.qty <= 0) throw new Error(`子件数量必须为正: ${c.skuId}`);
        run(db, 'INSERT INTO bundle_components(bundle_sku_id, component_sku_id, qty) VALUES(?,?,?)', input.id, c.skuId, c.qty);
      }
    }
  });
}

export function listSkus(db: DB) {
  const rows = all<{ id: string; title: string; is_bundle: number; safety_stock: number }>(
    db,
    'SELECT id, title, is_bundle, safety_stock FROM skus ORDER BY id',
  );
  return rows.map((s) => {
    const components = all<{ component_sku_id: string; qty: number }>(
      db,
      'SELECT component_sku_id, qty FROM bundle_components WHERE bundle_sku_id = ? ORDER BY component_sku_id',
      s.id,
    ).map((c) => ({ skuId: c.component_sku_id, qty: Number(c.qty) }));
    return {
      id: s.id,
      title: s.title,
      isBundle: s.is_bundle === 1,
      safetyStock: Number(s.safety_stock),
      onHand: getOnHand(db, s.id),
      components,
      sellable: skuSellable(db, s.id),
      belowSafety: getOnHand(db, s.id) < Number(s.safety_stock),
    };
  });
}

export function listShops(db: DB): ChannelShop[] {
  return all<{
    id: string;
    channel: ChannelCode;
    name: string;
    alloc_mode: 'fixed' | 'ratio';
    alloc_value: number;
    is_small: number;
    priority: number;
    active: number;
    base_url: string;
  }>(db, 'SELECT * FROM shops ORDER BY priority, id').map((r) => ({
    id: r.id,
    channel: r.channel,
    name: r.name,
    allocMode: r.alloc_mode,
    allocValue: Number(r.alloc_value),
    isSmall: r.is_small === 1,
    priority: Number(r.priority),
    active: r.active === 1,
    baseUrl: r.base_url,
  }));
}

export function getShop(db: DB, shopId: string): ChannelShop | undefined {
  return listShops(db).find((s) => s.id === shopId);
}

export function upsertShop(
  db: DB,
  input: Omit<ChannelShop, 'active'> & { active?: boolean },
): void {
  if (!['fixed', 'ratio'].includes(input.allocMode)) throw new Error('allocMode 必须是 fixed 或 ratio');
  if (input.allocMode === 'ratio' && (input.allocValue < 0 || input.allocValue > 1)) {
    throw new Error('ratio 模式的 allocValue 必须在 0..1 之间');
  }
  if (input.allocMode === 'fixed' && (input.allocValue < 0 || !Number.isFinite(input.allocValue))) {
    throw new Error('fixed 模式的 allocValue 必须是非负数');
  }
  run(
    db,
    `INSERT INTO shops(id, channel, name, alloc_mode, alloc_value, is_small, priority, active, base_url)
     VALUES(?,?,?,?,?,?,?,?,?)
     ON CONFLICT(id) DO UPDATE SET channel=excluded.channel, name=excluded.name, alloc_mode=excluded.alloc_mode,
       alloc_value=excluded.alloc_value, is_small=excluded.is_small, priority=excluded.priority,
       active=excluded.active, base_url=excluded.base_url`,
    input.id,
    input.channel,
    input.name,
    input.allocMode,
    input.allocValue,
    input.isSmall ? 1 : 0,
    input.priority,
    input.active === false ? 0 : 1,
    input.baseUrl ?? '',
  );
}

// ---------------------------------------------------------------------------
// 可用量计算
// ---------------------------------------------------------------------------

/** 渠道分配的未预占余量 */
export function channelUnreserved(db: DB, shopId: string, skuId: string): number {
  const row = get<{ allocated: number; reserved: number }>(
    db,
    'SELECT allocated, reserved FROM allocations WHERE shop_id = ? AND sku_id = ?',
    shopId,
    skuId,
  );
  if (!row) return 0;
  return Math.max(0, Number(row.allocated) - Number(row.reserved));
}

/** 共享池总量 = 实物库存 - 已分给渠道的总量（可为 0，不应为负） */
export function sharedPoolQty(db: DB, skuId: string): number {
  const onHand = getOnHand(db, skuId);
  const allocated = Number(
    get<{ total: number }>(db, 'SELECT COALESCE(SUM(allocated),0) AS total FROM allocations WHERE sku_id = ?', skuId)?.total ?? 0,
  );
  return onHand - allocated;
}

export function sharedReserved(db: DB, skuId: string): number {
  return Number(get<{ reserved: number }>(db, 'SELECT reserved FROM shared WHERE sku_id = ?', skuId)?.reserved ?? 0);
}

export function sharedUnreserved(db: DB, skuId: string): number {
  return Math.max(0, sharedPoolQty(db, skuId) - sharedReserved(db, skuId));
}

/** 单件 SKU 的可售量（全渠道 + 共享池） */
export function plainSellable(db: DB, skuId: string): number {
  return Math.max(0, getOnHand(db, skuId) - totalReserved(db, skuId));
}

export function totalReserved(db: DB, skuId: string): number {
  const ch = Number(
    get<{ total: number }>(db, 'SELECT COALESCE(SUM(reserved),0) AS total FROM allocations WHERE sku_id = ?', skuId)?.total ?? 0,
  );
  return ch + sharedReserved(db, skuId);
}

/** 渠道维度可售（渠道余量 + 共享池余量）：渠道下单可以借用共享池 */
export function channelSellable(db: DB, skuId: string, shopId?: string): number {
  const sku = getSku(db, skuId);
  if (!sku) return 0;
  if (sku.isBundle) return bundleSellable(db, skuId, shopId);
  // 全渠道视角：plainSellable 已包含共享池，不能重复加
  if (!shopId) return plainSellable(db, skuId);
  return channelUnreserved(db, shopId, skuId) + sharedUnreserved(db, skuId);
}

/** 全渠道总可售（不重复计算共享池） */
export function skuSellable(db: DB, skuId: string): number {
  const sku = getSku(db, skuId);
  if (!sku) return 0;
  if (sku.isBundle) return bundleSellable(db, skuId);
  return plainSellable(db, skuId);
}

export function bundleComponents(db: DB, bundleSkuId: string): Array<{ skuId: string; qty: number }> {
  return all<{ component_sku_id: string; qty: number }>(
    db,
    'SELECT component_sku_id, qty FROM bundle_components WHERE bundle_sku_id = ? ORDER BY component_sku_id',
    bundleSkuId,
  ).map((r) => ({ skuId: r.component_sku_id, qty: Number(r.qty) }));
}

/** 套装可售数 = min(各子件可售数 / 用量)。渠道维度时，子件可售 = 渠道余量 + 共享池余量 */
export function bundleSellable(db: DB, bundleSkuId: string, shopId?: string): number {
  const comps = bundleComponents(db, bundleSkuId);
  if (comps.length === 0) return 0;
  let min = Number.POSITIVE_INFINITY;
  for (const c of comps) {
    const compAvail = channelSellable(db, c.skuId, shopId);
    min = Math.min(min, Math.floor(compAvail / c.qty));
  }
  return Math.max(0, min);
}

export interface AllocationView {
  skuId: string;
  title: string;
  isBundle: boolean;
  onHand: number;
  safetyStock: number;
  channelAllocated: number;
  sharedQty: number;
  sharedReserved: number;
  sharedAvailable: number;
  sellable: number;
  channels: Array<{
    shopId: string;
    name: string;
    channel: ChannelCode;
    allocated: number;
    reserved: number;
    sold: number;
    available: number;
  }>;
}

export function allocationMatrix(db: DB): AllocationView[] {
  const shops = listShops(db).filter((s) => s.active);
  return listSkus(db).map((sku) => {
    const channels = shops.map((shop) => {
      const row = get<{ allocated: number; reserved: number; sold: number }>(
        db,
        'SELECT allocated, reserved, sold FROM allocations WHERE shop_id = ? AND sku_id = ?',
        shop.id,
        sku.id,
      );
      const allocated = row ? Number(row.allocated) : 0;
      const reserved = row ? Number(row.reserved) : 0;
      return {
        shopId: shop.id,
        name: shop.name,
        channel: shop.channel,
        allocated,
        reserved,
        sold: row ? Number(row.sold) : 0,
        available: Math.max(0, allocated - reserved),
      };
    });
    return {
      skuId: sku.id,
      title: sku.title,
      isBundle: sku.isBundle,
      onHand: sku.onHand,
      safetyStock: sku.safetyStock,
      channelAllocated: channels.reduce((s, c) => s + c.allocated, 0),
      sharedQty: sharedPoolQty(db, sku.id),
      sharedReserved: sharedReserved(db, sku.id),
      sharedAvailable: sharedUnreserved(db, sku.id),
      sellable: skuSellable(db, sku.id),
      channels,
    };
  });
}

// ---------------------------------------------------------------------------
// 渠道分配
// ---------------------------------------------------------------------------

/** 计算各渠道目标分配额。fixed 取固定值，ratio 按 可分配量 * 比例；总和不超过可分配量，超额部分按“小渠道先削”规则削减。 */
export function desiredAllocation(db: DB, skuId: string): Map<string, number> {
  const shops = listShops(db).filter((s) => s.active);
  const onHand = getOnHand(db, skuId);
  const safety = getSku(db, skuId)?.safetyStock ?? 0;
  const usable = Math.max(0, onHand - safety);

  const raw = new Map<string, number>();
  for (const shop of shops) {
    const want = shop.allocMode === 'ratio' ? Math.floor(usable * shop.allocValue) : Math.floor(shop.allocValue);
    raw.set(shop.id, Math.max(0, want));
  }

  let total = 0;
  for (const v of raw.values()) total += v;
  let excess = total - usable;
  if (excess > 0) {
    const order = [...shops].sort((a, b) => {
      if (a.isSmall !== b.isSmall) return a.isSmall ? -1 : 1;
      return b.priority - a.priority;
    });
    for (const shop of order) {
      if (excess <= 0) break;
      const cur = raw.get(shop.id) ?? 0;
      const cut = Math.min(cur, excess);
      raw.set(shop.id, cur - cut);
      excess -= cut;
    }
  }
  // 不把任何渠道压到已预占量以下（已预占是真实订单，必须能供货）
  for (const shop of shops) {
    const reserved = Number(
      get<{ reserved: number }>(db, 'SELECT reserved FROM allocations WHERE shop_id = ? AND sku_id = ?', shop.id, skuId)?.reserved ?? 0,
    );
    raw.set(shop.id, Math.max(reserved, raw.get(shop.id) ?? 0));
  }

  // 安全线护栏：小渠道只能吃“实物 − 安全库存 − 主渠道分配”之外的多余量。
  // 这样大促越卖越少时，小渠道分配会一路收缩到 0，把货让给主渠道/共享池。
  const mainWanted = shops
    .filter((shop) => !shop.isSmall)
    .reduce((sum, shop) => sum + (raw.get(shop.id) ?? 0), 0);
  const smallCap = Math.max(0, usable - mainWanted);
  let smallTotal = shops.filter((shop) => shop.isSmall).reduce((sum, shop) => sum + (raw.get(shop.id) ?? 0), 0);
  if (smallTotal > smallCap) {
    let excess = smallTotal - smallCap;
    for (const shop of [...shops].filter((s) => s.isSmall).sort((a, b) => b.priority - a.priority)) {
      if (excess <= 0) break;
      const reserved = Number(
        get<{ reserved: number }>(db, 'SELECT reserved FROM allocations WHERE shop_id = ? AND sku_id = ?', shop.id, skuId)?.reserved ?? 0,
      );
      const cur = raw.get(shop.id) ?? 0;
      const floor = Math.max(reserved, 0);
      const cut = Math.min(cur - floor, excess);
      if (cut <= 0) continue;
      raw.set(shop.id, cur - cut);
      excess -= cut;
    }
    smallTotal = shops.filter((shop) => shop.isSmall).reduce((sum, shop) => sum + (raw.get(shop.id) ?? 0), 0);
    void smallTotal;
  }
  return raw;
}

/** 把渠道分配落到目标值；差额通过共享池吸收 */
export function rebalanceAllocation(
  db: DB,
  skuId: string,
  reason: string,
  refId: string,
  opts: { shrinkSmall?: number } = {},
): void {
  const target = desiredAllocation(db, skuId);
  const shops = listShops(db).filter((s) => s.active);
  for (const shop of shops) {
    let want = target.get(shop.id) ?? 0;
    const row = get<{ allocated: number; reserved: number }>(
      db,
      'SELECT allocated, reserved FROM allocations WHERE shop_id = ? AND sku_id = ?',
      shop.id,
      skuId,
    );
    const cur = row ? Number(row.allocated) : 0;
    const reserved = row ? Number(row.reserved) : 0;
    if (opts.shrinkSmall && shop.isSmall && want > reserved) {
      // 安全线收紧：小渠道只保留已预占 + 剩余目标的一小部分
      want = reserved + Math.floor((want - reserved) * opts.shrinkSmall);
    }
    want = Math.max(reserved, want);
    if (want === cur) continue;
    run(
      db,
      `INSERT INTO allocations(shop_id, sku_id, allocated, reserved, sold) VALUES(?,?,?,?,0)
       ON CONFLICT(shop_id, sku_id) DO UPDATE SET allocated = excluded.allocated`,
      shop.id,
      skuId,
      want,
      reserved,
    );
    writeLedger(db, {
      skuId,
      channel: shop.channel,
      shopId: shop.id,
      reason: 'ALLOCATE',
      deltaAllocated: want - cur,
      refType: 'ALLOCATION',
      refId,
      note: reason,
    });
  }
}

export function writeLedger(
  db: DB,
  entry: {
    skuId: string;
    channel: string;
    shopId?: string | null;
    reason: string;
    deltaOnHand?: number;
    deltaReserved?: number;
    deltaAllocated?: number;
    deltaSold?: number;
    refType: string;
    refId: string;
    note?: string;
  },
): void {
  run(
    db,
    `INSERT INTO ledger(sku_id, channel, shop_id, reason, delta_on_hand, delta_reserved, delta_allocated, delta_sold, ref_type, ref_id, note, created_at)
     VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
    entry.skuId,
    entry.channel,
    entry.shopId ?? null,
    entry.reason,
    entry.deltaOnHand ?? 0,
    entry.deltaReserved ?? 0,
    entry.deltaAllocated ?? 0,
    entry.deltaSold ?? 0,
    entry.refType,
    entry.refId,
    entry.note ?? '',
    Date.now(),
  );
}

/**
 * 库存版本变化后：落渠道同步状态、入同步任务队列。
 * 版本号全局单调递增，渠道侧按 version 幂等覆盖（旧版本不会回滚新库存）。
 */
export function markSkuChanged(db: DB, skuId: string, reason: string): number {
  const shops = listShops(db).filter((s) => s.active);
  const version = nextGlobalVersion(db);
  // 套装变更影响其子件可用量，子件渠道也要同步
  const affected = collectAffectedSkus(db, skuId);
  for (const affectedSku of affected) {
    for (const shop of shops) {
      const avail = Math.max(0, channelSellable(db, affectedSku, shop.id));
      run(
        db,
        `INSERT INTO sync_state(shop_id, sku_id, last_version, last_available, updated_at)
         VALUES(?,?,?,?,?)
         ON CONFLICT(shop_id, sku_id) DO UPDATE SET last_version=excluded.last_version, last_available=excluded.last_available, updated_at=excluded.updated_at`,
        shop.id,
        affectedSku,
        version,
        avail,
        Date.now(),
      );
      enqueueSync(db, shop.channel, shop.id, affectedSku, version, avail, reason);
    }
  }
  return version;
}

function collectAffectedSkus(db: DB, skuId: string): string[] {
  const set = new Set<string>([skuId]);
  for (const row of all<{ bundle_sku_id: string }>(db, 'SELECT bundle_sku_id FROM bundle_components WHERE component_sku_id = ?', skuId)) {
    set.add(row.bundle_sku_id);
  }
  return [...set];
}

/**
 * 同步任务按 (shopId, skuId) 合并：同一 SKU 只保留一个待推送任务，
 * 新版本到来时覆盖旧任务内容。版本号单调递增，平台侧按 version 拒绝回退。
 */
export function enqueueSync(
  db: DB,
  channel: string,
  shopId: string,
  skuId: string,
  version: number,
  available: number,
  reason: string,
): void {
  const existing = get<{ id: number; version: number; status: string }>(
    db, 'SELECT id, version, status FROM sync_jobs WHERE shop_id = ? AND sku_id = ?', shopId, skuId,
  );
  if (existing) {
    if (existing.status === 'DONE') {
      // 已完成但来了更新版本：重新排队
      run(
        db,
        `UPDATE sync_jobs SET version = ?, available = ?, status = 'PENDING', attempts = 0,
           next_attempt_at = 0, last_error = NULL, updated_at = ? WHERE id = ?`,
        version, available, Date.now(), existing.id,
      );
      return;
    }
    if (version >= Number(existing.version)) {
      run(
        db,
        'UPDATE sync_jobs SET version = ?, available = ?, updated_at = ? WHERE id = ?',
        version, available, Date.now(), existing.id,
      );
    }
    void reason;
    return;
  }
  run(
    db,
    `INSERT INTO sync_jobs(channel, shop_id, sku_id, version, available, status, attempts, next_attempt_at, created_at, updated_at)
     VALUES(?,?,?,?,?,'PENDING',0,0,?,?)`,
    channel, shopId, skuId, version, available, Date.now(), Date.now(),
  );
  void reason;
}

/** 某渠道某 SKU 的“最新已确认同步版本” */
export function syncedVersion(db: DB, shopId: string, skuId: string): number {
  const row = get<{ synced_version: number }>(
    db, 'SELECT synced_version FROM sync_state WHERE shop_id = ? AND sku_id = ?', shopId, skuId,
  );
  return row ? Number(row.synced_version) : 0;
}

/** 保存一件实际库存变动并写流水 */
export function applyOnHandDelta(
  db: DB,
  skuId: string,
  delta: number,
  meta: { channel: string; shopId?: string | null; reason: string; refType: string; refId: string; note?: string },
): void {
  if (delta === 0) return;
  const before = getOnHand(db, skuId);
  const after = before + delta;
  if (after < 0) {
    throw new Error(`库存不足：${skuId} 现有 ${before}，需要 ${-delta}`);
  }
  setOnHand(db, skuId, after);
  writeLedger(db, {
    skuId,
    channel: meta.channel,
    shopId: meta.shopId ?? null,
    reason: meta.reason,
    deltaOnHand: delta,
    refType: meta.refType,
    refId: meta.refId,
    note: meta.note ?? '',
  });
}
