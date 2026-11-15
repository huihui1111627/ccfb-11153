import { startHarness, settle, drainSyncFully, acceptance, platformStats } from './harness.js';
import { getOnHand, listSkus } from '../core/catalog.js';

/**
 * 演示脚本：一步步走完“分配 → 预占 → 支付 → 超时释放 → 盘点 → 同步补偿 → 对账”。
 * 每一步都打印账本，便于人工核对。
 */
async function main(): Promise<void> {
  const h = await startHarness({ stockPerSku: 100 });
  const db = h.db;
  const log = (s = '') => console.log(s);
  const hr = (t: string) => log(`\n${'─'.repeat(70)}\n${t}\n${'─'.repeat(70)}`);

  const show = (skuId: string) => {
    const alloc = db
      .prepare(
        `SELECT s.name, a.allocated, a.reserved, a.sold FROM allocations a JOIN shops s ON s.id = a.shop_id
         WHERE a.sku_id = ? ORDER BY s.priority`,
      )
      .all(skuId) as Array<{ name: string; allocated: number; reserved: number; sold: number }>;
    const shared = db.prepare('SELECT reserved, sold FROM shared WHERE sku_id = ?').get(skuId) as
      | { reserved: number; sold: number }
      | undefined;
    const allocated = alloc.reduce((s, a) => s + Number(a.allocated), 0);
    log(
      `  ${skuId}: 实物 ${getOnHand(db, skuId)} ｜ 渠道 ${alloc
        .map((a) => `${a.name} ${a.allocated}(预占${a.reserved})`)
        .join(' / ')} ｜ 共享池 ${getOnHand(db, skuId) - allocated}(预占 ${shared?.reserved ?? 0})`,
    );
  };

  hr('① 初始状态：期初库存 100，按 30% / 30% / 固定 80 / 固定 40 分给 4 个渠道');
  for (const sku of listSkus(db).filter((s) => !s.isBundle)) show(sku.id);

  hr('② 天猫下单 3 件：预占渠道分配（实物不变）');
  const order1 = await h.platforms.get('tmall-flagship')!.placeOrder([{ skuId: 'TEE-BLK-M', qty: 3 }], {
    payProb: 0,
    cancelProb: 0,
  });
  await settle(h);
  show('TEE-BLK-M');
  log(`  订单 ${order1.orderNo} 状态: ${order1.orderId}`);

  hr('③ 用户付款：实物扣减，预占转已售');
  h.platforms.get('tmall-flagship')!.markPaid(order1.orderNo);
  await settle(h);
  show('TEE-BLK-M');

  hr('④ 京东下单后不付款：超过支付窗口自动释放');
  const order2 = await h.platforms.get('jd-self')!.placeOrder([{ skuId: 'TEE-BLK-M', qty: 5 }], {
    payProb: 0,
    cancelProb: 0,
  });
  await settle(h);
  log('  下单后（预占 5 件）：');
  show('TEE-BLK-M');
  const released = h.engine.releaseExpired(0); // 立即视为超时
  await settle(h);
  log(`  自动释放 ${released} 笔超时订单后：`);
  show('TEE-BLK-M');
  void order2;

  hr('⑤ 买套装：可售数按子件最紧的那个算');
  const bundleFree = Number(
    (
      db
        .prepare(
          `SELECT MIN(CAST((COALESCE(st.on_hand,0) - COALESCE((SELECT SUM(r.qty) FROM reservations r WHERE r.sku_id = bc.component_sku_id AND r.state='ACTIVE'),0)) / bc.qty AS INTEGER)) AS n
           FROM bundle_components bc JOIN stock st ON st.sku_id = bc.component_sku_id WHERE bc.bundle_sku_id = 'SET-TEE-SOCK'`,
        )
        .get() as { n: number }
    ).n,
  );
  log(`  套装 SET-TEE-SOCK（1×T恤 + 1×短袜）当前可售: ${bundleFree}`);
  const bundleOrder = await h.platforms.get('douyin-flagship')!.placeOrder(
    [{ skuId: 'SET-TEE-SOCK', qty: 2 }],
    { payProb: 1, payDelayMs: 30, cancelProb: 0 },
  );
  await settle(h);
  log(`  下单 2 套后（订单 ${bundleOrder.orderNo} / ${bundleOrder.status}）：`);
  show('TEE-BLK-M');
  show('SOCK-3PK');

  hr('⑥ 仓库盘点：TEE-BLK-M 实盘比账面少 8 件，按比例摊回各渠道');
  const { applyStocktake } = await import('../core/stocktake.js');
  const before = getOnHand(db, 'TEE-BLK-M');
  const result = applyStocktake(db, h.engine, {
    stocktakeId: 'DEMO-ST-1',
    skuId: 'TEE-BLK-M',
    counted: before - 8,
    rule: 'pro_rata',
    note: '演示抽盘',
  });
  log(`  盘点前 ${result.before} → 实盘 ${result.counted}（差异 ${result.delta}）`);
  log(`  渠道调整: ${result.channelAdjustments.map((c) => `${c.channel} ${c.delta > 0 ? '+' : ''}${c.delta}`).join(', ')}`);
  log(`  共享池承担: ${result.sharedDelta}；因实物不足撤销未付款订单: ${result.releasedOrderIds.length} 笔`);
  show('TEE-BLK-M');

  hr('⑦ 低于安全线：自动收紧小渠道分配');
  for (const sku of ['JKT-DNM-M']) {
    // 通过真实售卖把库存压到安全线以下
    let guard = 0;
    while (Number((db.prepare('SELECT on_hand FROM stock WHERE sku_id = ?').get(sku) as { on_hand: number }).on_hand) > 10 && guard++ < 50) {
      const p = h.platforms.get('tmall-flagship')!;
      const o = await p.placeOrder([{ skuId: sku, qty: 5 }], { payProb: 1, payDelayMs: 5, cancelProb: 0 });
      if (o.status === 'REJECTED') break;
      await settle(h, { maxMs: 2000 });
    }
  }
  const tightened = h.engine.applySafetyTightening();
  log(`  收紧的 SKU: ${tightened.join(', ') || '（无，可能已低于安全线且小渠道无余量）'}`);
  show('JKT-DNM-M');

  hr('⑧ 库存同步：故障注入下推送到 4 个平台，重试但不重复扣');
  await drainSyncFully(h);
  for (const s of platformStats(h)) {
    log(
      `  ${s.shopId.padEnd(18)} 推送 ${String(s.inventoryPushes).padStart(4)} 应用 ${String(s.inventoryApplied).padStart(4)} ` +
        `重复键 ${s.inventoryDuplicateKey} 版本过期 ${s.inventoryStaleIgnored} | webhook ${s.webhookDeliveries} 重复 ${s.webhookDuplicates} 重试 ${s.webhookRetries}`,
    );
  }

  hr('⑨ 每个 SKU 的变动链路（可追到订单/盘点单）');
  const ledger = db
    .prepare(
      `SELECT l.id, l.channel, l.shop_id, l.reason, l.delta_on_hand, l.delta_reserved, l.delta_allocated, l.delta_sold, l.ref_type, l.ref_id
       FROM ledger l WHERE l.sku_id = 'TEE-BLK-M' ORDER BY l.id`,
    )
    .all() as Array<Record<string, unknown>>;
  for (const row of ledger) {
    const parts = [
      row.delta_on_hand ? `实物${Number(row.delta_on_hand) > 0 ? '+' : ''}${row.delta_on_hand}` : '',
      row.delta_reserved ? `预占${Number(row.delta_reserved) > 0 ? '+' : ''}${row.delta_reserved}` : '',
      row.delta_allocated ? `分配${Number(row.delta_allocated) > 0 ? '+' : ''}${row.delta_allocated}` : '',
      row.delta_sold ? `售出${Number(row.delta_sold) > 0 ? '+' : ''}${row.delta_sold}` : '',
    ]
      .filter(Boolean)
      .join(' ');
    log(`  #${String(row.id).padStart(3)} ${String(row.reason).padEnd(10)} ${String(row.channel).padEnd(9)} ${parts.padEnd(40)} ← ${row.ref_type}:${row.ref_id}`);
  }

  hr('⑩ 最终验收：超卖件数 / 库存账实差');
  const report = await acceptance(h);
  for (const c of report.checks) log(`  ${c.ok ? '✅' : '❌'} ${c.name} — ${c.detail}`);
  log(`\n  超卖件数: ${report.oversoldQty}（要求 0）`);
  log(`  库存账实差: 流水不一致 ${report.ledgerDiscrepancies} 处，分配不守恒 ${report.inventoryDiscrepancies} 处（要求 0）`);
  log(`\n  ${report.passed ? '✅ 验收通过' : '❌ 验收未通过'}\n`);

  await h.close();
  process.exit(report.passed ? 0 : 1);
}

main().catch((err) => {
  console.error(err);
  process.exit(1);
});
