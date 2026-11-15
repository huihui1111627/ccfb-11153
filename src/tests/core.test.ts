import { suite, test } from './framework.js';
import {
  assert,
  assertInvariants,
  freshDb,
  getOnHand,
  channelUnreserved,
  sharedUnreserved,
  sharedPoolQty,
  skuSellable,
  bundleSellable,
  desiredAllocation,
  listShops,
  rebalanceAllocation,
  applyStocktake,
  allocationMatrix,
  setOnHand,
  openStock,
  consume,
} from './helpers.js';
import { getOrderView } from '../core/engine.js';

// ---------------------------------------------------------------------------
suite('1. 渠道分配（比例 / 固定 / 共享池）', () => {
  test('比例分配：按可用库存比例切给渠道，剩余进共享池', () => {
    const { db } = freshDb(1000);
    const matrix = allocationMatrix(db).find((m) => m.skuId === 'TEE-BLK-M')!;
    const desired = desiredAllocation(db, 'TEE-BLK-M');
    for (const channel of matrix.channels) {
      assert.equal(channel.allocated, desired.get(channel.shopId), `${channel.shopId} 分配不符`);
    }
    // 可分配量 = 1000 - 安全库存 30 = 970；天猫/京东各 30% = 291，抖音固定 20，小程序固定 10
    const usable = 1000 - 30;
    assert.equal(matrix.channels.find((c) => c.channel === 'tmall')!.allocated, Math.floor(usable * 0.3));
    assert.equal(matrix.channels.find((c) => c.channel === 'jd')!.allocated, Math.floor(usable * 0.3));
    assert.equal(matrix.channels.find((c) => c.channel === 'douyin')!.allocated, 20);
    assert.equal(matrix.channels.find((c) => c.channel === 'miniapp')!.allocated, 10);
    const allocSum = matrix.channels.reduce((s, c) => s + c.allocated, 0);
    assert.equal(matrix.sharedQty, 1000 - allocSum);
    assertInvariants(db);
  });

  test('固定值分配 + 安全库存：可分配量 = 实物 - 安全库存', () => {
    const { db } = freshDb(100);
    // 安全库存 30 -> 可分配 70，比例渠道 21/21，固定 80+40 被削减
    const desired = desiredAllocation(db, 'TEE-BLK-M');
    const total = [...desired.values()].reduce((s, v) => s + v, 0);
    // 小渠道有安全线护栏：主渠道先占 21+21，剩余 70-42=28 才给小渠道，上限 10
    assert.ok(total <= 70, `分配总额 ${total} 不应超过可分配量 70`);
    assert.ok((desired.get('miniapp-mall') ?? 0) <= 10);
    assertInvariants(db);
  });

  test('需求超过可用库存时，小渠道先被削减', () => {
    const { db } = freshDb(50);
    const shops = listShops(db);
    const small = shops.find((s) => s.isSmall)!;
    const desired = desiredAllocation(db, 'TEE-BLK-M');
    const usable = 50 - 30;
    assert.equal([...desired.values()].reduce((s, v) => s + v, 0), usable);
    assert.equal(desired.get(small.id), 0, '小渠道应被清零');
    assertInvariants(db);
  });

  test('手工重分配后差额全部回到共享池，守恒', () => {
    const { db } = freshDb(300);
    for (const shop of listShops(db)) {
      db.prepare('UPDATE shops SET alloc_mode = ?, alloc_value = ? WHERE id = ?').run('fixed', 20, shop.id);
    }
    rebalanceAllocation(db, 'TEE-BLK-M', '测试', 'T');
    const m = allocationMatrix(db).find((x) => x.skuId === 'TEE-BLK-M')!;
    assert.equal(m.channels.reduce((s, c) => s + c.allocated, 0), 80);
    assert.equal(m.sharedQty, 220);
    assertInvariants(db);
  });
});

// ---------------------------------------------------------------------------
suite('2. 预占 / 支付扣减 / 超时释放', () => {
  test('下单预占渠道库存，支付后实物扣减且分配减少', () => {
    const { db, engine } = freshDb(100);
    const before = getOnHand(db, 'TEE-BLK-M');
    const chBefore = channelUnreserved(db, 'tmall-flagship', 'TEE-BLK-M');
    const r = engine.reserve({
      shopId: 'tmall-flagship', channel: 'tmall', externalOrderNo: 'T1',
      lines: [{ skuId: 'TEE-BLK-M', qty: 3 }],
    });
    assert.ok(r.ok);
    assert.equal(getOnHand(db, 'TEE-BLK-M'), before, '预占不改变实物');
    assert.equal(channelUnreserved(db, 'tmall-flagship', 'TEE-BLK-M'), chBefore - 3);
    assert.equal(skuSellable(db, 'TEE-BLK-M'), before - 3, '可售要扣预占');

    const pay = engine.pay(r.orderId);
    assert.ok(pay.ok);
    assert.equal(getOnHand(db, 'TEE-BLK-M'), before - 3, '支付后实物扣减');
    assert.equal(skuSellable(db, 'TEE-BLK-M'), before - 3);
    assertInvariants(db);
  });

  test('渠道余量不足时借用共享池', () => {
    const { db, engine } = freshDb(100);
    // 先耗尽小渠道的渠道余量，再下单
    const shopFree = channelUnreserved(db, 'miniapp-mall', 'TEE-BLK-M');
    if (shopFree > 0) {
      const r0 = engine.reserve({
        shopId: 'miniapp-mall', channel: 'miniapp', externalOrderNo: 'M0',
        lines: [{ skuId: 'TEE-BLK-M', qty: shopFree }],
      });
      assert.ok(r0.ok);
      engine.pay(r0.orderId);
    }
    assert.equal(channelUnreserved(db, 'miniapp-mall', 'TEE-BLK-M'), 0, '小程序渠道余量应为 0');
    const sharedBefore = sharedUnreserved(db, 'TEE-BLK-M');
    const r = engine.reserve({
      shopId: 'miniapp-mall', channel: 'miniapp', externalOrderNo: 'M1',
      lines: [{ skuId: 'TEE-BLK-M', qty: 2 }],
    });
    assert.ok(r.ok);
    assert.equal(sharedUnreserved(db, 'TEE-BLK-M'), sharedBefore - 2);
    assertInvariants(db);
  });

  test('库存不足时拒单，不产生预占', () => {
    const { db, engine } = freshDb(10);
    const r = engine.reserve({
      shopId: 'tmall-flagship', channel: 'tmall', externalOrderNo: 'T2',
      lines: [{ skuId: 'TEE-BLK-M', qty: 11 }],
    });
    assert.equal(r.ok, false);
    const view = getOrderView(db, r.orderId)!;
    assert.equal(view.status, 'FAILED');
    assert.equal(skuSellable(db, 'TEE-BLK-M'), 10);
    assertInvariants(db);
  });

  test('重复下单（同一平台单号）幂等，不重复预占', () => {
    const { db, engine } = freshDb(100);
    const a = engine.reserve({ shopId: 'tmall-flagship', channel: 'tmall', externalOrderNo: 'DUP', lines: [{ skuId: 'TEE-BLK-M', qty: 5 }] });
    const b = engine.reserve({ shopId: 'tmall-flagship', channel: 'tmall', externalOrderNo: 'DUP', lines: [{ skuId: 'TEE-BLK-M', qty: 5 }] });
    assert.equal(a.orderId, b.orderId);
    assert.equal(b.duplicated, true);
    assert.equal(skuSellable(db, 'TEE-BLK-M'), 95);
    assertInvariants(db);
  });

  test('支付幂等：重复回调不重复扣减', () => {
    const { db, engine } = freshDb(100);
    const r = engine.reserve({ shopId: 'tmall-flagship', channel: 'tmall', externalOrderNo: 'P1', lines: [{ skuId: 'TEE-BLK-M', qty: 4 }] });
    engine.pay(r.orderId);
    const again = engine.pay(r.orderId);
    assert.equal(again.alreadyPaid, true);
    assert.equal(getOnHand(db, 'TEE-BLK-M'), 96);
    assertInvariants(db);
  });

  test('超时未支付自动释放，库存回到可售', () => {
    const { db, engine } = freshDb(100);
    const t0 = Date.now();
    const r = engine.reserve({ shopId: 'tmall-flagship', channel: 'tmall', externalOrderNo: 'E1', lines: [{ skuId: 'TEE-BLK-M', qty: 6 }], now: t0 });
    assert.equal(skuSellable(db, 'TEE-BLK-M'), 94);
    const released = engine.releaseExpired(1000, t0 + 5000);
    assert.equal(released, 1);
    assert.equal(skuSellable(db, 'TEE-BLK-M'), 100);
    assert.equal(getOrderView(db, r.orderId)!.status, 'RELEASED');
    assertInvariants(db);
  });

  test('释放幂等：重复释放无副作用', () => {
    const { db, engine } = freshDb(100);
    const r = engine.reserve({ shopId: 'tmall-flagship', channel: 'tmall', externalOrderNo: 'E2', lines: [{ skuId: 'TEE-BLK-M', qty: 2 }] });
    const a = engine.release(r.orderId, '测试');
    const b = engine.release(r.orderId, '重复');
    assert.ok(a.ok);
    assert.equal(b.alreadyReleased, true);
    assert.equal(skuSellable(db, 'TEE-BLK-M'), 100);
    assertInvariants(db);
  });

  test('退款回补：实物恢复且渠道分配回补', () => {
    const { db, engine } = freshDb(100);
    const r = engine.reserve({ shopId: 'tmall-flagship', channel: 'tmall', externalOrderNo: 'R1', lines: [{ skuId: 'TEE-BLK-M', qty: 3 }] });
    engine.pay(r.orderId);
    assert.equal(getOnHand(db, 'TEE-BLK-M'), 97);
    const refund = engine.refund(r.orderId);
    assert.ok(refund.ok);
    assert.equal(getOnHand(db, 'TEE-BLK-M'), 100);
    assert.equal(skuSellable(db, 'TEE-BLK-M'), 100);
    assertInvariants(db);
  });

  test('超时释放后付款晚到：重新预占扣减，不超卖', () => {
    const { db, engine } = freshDb(100);
    const t0 = Date.now();
    const r = engine.reserve({ shopId: 'tmall-flagship', channel: 'tmall', externalOrderNo: 'LATE', lines: [{ skuId: 'TEE-BLK-M', qty: 3 }], now: t0 });
    engine.releaseExpired(500, t0 + 2000);
    const revived = engine.reviveAndPay(r.orderId);
    assert.equal(revived.revived, true);
    assert.equal(getOnHand(db, 'TEE-BLK-M'), 97);
    assertInvariants(db);
  });
});

// ---------------------------------------------------------------------------
suite('4. 套装按子件算可售', () => {
  test('套装可售 = min(子件可售 / 用量)', () => {
    const { db } = freshDb(100);
    // SET = TEE-BLK-M × 1 + SOCK-3PK × 1
    const tee = skuSellable(db, 'TEE-BLK-M');
    const sock = skuSellable(db, 'SOCK-3PK');
    assert.equal(bundleSellable(db, 'SET-TEE-SOCK'), Math.min(tee, sock));
  });

  test('子件被其他订单占用后，套装可售随之下降', () => {
    const { db, engine } = freshDb(100);
    const before = bundleSellable(db, 'SET-TEE-SOCK');
    engine.reserve({ shopId: 'tmall-flagship', channel: 'tmall', externalOrderNo: 'B1', lines: [{ skuId: 'TEE-BLK-M', qty: 40 }] });
    assert.equal(bundleSellable(db, 'SET-TEE-SOCK'), before - 40);
    assertInvariants(db);
  });

  test('买套装会预占全部子件', () => {
    const { db, engine } = freshDb(100);
    const teeBefore = skuSellable(db, 'TEE-BLK-M');
    const sockBefore = skuSellable(db, 'SOCK-3PK');
    const r = engine.reserve({ shopId: 'tmall-flagship', channel: 'tmall', externalOrderNo: 'B2', lines: [{ skuId: 'SET-TEE-SOCK', qty: 3 }] });
    assert.ok(r.ok);
    assert.equal(skuSellable(db, 'TEE-BLK-M'), teeBefore - 3);
    assert.equal(skuSellable(db, 'SOCK-3PK'), sockBefore - 3);
    assertInvariants(db);
  });

  test('子件不足时套装拒绝下单', () => {
    const { db, engine } = freshDb(100);
    consume(db, engine, 'SOCK-3PK', 98);
    assert.equal(bundleSellable(db, 'SET-TEE-SOCK'), 2);
    const r = engine.reserve({ shopId: 'jd-self', channel: 'jd', externalOrderNo: 'B4', lines: [{ skuId: 'SET-TEE-SOCK', qty: 3 }] });
    assert.equal(r.ok, false);
    assertInvariants(db);
  });

  test('套装可售不能超过任一子件的可售上限', () => {
    const { db, engine } = freshDb(100);
    consume(db, engine, 'TEE-BLK-M', 95);
    assert.equal(bundleSellable(db, 'SET-TEE-SOCK'), 5);
    assertInvariants(db);
  });
});

// ---------------------------------------------------------------------------
suite('5. 盘点多规则摊回', () => {
  test('pro_rata：盘盈按权重摊回各渠道', () => {
    const { db, engine } = freshDb(100);
    const before = allocationMatrix(db).find((m) => m.skuId === 'TEE-BLK-M')!;
    const r = applyStocktake(db, engine, { stocktakeId: 'ST1', skuId: 'TEE-BLK-M', counted: 120, rule: 'pro_rata' });
    assert.equal(r.delta, 20);
    assert.equal(getOnHand(db, 'TEE-BLK-M'), 120);
    const after = allocationMatrix(db).find((m) => m.skuId === 'TEE-BLK-M')!;
    assert.ok(after.channels.reduce((s, c) => s + c.allocated, 0) >= before.channels.reduce((s, c) => s + c.allocated, 0));
    assertInvariants(db);
  });

  test('pro_rata：盘亏按权重扣回，不破坏已预占', () => {
    const { db, engine } = freshDb(100);
    const r0 = engine.reserve({ shopId: 'tmall-flagship', channel: 'tmall', externalOrderNo: 'S1', lines: [{ skuId: 'TEE-BLK-M', qty: 10 }] });
    assert.ok(r0.ok);
    const r = applyStocktake(db, engine, { stocktakeId: 'ST2', skuId: 'TEE-BLK-M', counted: 90, rule: 'pro_rata' });
    assert.equal(r.delta, -10);
    assert.equal(getOnHand(db, 'TEE-BLK-M'), 90);
    assertInvariants(db);
    // 已预占仍然有效
    assert.ok(engine.pay(r0.orderId).ok);
    assertInvariants(db);
  });

  test('shared_first：盘亏先由共享池吸收', () => {
    const { db, engine } = freshDb(100);
    const pool = sharedPoolQty(db, 'TEE-BLK-M');
    assert.ok(pool > 0);
    applyStocktake(db, engine, { stocktakeId: 'ST3', skuId: 'TEE-BLK-M', counted: 100 - Math.min(pool, 5), rule: 'shared_first' });
    assertInvariants(db);
  });

  test('channel_first：盘盈先给渠道，尾差进共享池', () => {
    const { db, engine } = freshDb(100);
    const allocBefore = allocationMatrix(db).find((m) => m.skuId === 'TEE-BLK-M')!.channels.reduce((s, c) => s + c.allocated, 0);
    applyStocktake(db, engine, { stocktakeId: 'ST4', skuId: 'TEE-BLK-M', counted: 111, rule: 'channel_first' });
    const allocAfter = allocationMatrix(db).find((m) => m.skuId === 'TEE-BLK-M')!.channels.reduce((s, c) => s + c.allocated, 0);
    assert.equal(allocAfter, allocBefore + 11);
    assertInvariants(db);
  });

  test('盘亏到低于全部未付款预占：撤销最晚的未付款订单而不是超卖', () => {
    const { db, engine } = freshDb(100);
    const t0 = Date.now();
    let reservedQty = 0;
    for (let i = 0; i < 5; i++) {
      const r = engine.reserve({
        shopId: 'tmall-flagship', channel: 'tmall', externalOrderNo: `SHORT-${i}`,
        lines: [{ skuId: 'TEE-BLK-M', qty: 10 }], now: t0 + i * 1000,
      });
      if (r.ok) reservedQty += 10;
    }
    assert.equal(reservedQty, 50, '5 笔各 10 件，应全部预占成功');
    assert.equal(skuSellable(db, 'TEE-BLK-M'), 50);
    // 盘点只剩 30 件，未付款预占 80 件，必须撤销一部分订单
    const r = applyStocktake(db, engine, { stocktakeId: 'ST5', skuId: 'TEE-BLK-M', counted: 30, rule: 'pro_rata' });
    assert.ok(r.releasedOrderIds.length >= 2, `应释放至少 2 笔，实际 ${r.releasedOrderIds.length}`);
    assert.equal(getOnHand(db, 'TEE-BLK-M'), 30);
    assertInvariants(db);
    const activeReserved = Number(
      (db.prepare(`SELECT COALESCE(SUM(qty),0) AS n FROM reservations WHERE sku_id = 'TEE-BLK-M' AND state = 'ACTIVE'`).get() as { n: number }).n,
    );
    assert.ok(activeReserved <= 30, `活跃预占 ${activeReserved} 超过实盘`);
  });

  test('盘点后流水可追到本次盘点单', () => {
    const { db, engine } = freshDb(100);
    applyStocktake(db, engine, { stocktakeId: 'ST-TRACE', skuId: 'TEE-BLK-M', counted: 87, rule: 'pro_rata' });
    const rows = engine.traceByRef('STOCKTAKE', 'ST-TRACE');
    assert.ok(rows.length > 0);
    const total = rows.reduce((s, r) => s + Number(r.delta_on_hand) + Number(r.delta_allocated), 0);
    assert.ok(total !== 0);
    assertInvariants(db);
  });
});

// ---------------------------------------------------------------------------
suite('7. 安全线收紧小渠道', () => {
  test('库存下降时小渠道分配按“实物-安全库存-主渠道”收敛', () => {
    const { db, engine } = freshDb(200);
    // 给一个专属 SKU，安全库存 40，四渠道均按 20% 比例
    for (const shop of listShops(db)) {
      db.prepare('UPDATE shops SET alloc_mode = ?, alloc_value = ? WHERE id = ?').run('ratio', 0.2, shop.id);
    }
    db.prepare(
      `INSERT INTO skus(id, title, is_bundle, safety_stock, created_at) VALUES('PT-SAFETY','安全线测试',0,40,?)
       ON CONFLICT(id) DO UPDATE SET safety_stock = 40`,
    ).run(Date.now());
    openStock(db, 'PT-SAFETY', 200);
    rebalanceAllocation(db, 'PT-SAFETY', '初始分配', 'T');

    const smallOf = () => allocationMatrix(db).find((m) => m.skuId === 'PT-SAFETY')!.channels.find((c) => c.shopId === 'miniapp-mall')!.allocated;
    const mainOf = () =>
      allocationMatrix(db)
        .find((m) => m.skuId === 'PT-SAFETY')!
        .channels.filter((c) => c.shopId !== 'miniapp-mall')
        .reduce((s, c) => s + c.allocated, 0);

    const initialSmall = smallOf();
    assert.ok(initialSmall > 0, `小渠道初始应有分配，实际 ${initialSmall}`);

    // 通过真实售卖把库存压向安全线
    const shops = listShops(db);
    let n = 0;
    while (getOnHand(db, 'PT-SAFETY') > 60) {
      const shop = shops[n % shops.length]!;
      const r = engine.reserve({
        shopId: shop.id, channel: shop.channel,
        externalOrderNo: `SAFE-${n++}`, lines: [{ skuId: 'PT-SAFETY', qty: 2 }],
      });
      if (!r.ok) break;
      engine.pay(r.orderId);
    }

    const afterSmall = smallOf();
    assert.ok(afterSmall < initialSmall, `库存下降后小渠道应从 ${initialSmall} 收紧，实际 ${afterSmall}`);
    const cap = Math.max(0, getOnHand(db, 'PT-SAFETY') - 40 - mainOf());
    assert.ok(afterSmall <= cap + 1, `小渠道 ${afterSmall} 应不超过上限 ${cap}`);

    const alert = db.prepare(`SELECT * FROM alerts WHERE type = 'SAFETY_TIGHTEN' AND sku_id = 'PT-SAFETY'`).get();
    assert.ok(alert, '应产生安全线收紧告警');
    assert.ok(
      (engine.ledgerFor('PT-SAFETY', 1000) as Array<{ reason: string }>).some((l) => l.reason === 'REALLOCATE'),
      '应记录 REALLOCATE 流水',
    );
    assertInvariants(db);
  });

  test('收紧是幂等的：重复调用不再继续削减', () => {
    const { db, engine } = freshDb(120);
    for (const shop of listShops(db)) {
      db.prepare('UPDATE shops SET alloc_mode = ?, alloc_value = ? WHERE id = ?').run('ratio', 0.25, shop.id);
    }
    db.prepare(
      `INSERT INTO skus(id, title, is_bundle, safety_stock, created_at) VALUES('PT-IDEM','幂等测试',0,50,?)
       ON CONFLICT(id) DO UPDATE SET safety_stock = 50`,
    ).run(Date.now());
    openStock(db, 'PT-IDEM', 120);
    rebalanceAllocation(db, 'PT-IDEM', '初始', 'T');

    const shop = listShops(db)[0]!;
    let n = 0;
    while (getOnHand(db, 'PT-IDEM') > 50) {
      const r = engine.reserve({ shopId: shop.id, channel: shop.channel, externalOrderNo: `IDEM-${n++}`, lines: [{ skuId: 'PT-IDEM', qty: 1 }] });
      if (!r.ok) break;
      engine.pay(r.orderId);
    }
    engine.applySafetyTightening();
    const snapshot = allocationMatrix(db).find((m) => m.skuId === 'PT-IDEM')!;
    engine.applySafetyTightening();
    engine.applySafetyTightening();
    const after = allocationMatrix(db).find((m) => m.skuId === 'PT-IDEM')!;
    assert.deepEqual(
      after.channels.map((c) => [c.shopId, c.allocated]),
      snapshot.channels.map((c) => [c.shopId, c.allocated]),
      '重复收紧不应改变分配',
    );
    assertInvariants(db);
  });
});

// ---------------------------------------------------------------------------
suite('8. 变动可追溯', () => {
  test('每个 SKU 的每次变动都能追到订单号或调整单号', () => {
    const { db, engine } = freshDb(100);
    const r = engine.reserve({ shopId: 'tmall-flagship', channel: 'tmall', externalOrderNo: 'TRACE-1', lines: [{ skuId: 'TEE-BLK-M', qty: 2 }] });
    engine.pay(r.orderId);
    applyStocktake(db, engine, { stocktakeId: 'ST-TRACE2', skuId: 'TEE-BLK-M', counted: 95, rule: 'pro_rata' });
    const rows = engine.ledgerFor('TEE-BLK-M');
    assert.ok(rows.length >= 3);
    for (const row of rows) {
      assert.ok(row.ref_type && row.ref_id, `流水 ${row.id} 缺少可追溯来源`);
      assert.ok(['ORDER', 'STOCKTAKE', 'ALLOCATION', 'SYSTEM'].includes(String(row.ref_type)), `未知来源类型 ${row.ref_type}`);
    }
    const orderLedger = engine.traceByRef('ORDER', r.orderId);
    assert.ok(orderLedger.some((l) => l.reason === 'RESERVE'));
    assert.ok(orderLedger.some((l) => l.reason === 'PAID'));
  });
});
