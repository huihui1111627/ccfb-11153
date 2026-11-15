import { setTimeout as sleep } from 'node:timers/promises';
import { type DB, openDb } from '../core/db.js';
import { seed } from '../core/seed.js';
import { InventoryHubServer } from '../server/hub.js';
import { MockPlatform, type ChaosConfig } from '../platforms/mock.js';
import { makeHttpInventoryPusher, registerShopBaseUrl, setHubBaseUrl } from '../platforms/client.js';
import { SEED_SHOPS } from '../core/seed.js';
import { syncHealth, syncQueueSize } from '../core/sync.js';
import { listShops } from '../core/catalog.js';
import { repairLaggingSync, SyncWorker } from '../core/sync.js';
import { InventoryEngine } from '../core/engine.js';
import { OrderEventProcessor } from '../core/events.js';
import { getOnHand, listSkus } from '../core/catalog.js';
import { invariantViolations, reconcile } from '../server/hub.js';
import type { ChannelCode, OrderLine } from '../core/types.js';

export interface HarnessOptions {
  dbPath?: string;
  stockPerSku?: number;
  chaos?: Partial<ChaosConfig>;
  /** 每渠道店铺的 chaos 覆盖 */
  perShopChaos?: Record<string, Partial<ChaosConfig>>;
  seedData?: boolean;
}

export interface Harness {
  db: DB;
  hub: InventoryHubServer;
  hubUrl: string;
  platforms: Map<string, MockPlatform>;
  engine: InventoryEngine;
  processor: OrderEventProcessor;
  syncWorker: SyncWorker;
  close: () => Promise<void>;
}

/** 启动一套完整环境：中台 + 4 个 mock 平台，并把彼此的地址接好 */
export async function startHarness(opts: HarnessOptions = {}): Promise<Harness> {
  const db = openDb(opts.dbPath ?? ':memory:');
  if (opts.seedData !== false) seed(db, { stockPerSku: opts.stockPerSku ?? 800 });

  const hub = new InventoryHubServer({
    db,
    pushInventory: makeHttpInventoryPusher(1200),
  });
  const hubPort = await hub.listen(0);
  const hubUrl = `http://127.0.0.1:${hubPort}`;
  setHubBaseUrl(hubUrl);

  const platforms = new Map<string, MockPlatform>();
  for (const shop of SEED_SHOPS) {
    const chaos = { ...(opts.chaos ?? {}), ...(opts.perShopChaos?.[shop.id] ?? {}) };
    const platform = new MockPlatform({
      channel: shop.channel as ChannelCode,
      shopId: shop.id,
      hubBaseUrl: hubUrl,
      chaos,
    });
    await platform.listen(0);
    registerShopBaseUrl(shop.id, platform.baseUrl);
    platforms.set(shop.id, platform);
  }

  // 把平台地址写回店铺，方便后台展示
  for (const shop of listShops(db)) {
    const p = platforms.get(shop.id);
    if (p) {
      db.prepare('UPDATE shops SET base_url = ? WHERE id = ?').run(p.baseUrl, shop.id);
    }
  }

  const engine = new InventoryEngine(db);
  const processor = new OrderEventProcessor(db, engine);
  const syncWorker = new SyncWorker(db, async (job) => {
    const r = await makeHttpInventoryPusher(1200)(job);
    return { ok: r.ok, retryable: !r.ok, duplicated: r.duplicated, stale: r.stale, error: r.error };
  });

  // 起步先把期初库存推到各平台，让平台缓存与中台一致
  const boot: Harness = {
    db,
    hub,
    hubUrl,
    platforms,
    engine,
    processor,
    syncWorker,
    close: async () => {
      await sleep(50);
      for (const p of platforms.values()) await p.close();
      await hub.close();
    },
  };
  await drainSyncFully(boot, { maxRounds: 10 });
  return boot;
}

/** 等待所有平台没有待推送的定时器，且同步队列清空 */
export async function settle(h: Harness, opts: { maxMs?: number; settleSync?: boolean } = {}): Promise<void> {
  const maxMs = opts.maxMs ?? 8000;
  const deadline = Date.now() + maxMs;
  const work = () =>
    [...h.platforms.values()].reduce((s, p) => s + p.pendingWork, 0) +
    (opts.settleSync ? syncQueueSize(h.db) : 0);
  let last = work();
  let idleSince = Date.now();
  while (Date.now() < deadline) {
    await sleep(20);
    const now = work();
    if (now < last) {
      last = now;
      idleSince = Date.now();
    }
    if (now === 0) return;
    // 队列在减少就不会卡死；连续 3 秒无进展再放弃（平台重试最长约 1.5 秒）
    if (Date.now() - idleSince > 3000) return;
  }
}

/** 把同步队列彻底跑完（含重试退避），必要时对账修复 */
export async function drainSyncFully(h: Harness, opts: { maxRounds?: number } = {}): Promise<void> {
  const maxRounds = opts.maxRounds ?? 30;
  for (let round = 0; round < maxRounds; round++) {
    const r = await h.syncWorker.drain(2000);
    repairLaggingSync(h.db);
    const health = syncHealth(h.db);
    const unresolved = health.reduce((s, x) => s + x.laggingSkus + x.pending + x.retrying + x.dead, 0);
    if (unresolved === 0 || (r.processed === 0 && syncQueueSize(h.db) === 0 && health.every((x) => x.laggingSkus === 0))) {
      return;
    }
    // 死信重试：对账会重新入队
    if (r.dead > 0) {
      h.db.prepare(`UPDATE sync_jobs SET status = 'PENDING', attempts = 0, next_attempt_at = 0 WHERE status = 'DEAD'`).run();
    }
    await sleep(30);
  }
}

/** 每个渠道的“应该可卖数”，与平台缓存对比，用于对账 */
export function inventoryLedgerSnapshot(h: Harness) {
  const db = h.db;
  return listSkus(db).map((sku) => {
    const shops = listShops(db).map((shop: ReturnType<typeof listShops>[number]) => {
      const state = db
        .prepare('SELECT synced_available, synced_version, last_available, last_version FROM sync_state WHERE shop_id = ? AND sku_id = ?')
        .get(shop.id, sku.id) as
        | { synced_available: number; synced_version: number; last_available: number; last_version: number }
        | undefined;
      const platform = h.platforms.get(shop.id);
      const cache = platform?.localInventory.get(sku.id);
      return {
        shopId: shop.id,
        channel: shop.channel,
        syncedAvailable: state ? Number(state.synced_available) : -1,
        desiredAvailable: state ? Number(state.last_available) : -1,
        platformAvailable: cache?.available ?? null,
        platformVersion: cache?.version ?? null,
      };
    });
    return { skuId: sku.id, onHand: getOnHand(db, sku.id), shops };
  });
}

export interface SalesConfig {
  skus: string[];
  durationMs: number;
  concurrency: number;
  orderIntervalMs: number;
  bundleSku?: string;
  /** 每个渠道的下单权重 */
  weights?: Record<string, number>;
  /** 注入盘点事件 */
  stocktake?: boolean;
  /** 注入安全线告警 */
  safetyChecks?: boolean;
  /** 每秒订单上限（模拟平台限流） */
  targetRps?: number;
  /** 支付超时窗口：超过这个时间未付款的预占会被自动释放 */
  payTimeoutMs?: number;
}

export interface SalesStats {
  attempts: number;
  accepted: number;
  rejected: number;
  paid: number;
  cancelled: number;
  perShop: Record<string, { attempts: number; accepted: number; rejected: number; retried: number }>;
  durationMs: number;
  note?: string;
  invariantBreaches?: Array<{ skuId: string; rule: string; detail: string }>;
}

function pickWeighted(weights: Record<string, number>): string {
  const entries = Object.entries(weights);
  const total = entries.reduce((s, [, w]) => s + w, 0);
  let r = Math.random() * total;
  for (const [key, w] of entries) {
    r -= w;
    if (r <= 0) return key;
  }
  return entries[entries.length - 1]![0];
}

/** 模拟一场大促：持续下单，按比例支付/取消，中途盘点并检查安全线 */
export async function runFlashSale(h: Harness, cfg: SalesConfig): Promise<SalesStats> {
  const stats: SalesStats = {
    attempts: 0,
    accepted: 0,
    rejected: 0,
    paid: 0,
    cancelled: 0,
    perShop: {},
    durationMs: 0,
  };
  const shops = [...h.platforms.keys()];
  for (const shop of shops) stats.perShop[shop] = { attempts: 0, accepted: 0, rejected: 0, retried: 0 };
  const weights = cfg.weights ?? Object.fromEntries(shops.map((s) => [s, 1]));
  const start = Date.now();
  const inflight = new Set<Promise<unknown>>();
  let nextOrderAt = start;
  let stocktakeDone = false;
  let nextSafetyAt = start + 500;
  let invariantBreaches: Array<{ skuId: string; rule: string; detail: string }> = [];

  const issueOrder = async (): Promise<void> => {
    const shopId = pickWeighted(weights);
    const platform = h.platforms.get(shopId);
    if (!platform) return;
    const sku = cfg.skus[Math.floor(Math.random() * cfg.skus.length)]!;
    const qty = Math.random() < 0.15 ? 2 : 1;
    const lines: OrderLine[] = [{ skuId: sku, qty }];
    if (cfg.bundleSku && Math.random() < 0.12) {
      lines.push({ skuId: cfg.bundleSku, qty: 1 });
    }
    stats.attempts++;
    stats.perShop[shopId]!.attempts++;
    try {
      // 走平台 HTTP 入口：真实经过限流/超时/5xx 层；4xx/5xx 等价于用户下单失败
      const order = await platform.httpPlaceOrder(lines, {
        cancelProb: 0.14,
        payProb: 0.84,
        payDelayMs: Math.floor(Math.random() * 600) + 40,
      });
      stats.perShop[shopId]!.retried += order.retried ?? 0;
      if (order.status !== 202 || order.state === 'REJECTED') {
        stats.rejected++;
        stats.perShop[shopId]!.rejected++;
      } else {
        stats.accepted++;
        stats.perShop[shopId]!.accepted++;
      }
    } catch {
      stats.rejected++;
      stats.perShop[shopId]!.rejected++;
    }
  };

  while (Date.now() - start < cfg.durationMs) {
    const now = Date.now();
    const rps = cfg.targetRps ?? 80;
    if (now >= nextOrderAt) {
      nextOrderAt = now + 1000 / rps;
      for (let i = 0; i < cfg.concurrency; i++) {
        const p = issueOrder().finally(() => inflight.delete(p));
        inflight.add(p);
      }
    }
    // 中途盘点：取一个热销 SKU，按规则摊回
    if (cfg.stocktake !== false && !stocktakeDone && now - start > cfg.durationMs * 0.45) {
      stocktakeDone = true;
      const target = cfg.skus[0]!;
      const beforeQty = getOnHand(h.db, target);
      if (beforeQty > 0) {
        const counted = Math.max(0, Math.floor(beforeQty * 0.97));
        const { applyStocktake } = await import('../core/stocktake.js');
        applyStocktake(h.db, h.engine, {
          stocktakeId: `FS-${Date.now()}`,
          skuId: target,
          counted,
          rule: 'pro_rata',
          note: '大促中途抽盘',
        });
        stats.note = `盘点 ${target}: ${beforeQty} -> ${counted}`;
      }
    }
    // 模拟后台安全线定时器：每 500ms 检查一次，低于安全线就收紧小渠道
    if (cfg.safetyChecks !== false && now >= nextSafetyAt) {
      nextSafetyAt = now + 500;
      h.engine.applySafetyTightening();
    }
    // 运行中每 ~250ms 巡检不变量，尽早发现（而不是只在收尾时）
    if (now % 250 < 6) {
      invariantBreaches = invariantBreaches.concat(invariantViolations(h.db));
      if (invariantBreaches.length > 20) invariantBreaches = invariantBreaches.slice(0, 20);
    }
    await sleep(5);
  }
  await Promise.allSettled([...inflight]);
  stats.invariantBreaches = invariantBreaches;
  stats.durationMs = Date.now() - start;

  // 结算：等所有 webhook 落地
  await settle(h, { maxMs: 15000, settleSync: false });
  const orderRows = h.db
    .prepare(`SELECT status, COUNT(*) AS n FROM orders GROUP BY status`)
    .all() as Array<{ status: string; n: number }>;
  for (const row of orderRows) {
    if (row.status === 'PAID') stats.paid = Number(row.n);
    if (row.status === 'RELEASED' || row.status === 'CANCELLED') stats.cancelled += Number(row.n);
  }
  return stats;
}

export interface AcceptanceReport {
  passed: boolean;
  invariantViolations: Array<{ skuId: string; rule: string; detail: string }>;
  oversoldQty: number;
  oversoldEvents: number;
  oversoldDetail: Array<{ orderId: string; skuId: string; qty: number; reason: string }>;
  ledgerDiscrepancies: number;
  ledgerDetail: Array<Record<string, unknown>>;
  inventoryDiscrepancies: number;
  inventoryDetail: Array<Record<string, unknown>>;
  syncLagging: number;
  deadSync: number;
  checks: Array<{ name: string; ok: boolean; detail: string }>;
}

/**
 * 验收：只认两个数——超卖件数、库存账实差，都必须为 0。
 * 额外给出同步滞后、死信、分配守恒等辅助检查。
 */
export async function acceptance(h: Harness): Promise<AcceptanceReport> {
  await settle(h, { maxMs: 15000, settleSync: false });
  await drainSyncFully(h);

  const rec = reconcile(h.db);
  const oversoldDetail = rec.oversoldRows.map((r) => ({ orderId: r.order_id, skuId: r.sku_id, qty: Number(r.qty), reason: r.reason }));
  const oversoldQty = rec.oversoldQty;
  const ledgerDiscrepancies = rec.discrepancies.filter((d) => d.ledgerDiff !== 0);
  const inventoryDiscrepancies = rec.discrepancies.filter((d) => d.diff !== 0);

  // 平台缓存 vs 中台已确认版本 的一致性（同步滞后不算账实差，但会单列）
  const health = syncHealth(h.db);
  const syncLagging = health.reduce((s, x) => s + x.laggingSkus, 0);
  const deadSync = health.reduce((s, x) => s + x.dead, 0);

  const violations = invariantViolations(h.db);

  const checks = [
    {
      name: '守恒断言（分配/预占/共享池）',
      ok: violations.length === 0,
      detail: violations.length === 0 ? '全时点不变量成立' : JSON.stringify(violations.slice(0, 5)),
    },
    {
      name: '超卖件数 = 0',
      ok: oversoldQty === 0,
      detail: oversoldQty === 0 ? '无超卖事件' : `${oversoldDetail.length} 笔超卖，共 ${oversoldQty} 件`,
    },
    {
      name: '库存账实差 = 0（流水重放 = 账面）',
      ok: ledgerDiscrepancies.length === 0,
      detail:
        ledgerDiscrepancies.length === 0
          ? '每个 SKU 的流水汇总与 stock.on_hand 一致'
          : JSON.stringify(ledgerDiscrepancies.slice(0, 5)),
    },
    {
      name: '分配守恒：Σ渠道分配 + 共享池 = 实物',
      ok: inventoryDiscrepancies.length === 0,
      detail:
        inventoryDiscrepancies.length === 0
          ? '全部 SKU 守恒'
          : JSON.stringify(inventoryDiscrepancies.slice(0, 5)),
    },
    {
      name: '渠道库存同步无滞后',
      ok: syncLagging === 0 && deadSync === 0,
      detail: `滞后 SKU ${syncLagging} 个，死信 ${deadSync} 个`,
    },
  ];

  return {
    passed: checks.every((c) => c.ok),
    invariantViolations: violations,
    oversoldQty,
    oversoldEvents: oversoldDetail.length,
    oversoldDetail,
    ledgerDiscrepancies: ledgerDiscrepancies.length,
    ledgerDetail: ledgerDiscrepancies as unknown as Array<Record<string, unknown>>,
    inventoryDiscrepancies: inventoryDiscrepancies.length,
    inventoryDetail: inventoryDiscrepancies as unknown as Array<Record<string, unknown>>,
    syncLagging,
    deadSync,
    checks,
  };
}

export function platformStats(h: Harness) {
  return [...h.platforms.values()].map((p) => p.stats);
}
