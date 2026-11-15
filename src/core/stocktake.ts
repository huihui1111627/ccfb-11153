import { type DB, all, get, run, tx } from './db.js';
import { applyOnHandDelta, getOnHand, listShops, markSkuChanged, writeLedger } from './catalog.js';
import { raiseAlert } from './alerts.js';
import type { InventoryEngine } from './engine.js';

export type StocktakeRule = 'pro_rata' | 'shared_first' | 'channel_first';

export interface StocktakeInput {
  stocktakeId: string;
  skuId: string;
  counted: number;
  rule?: StocktakeRule;
  note?: string;
}

export interface StocktakeResult {
  stocktakeId: string;
  skuId: string;
  before: number;
  counted: number;
  delta: number;
  channelAdjustments: Array<{ shopId: string; channel: string; delta: number }>;
  sharedDelta: number;
  releasedOrderIds: string[];
  note: string;
}

/**
 * 仓库盘点：实物数按规则摊回各渠道。
 *
 * 守恒前提：onHand === Σ allocated + 共享池量，且 Σ reserved <= onHand。
 * 盘亏时如果未付款预占已经超过实盘，必须撤销部分未付款订单（RELEASE），
 * 而不是把渠道压成负数——这样“账实差”和“超卖”才会同时为零。
 *
 * 规则：
 * - pro_rata      ：差异按各渠道 allocated 权重分摊，尾差进共享池
 * - shared_first  ：共享池先吸收差异，不足部分再按权重摊
 * - channel_first ：渠道先按权重承担，剩余进共享池
 */
export function applyStocktake(db: DB, engine: InventoryEngine, input: StocktakeInput): StocktakeResult {
  const rule = input.rule ?? 'pro_rata';
  if (!Number.isInteger(input.counted) || input.counted < 0) throw new Error('盘点数必须是非负整数');

  return tx(db, () => {
    const before = getOnHand(db, input.skuId);
    const delta = input.counted - before;

    // 1) 先撤销超出实盘的未付款预占（从最晚下单开始），保持 reserved <= counted
    const releasedOrderIds = releaseExcessReservations(db, engine, input.skuId, input.counted, input.stocktakeId);

    // 2) 记录变动前的分配快照，作为摊回权重
    const totalAllocatedBefore = Number(
      get<{ total: number }>(db, 'SELECT COALESCE(SUM(allocated),0) AS total FROM allocations WHERE sku_id = ?', input.skuId)?.total ?? 0,
    );
    const poolBefore = before - totalAllocatedBefore;

    // 3) 实盘值落账
    if (delta !== 0) {
      applyOnHandDelta(db, input.skuId, delta, {
        channel: 'WAREHOUSE',
        reason: 'STOCKTAKE',
        refType: 'STOCKTAKE',
        refId: input.stocktakeId,
        note: input.note ?? `盘点 ${rule}`,
      });
    }

    // 4) 差异摊回渠道（用变动前的分配快照做权重基准）
    const channelAdjustments = distributeDelta(db, input, rule, delta, totalAllocatedBefore, poolBefore);

    // 5) 兜底守恒：盘亏时共享池可能被压成负数（渠道分配占满实物），回收渠道分配
    let poolNow = input.counted - Number(
      get<{ total: number }>(db, 'SELECT COALESCE(SUM(allocated),0) AS total FROM allocations WHERE sku_id = ?', input.skuId)?.total ?? 0,
    );
    for (let guard = 0; guard < 64 && poolNow < 0; guard++) {
      const biggest = get<{ shop_id: string; channel: string; allocated: number; reserved: number }>(
        db,
        `SELECT a.shop_id, s.channel, a.allocated, a.reserved FROM allocations a
         JOIN shops s ON s.id = a.shop_id
         WHERE a.sku_id = ? AND a.allocated > a.reserved
         ORDER BY a.allocated DESC LIMIT 1`,
        input.skuId,
      );
      if (!biggest) break;
      const cut = Math.min(-poolNow, Number(biggest.allocated) - Number(biggest.reserved));
      if (cut <= 0) break;
      run(db, 'UPDATE allocations SET allocated = allocated - ? WHERE shop_id = ? AND sku_id = ?', cut, biggest.shop_id, input.skuId);
      writeLedger(db, {
        skuId: input.skuId, channel: biggest.channel, shopId: biggest.shop_id, reason: 'STOCKTAKE',
        deltaAllocated: -cut, refType: 'STOCKTAKE', refId: input.stocktakeId,
        note: '盘亏后共享池为负，回收渠道分配',
      });
      const found = channelAdjustments.find((c) => c.shopId === biggest.shop_id);
      if (found) found.delta -= cut;
      else channelAdjustments.push({ shopId: biggest.shop_id, channel: biggest.channel, delta: -cut });
      poolNow = input.counted - Number(
        get<{ total: number }>(db, 'SELECT COALESCE(SUM(allocated),0) AS total FROM allocations WHERE sku_id = ?', input.skuId)?.total ?? 0,
      );
    }
    const sharedDelta = Number(
      get<{ total: number }>(
        db,
        `SELECT COALESCE(SUM(delta_allocated),0) AS total FROM ledger
         WHERE ref_type = 'STOCKTAKE' AND ref_id = ? AND shop_id IS NULL`,
        input.stocktakeId,
      )?.total ?? 0,
    );

    run(
      db,
      'INSERT INTO stocktakes(id, sku_id, counted, before_qty, delta, rule, note, created_at) VALUES(?,?,?,?,?,?,?,?)',
      input.stocktakeId, input.skuId, input.counted, before, delta, rule, input.note ?? '', Date.now(),
    );
    markSkuChanged(db, input.skuId, 'stocktake');

    return {
      stocktakeId: input.stocktakeId,
      skuId: input.skuId,
      before,
      counted: input.counted,
      delta,
      channelAdjustments,
      sharedDelta,
      releasedOrderIds,
      note: input.note ?? '',
    };
  });
}

/** 盘亏导致实盘 < 未付款预占时，撤销最晚的未付款订单直到预占 <= 实盘 */
function releaseExcessReservations(
  db: DB,
  engine: InventoryEngine,
  skuId: string,
  counted: number,
  stocktakeId: string,
): string[] {
  const released: string[] = [];
  for (let guard = 0; guard < 10000; guard++) {
    const active = get<{ qty: number }>(
      db,
      `SELECT COALESCE(SUM(r.qty),0) AS qty FROM reservations r
       JOIN orders o ON o.id = r.order_id
       WHERE r.sku_id = ? AND r.state = 'ACTIVE' AND o.status = 'RESERVED'`,
      skuId,
    );
    if (Number(active?.qty ?? 0) <= counted) break;
    const candidate = get<{ id: string }>(
      db,
      `SELECT o.id FROM orders o JOIN reservations r ON r.order_id = o.id
       WHERE o.status = 'RESERVED' AND r.sku_id = ? AND r.state = 'ACTIVE'
       ORDER BY o.created_at DESC, o.id DESC LIMIT 1`,
      skuId,
    );
    if (!candidate) break;
    const r = engine.release(candidate.id, `盘点 ${stocktakeId} 后实物不足，撤销未付款预占`);
    if (!r.ok && !r.alreadyReleased) break;
    released.push(candidate.id);
    if (released.length > 2000) break;
  }
  return released;
}

function distributeDelta(
  db: DB,
  input: StocktakeInput,
  rule: StocktakeRule,
  delta: number,
  totalAllocatedBefore: number,
  poolBefore: number,
): Array<{ shopId: string; channel: string; delta: number }> {
  const shops = listShops(db).filter((s) => s.active);
  const rows = shops
    .filter((shop) => {
      const a = get<{ allocated: number; reserved: number }>(
        db, 'SELECT allocated, reserved FROM allocations WHERE shop_id = ? AND sku_id = ?', shop.id, input.skuId,
      );
      return !!a && (Number(a.allocated) > 0 || Number(a.reserved) > 0);
    })
    .map((shop) => {
      const a = get<{ allocated: number; reserved: number }>(
        db, 'SELECT allocated, reserved FROM allocations WHERE shop_id = ? AND sku_id = ?', shop.id, input.skuId,
      )!;
      return { shop, allocated: Number(a.allocated), reserved: Number(a.reserved) };
    });

  const applied: Array<{ shopId: string; channel: string; delta: number }> = [];
  if (delta === 0) return applied;

  const apply = (shopId: string, channel: string, d: number) => {
    if (d === 0) return;
    run(db, 'UPDATE allocations SET allocated = allocated + ? WHERE shop_id = ? AND sku_id = ?', d, shopId, input.skuId);
    writeLedger(db, {
      skuId: input.skuId, channel, shopId, reason: 'STOCKTAKE', deltaAllocated: d,
      refType: 'STOCKTAKE', refId: input.stocktakeId, note: `盘点差异摊回 (${rule})`,
    });
    const found = applied.find((a) => a.shopId === shopId);
    if (found) found.delta += d;
    else applied.push({ shopId, channel, delta: d });
  };
  const toShared = (amount: number, note: string) => {
    if (amount === 0) return;
    writeLedger(db, {
      skuId: input.skuId, channel: 'SHARED', reason: 'STOCKTAKE', deltaAllocated: amount,
      refType: 'STOCKTAKE', refId: input.stocktakeId, note,
    });
  };

  let remaining = delta;

  // shared_first：盘盈全进共享池；盘亏先由共享池吸收（不能把共享池压成负数）
  if (rule === 'shared_first') {
    if (remaining >= 0) {
      toShared(remaining, '盘盈进入共享池');
      return applied;
    }
    const absorb = Math.min(-remaining, Math.max(0, poolBefore));
    if (absorb > 0) toShared(-absorb, '盘亏由共享池先行承担');
    remaining += absorb;
  }

  // channel_first / pro_rata 的盘盈：先按权重分给渠道，尾差进共享池
  // 盘亏：按权重从渠道回收，但不低于已预占（reserved 是不可动用的真实订单）
  if (rows.length > 0 && totalAllocatedBefore > 0 && remaining !== 0) {
    let assigned = 0;
    for (const row of rows) {
      if (remaining === 0) break;
      const share = Math.trunc((remaining * row.allocated) / totalAllocatedBefore);
      if (share === 0) continue;
      const clamped = remaining < 0 ? Math.max(share, row.reserved - row.allocated) : share;
      if (clamped === 0) continue;
      apply(row.shop.id, row.shop.channel, clamped);
      assigned += clamped;
      remaining -= clamped;
    }
    // 尾差：给当前分配最大的渠道，盘亏仍受 reserved 保护
    if (remaining !== 0) {
      const sorted = [...rows].sort((a, b) => b.allocated - a.allocated);
      for (const row of sorted) {
        if (remaining === 0) break;
        const cur = Number(
          get<{ allocated: number }>(db, 'SELECT allocated FROM allocations WHERE shop_id = ? AND sku_id = ?', row.shop.id, input.skuId)?.allocated ?? 0,
        );
        const reserved = Number(
          get<{ reserved: number }>(db, 'SELECT reserved FROM allocations WHERE shop_id = ? AND sku_id = ?', row.shop.id, input.skuId)?.reserved ?? 0,
        );
        const room = remaining < 0 ? Math.max(reserved - cur, remaining) : remaining;
        if (room === 0) continue;
        apply(row.shop.id, row.shop.channel, room);
        remaining -= room;
      }
    }
  }

  // 剩余差额进共享池（盘亏时释放预占已保证可行；为负说明数据异常，由调用方告警）
  toShared(remaining, remaining > 0 ? '盘盈尾差进共享池' : '盘亏超出渠道可回收部分，由共享池承担');
  return applied;
}
