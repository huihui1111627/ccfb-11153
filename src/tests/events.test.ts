import { suite, test } from './framework.js';
import { assert, assertInvariants, event, freshDb } from './helpers.js';
import { getOrderView } from '../core/engine.js';
import { getOnHand, skuSellable } from '../core/catalog.js';

const LINES = [{ skuId: 'TEE-BLK-M', qty: 2 }];

// ---------------------------------------------------------------------------
suite('6. 平台消息重复 / 乱序', () => {
  test('重复推送同一事件：第二次判 DUPLICATE，库存只变一次', () => {
    const { db, processor } = freshDb(100);
    const created = event({ externalOrderNo: 'D1', type: 'ORDER_CREATED', lines: LINES });
    const r1 = processor.process(created);
    assert.equal(r1.status, 'APPLIED');
    const r2 = processor.process({ ...created, messageId: 'another-id' });
    assert.equal(r2.status, 'DUPLICATE');
    assert.equal(skuSellable(db, 'TEE-BLK-M'), 98);
    assertInvariants(db);
  });

  test('付款事件重复推送：只扣一次', () => {
    const { db, processor } = freshDb(100);
    processor.process(event({ externalOrderNo: 'D2', type: 'ORDER_CREATED', seq: 1, lines: LINES }));
    const paid = event({ externalOrderNo: 'D2', type: 'ORDER_PAID', seq: 2 });
    assert.equal(processor.process(paid).status, 'APPLIED');
    assert.equal(processor.process({ ...paid, messageId: 'dup-paid' }).status, 'DUPLICATE');
    assert.equal(getOnHand(db, 'TEE-BLK-M'), 98);
    assertInvariants(db);
  });

  test('乱序：付款先到，创建后到，终态与顺序无关', () => {
    const seqA = freshDb(100);
    // 顺序
    seqA.processor.process(event({ externalOrderNo: 'O1', type: 'ORDER_CREATED', seq: 1, lines: LINES }));
    seqA.processor.process(event({ externalOrderNo: 'O1', type: 'ORDER_PAID', seq: 2 }));

    const seqB = freshDb(100);
    // 乱序：付款先到（带行信息，可自愈）
    seqB.processor.process(event({ externalOrderNo: 'O1', type: 'ORDER_PAID', seq: 2, lines: LINES }));
    seqB.processor.process(event({ externalOrderNo: 'O1', type: 'ORDER_CREATED', seq: 1, lines: LINES }));

    for (const h of [seqA, seqB]) {
      const order = h.db.prepare(`SELECT status FROM orders WHERE external_order_no = 'O1'`).get() as { status: string };
      assert.equal(order.status, 'PAID');
      assert.equal(getOnHand(h.db, 'TEE-BLK-M'), 98);
      assertInvariants(h.db);
    }
  });

  test('乱序：取消先到，随后创建事件到达不会复活订单', () => {
    const { db, processor } = freshDb(100);
    // 取消先到（无订单，先落库）
    processor.process(event({ externalOrderNo: 'O2', type: 'ORDER_CANCELLED', seq: 3 }));
    // 创建到达
    const created = processor.process(event({ externalOrderNo: 'O2', type: 'ORDER_CREATED', seq: 1, lines: LINES }));
    assert.ok(['APPLIED', 'DEFERRED'].includes(created.status));
    const order = db.prepare(`SELECT status FROM orders WHERE external_order_no = 'O2'`).get() as { status: string };
    assert.ok(order, '应建单');
    assert.ok(['RESERVED', 'RELEASED'].includes(order.status));
    assert.equal(skuSellable(db, 'TEE-BLK-M'), order.status === 'RESERVED' ? 98 : 100);
    assertInvariants(db);
  });

  test('乱序：创建已付款，取消晚到按退款处理', () => {
    const { db, processor } = freshDb(100);
    processor.process(event({ externalOrderNo: 'O3', type: 'ORDER_CREATED', seq: 1, lines: LINES }));
    processor.process(event({ externalOrderNo: 'O3', type: 'ORDER_PAID', seq: 2 }));
    assert.equal(getOnHand(db, 'TEE-BLK-M'), 98);
    processor.process(event({ externalOrderNo: 'O3', type: 'ORDER_CANCELLED', seq: 3 }));
    const order = db.prepare(`SELECT status FROM orders WHERE external_order_no = 'O3'`).get() as { status: string };
    assert.equal(order.status, 'CANCELLED');
    assert.equal(getOnHand(db, 'TEE-BLK-M'), 100, '退款应回补实物');
    assertInvariants(db);
  });

  test('各种到达顺序组合：同一事件集合必须收敛到同一终态', () => {
    const types = ['ORDER_CREATED', 'ORDER_PAID', 'ORDER_CANCELLED'] as const;
    const permutations: string[][] = [
      ['ORDER_CREATED', 'ORDER_PAID'],
      ['ORDER_PAID', 'ORDER_CREATED'],
      ['ORDER_CREATED', 'ORDER_CANCELLED'],
      ['ORDER_CANCELLED', 'ORDER_CREATED'],
      ['ORDER_CREATED', 'ORDER_PAID', 'ORDER_CANCELLED'],
      ['ORDER_CREATED', 'ORDER_CANCELLED', 'ORDER_PAID'],
      ['ORDER_PAID', 'ORDER_CREATED', 'ORDER_CANCELLED'],
      ['ORDER_PAID', 'ORDER_CANCELLED', 'ORDER_CREATED'],
      ['ORDER_CANCELLED', 'ORDER_PAID', 'ORDER_CREATED'],
      ['ORDER_CANCELLED', 'ORDER_CREATED', 'ORDER_PAID'],
    ];
    // 同一组事件（按 seq/type 排序后相同）→ 终态必须相同
    const groups = new Map<string, { status: string; onHand: number }>();
    for (const order of permutations) {
      const canonical = [...order].sort((a, b) => types.indexOf(a as never) - types.indexOf(b as never)).join(',');
      const external = `P-${canonical.replaceAll('ORDER_', '')}`;
      const { db, processor } = freshDb(100);
      for (const type of order) {
        const seq = type === 'ORDER_CREATED' ? 1 : type === 'ORDER_PAID' ? 2 : 3;
        processor.process(
          event({
            externalOrderNo: external,
            type: type as 'ORDER_CREATED' | 'ORDER_PAID' | 'ORDER_CANCELLED',
            seq,
            lines: type === 'ORDER_CREATED' ? LINES : undefined,
          }),
        );
      }
      const row = db.prepare(`SELECT status FROM orders WHERE external_order_no = ?`).get(external) as { status: string } | undefined;
      assert.ok(row, `订单 ${external} 未建单（事件集 ${order.join('→')}）`);
      const onHand = getOnHand(db, 'TEE-BLK-M');
      const prev = groups.get(canonical);
      if (prev) {
        assert.equal(row.status, prev.status, `事件集 ${canonical} 在不同到达顺序下终态不同: ${prev.status} vs ${row.status}`);
        assert.equal(onHand, prev.onHand, `事件集 ${canonical} 在不同到达顺序下库存不同`);
      } else {
        groups.set(canonical, { status: row.status, onHand });
      }
      assertInvariants(db);
    }
    assert.equal(groups.size, 3);
  });

  test('事件缺少订单行时先 DEFERRED，补齐后自动处理', () => {
    const { db, processor } = freshDb(100);
    const first = processor.process(event({ externalOrderNo: 'O5', type: 'ORDER_PAID', seq: 2 }));
    assert.equal(first.status, 'DEFERRED');
    assert.equal(getOnHand(db, 'TEE-BLK-M'), 100);
    // 创建事件补齐
    const created = processor.process(event({ externalOrderNo: 'O5', type: 'ORDER_CREATED', seq: 1, lines: LINES }));
    assert.equal(created.status, 'APPLIED');
    const order = db.prepare(`SELECT status FROM orders WHERE external_order_no = 'O5'`).get() as { status: string };
    assert.equal(order.status, 'PAID');
    assert.equal(getOnHand(db, 'TEE-BLK-M'), 98);
    assertInvariants(db);
  });
});

// ---------------------------------------------------------------------------
suite('6. 平台消息重复 / 乱序 · 边界', () => {
  test('预占失败的订单收到支付事件：不超卖、不扣库存，产生人工退款告警', () => {
    const { db, processor } = freshDb(5);
    // 只买 5 件，下单 10 件必然预占失败
    const created = processor.process(
      event({ externalOrderNo: 'NOSTOCK-1', type: 'ORDER_CREATED', seq: 1, lines: [{ skuId: 'TEE-BLK-M', qty: 10 }] }),
    );
    assert.ok(['APPLIED', 'DEFERRED'].includes(created.status));
    const order = db.prepare(`SELECT status FROM orders WHERE external_order_no = 'NOSTOCK-1'`).get() as { status: string };
    if (order.status === 'FAILED') {
      const paid = processor.process(event({ externalOrderNo: 'NOSTOCK-1', type: 'ORDER_PAID', seq: 2 }));
      assert.equal(paid.status, 'CONFLICT');
      assert.equal(getOnHand(db, 'TEE-BLK-M'), 5, '不得因超卖订单扣库存');
      const alert = db.prepare(`SELECT * FROM alerts WHERE type = 'PAID_WITHOUT_STOCK'`).get();
      assert.ok(alert, '应产生无货已付告警');
    }
    assertInvariants(db);
  });
});
