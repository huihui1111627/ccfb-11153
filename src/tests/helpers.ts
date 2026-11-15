import assert from 'node:assert/strict';
import { type DB, openDb } from '../core/db.js';
import { seed } from '../core/seed.js';
import { InventoryEngine } from '../core/engine.js';
import { OrderEventProcessor } from '../core/events.js';
import { applyStocktake } from '../core/stocktake.js';
import {
  allocationMatrix,
  bundleSellable,
  channelUnreserved,
  desiredAllocation,
  getOnHand,
  listShops,
  markSkuChanged,
  rebalanceAllocation,
  setOnHand,
  sharedPoolQty,
  sharedUnreserved,
  skuSellable,
} from '../core/catalog.js';
import { reconcile } from '../server/hub.js';
import { OrderEventPayload } from '../core/types.js';
import { SyncWorker } from '../core/sync.js';

export function freshDb(stock = 100, opts: { ratioShops?: boolean } = {}): { db: DB; engine: InventoryEngine; processor: OrderEventProcessor } {
  const db = openDb(':memory:');
  seed(db, { stockPerSku: stock });
  if (opts.ratioShops === false) {
    // 全部改固定值分配，便于精确断言
    for (const shop of listShops(db)) {
      db.prepare('UPDATE shops SET alloc_mode = ?, alloc_value = ? WHERE id = ?').run('fixed', 10, shop.id);
    }
    for (const sku of ['TEE-BLK-M', 'TEE-BLK-L', 'TEE-WHT-M', 'JKT-DNM-M', 'JKT-DNM-L', 'SOCK-3PK']) {
      rebalanceAllocation(db, sku, '测试重分配', 'TEST');
    }
  }
  const engine = new InventoryEngine(db);
  const processor = new OrderEventProcessor(db, engine);
  return { db, engine, processor };
}

export function event(
  overrides: Partial<OrderEventPayload> & Pick<OrderEventPayload, 'externalOrderNo' | 'type'>,
): OrderEventPayload {
  return {
    messageId: `${overrides.externalOrderNo}:${overrides.type}:${overrides.seq ?? 1}:${Math.random().toString(36).slice(2, 8)}`,
    channel: 'tmall',
    shopId: 'tmall-flagship',
    seq: overrides.type === 'ORDER_CREATED' ? 1 : overrides.type === 'ORDER_PAID' ? 2 : 3,
    occurredAt: Date.now(),
    ...overrides,
  };
}

/** 测试专用：设置期初库存并补一条期初流水，保持“流水汇总 == 账面”不变量 */
export function openStock(db: DB, skuId: string, qty: number): void {
  setOnHand(db, skuId, qty);
  db.prepare(
    `INSERT INTO ledger(sku_id, channel, shop_id, reason, delta_on_hand, ref_type, ref_id, note, created_at)
     VALUES(?, 'WAREHOUSE', NULL, 'STOCKTAKE', ?, 'STOCKTAKE', ?, '测试期初', ?)`,
  ).run(skuId, qty, `OPEN-${skuId}-${Date.now()}`, Date.now());
}

/**
 * 通过真实下单-支付路径消耗库存（跨渠道拆分，优先渠道分配再借共享池）。
 * 用于构造“库存不足”“子件耗尽”等场景，而不是直接改账面数。
 */
export function consume(db: DB, engine: InventoryEngine, skuId: string, qty: number): void {
  const shops = listShops(db).filter((s) => s.active);
  let remaining = qty;
  let n = 0;
  while (remaining > 0) {
    let progressed = false;
    for (const shop of shops) {
      if (remaining <= 0) break;
      const r = engine.reserve({
        shopId: shop.id,
        channel: shop.channel,
        externalOrderNo: `CONSUME-${skuId}-${n++}`,
        lines: [{ skuId, qty: 1 }],
      });
      if (r.ok && r.status !== 'EXISTING') {
        engine.pay(r.orderId);
        remaining--;
        progressed = true;
      }
    }
    if (!progressed) throw new Error(`无法继续消耗 ${skuId}，剩余 ${remaining}`);
  }
}

/** 校验守恒与流水一致性 */
export function assertInvariants(db: DB): void {
  for (const row of allocationMatrix(db)) {
    const sum = row.channels.reduce((s, c) => s + c.allocated, 0);
    assert.equal(row.onHand, sum + row.sharedQty, `${row.skuId} 守恒失败: onHand=${row.onHand} alloc=${sum} shared=${row.sharedQty}`);
    assert.ok(row.sharedQty >= 0, `${row.skuId} 共享池为负: ${row.sharedQty}`);
    for (const c of row.channels) {
      assert.ok(c.allocated >= c.reserved, `${row.skuId}/${c.shopId} 分配 ${c.allocated} < 预占 ${c.reserved}`);
    }
  }
  for (const sku of db.prepare('SELECT id FROM skus').all() as Array<{ id: string }>) {
    const ledger = db.prepare('SELECT COALESCE(SUM(delta_on_hand),0) AS n FROM ledger WHERE sku_id = ?').get(sku.id) as { n: number };
    assert.equal(Number(ledger.n), getOnHand(db, sku.id), `${sku.id} 流水与账面不一致`);
  }
  const rec = reconcile(db);
  assert.equal(rec.oversoldQty, 0, `存在超卖事件 ${JSON.stringify(rec.oversoldRows)}`);
}

export { assert, getOnHand, channelUnreserved, sharedUnreserved, sharedPoolQty, skuSellable, bundleSellable, desiredAllocation, listShops, markSkuChanged, rebalanceAllocation, applyStocktake, allocationMatrix, setOnHand, SyncWorker };
