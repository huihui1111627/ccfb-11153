import { suite, test } from './framework.js';
import { assert } from './helpers.js';
import { acceptance, runFlashSale, settle, startHarness } from '../sim/harness.js';
import { SEED_SKUS } from '../core/seed.js';

suite('9. 端到端大促（4 平台 + 故障注入）', () => {
  test('小规模大促：超卖 0，账实差 0', async () => {
    const h = await startHarness({
      stockPerSku: 120,
      chaos: {
        timeoutRate: 0.03,
        errorRate: 0.03,
        duplicateRate: 0.2,
        reorderRate: 0.35,
        reorderMaxDelayMs: 250,
        pushFailRate: 0.08,
        lostAckRate: 0.02,
      },
    });
    try {
      let running = true;
      const loop = (async () => {
        while (running) {
          await h.syncWorker.tick();
          await new Promise((r) => setTimeout(r, 10));
        }
      })();
      const sale = await runFlashSale(h, {
        skus: SEED_SKUS.map((s) => s.id),
        bundleSku: 'SET-TEE-SOCK',
        durationMs: 3000,
        concurrency: 4,
        orderIntervalMs: 10,
        targetRps: 60,
        stocktake: true,
        safetyChecks: true,
      });
      running = false;
      await loop;
      await settle(h, { maxMs: 10000 });
      h.engine.releaseExpired(1200);
      await settle(h, { maxMs: 5000 });
      const report = await acceptance(h);
      assert.equal(report.oversoldQty, 0, `超卖: ${JSON.stringify(report.oversoldDetail)}`);
      assert.equal(report.ledgerDiscrepancies, 0, `账实差: ${JSON.stringify(report.ledgerDetail)}`);
      assert.equal(report.inventoryDiscrepancies, 0, `分配不守恒: ${JSON.stringify(report.inventoryDetail)}`);
      assert.ok(sale.attempts > 50, `压测订单量过少: ${sale.attempts}`);
      assert.ok(report.passed);
    } finally {
      await h.close();
    }
  });

  test('高故障率 + 重复乱序推送：结果仍为零', async () => {
    const h = await startHarness({
      stockPerSku: 80,
      chaos: {
        timeoutRate: 0.06,
        errorRate: 0.06,
        busyRate: 0.04,
        duplicateRate: 0.35,
        reorderRate: 0.45,
        reorderMaxDelayMs: 350,
        pushFailRate: 0.15,
        lostAckRate: 0.04,
      },
    });
    try {
      let running = true;
      const loop = (async () => {
        while (running) {
          await h.syncWorker.tick();
          await new Promise((r) => setTimeout(r, 10));
        }
      })();
      await runFlashSale(h, {
        skus: SEED_SKUS.map((s) => s.id),
        bundleSku: 'SET-TEE-SOCK',
        durationMs: 2500,
        concurrency: 5,
        orderIntervalMs: 10,
        targetRps: 50,
        stocktake: true,
        safetyChecks: true,
      });
      running = false;
      await loop;
      await settle(h, { maxMs: 12000 });
      h.engine.releaseExpired(1000);
      await settle(h, { maxMs: 6000 });
      const report = await acceptance(h);
      assert.equal(report.oversoldQty, 0, `超卖: ${JSON.stringify(report.oversoldDetail.slice(0, 3))}`);
      assert.equal(report.ledgerDiscrepancies, 0);
      assert.equal(report.inventoryDiscrepancies, 0);
      // 确认确实注入了重复/乱序
      const dup = h.db.prepare(`SELECT COUNT(*) AS n FROM order_events WHERE result = 'DUPLICATE'`).get() as { n: number };
      assert.ok(Number(dup.n) > 0, '应观察到重复事件被幂等处理');
    } finally {
      await h.close();
    }
  }, );
});
