import { suite, test } from './framework.js';
import { assert, freshDb } from './helpers.js';
import { SyncWorker, repairLaggingSync, syncQueueSize, syncHealth } from '../core/sync.js';
import { markSkuChanged, listShops } from '../core/catalog.js';
import { run, get, all } from '../core/db.js';
import type { DB } from '../core/db.js';

/** 收集推送调用的假平台 */
function fakePlatform(opts: { failFirst?: number; alwaysFail?: boolean } = {}) {
  const calls: Array<{ shopId: string; skuId: string; version: number; idempotencyKey: string; available: number }> = [];
  const seen = new Set<string>();
  let remainingFails = opts.failFirst ?? 0;
  let applied = 0;
  const push = async (job: {
    shopId: string; skuId: string; version: number; idempotencyKey: string; available: number;
  }) => {
    calls.push({ ...job });
    if (opts.alwaysFail) return { ok: false, retryable: true, error: 'ALWAYS_FAIL' };
    if (remainingFails > 0) {
      remainingFails--;
      return { ok: false, retryable: true, error: 'TRANSIENT' };
    }
    if (seen.has(job.idempotencyKey)) {
      return { ok: true, retryable: false, duplicated: true };
    }
    seen.add(job.idempotencyKey);
    applied++;
    return { ok: true, retryable: false };
  };
  return { push, calls, seen, get applied() { return applied; } };
}

function futureNow() {
  let t = Date.now();
  return {
    now: () => (t += 10_000),
    advance: (ms: number) => {
      t += ms;
    },
  };
}

suite('3. 同步限流/超时补偿，不重复扣', () => {
  test('推送失败后指数退避重试并最终成功', async () => {
    const { db } = freshDb(100);
    const platform = fakePlatform({ failFirst: 3 });
    const clock = futureNow();
    const worker = new SyncWorker(db, platform.push, { ...{ baseBackoffMs: 1, maxBackoffMs: 2, maxAttempts: 10, batchSize: 100, breakerThreshold: 5 }, now: clock.now });
    markSkuChanged(db, 'TEE-BLK-M', 'test');
    let processed = 0;
    for (let i = 0; i < 20; i++) {
      const r = await worker.tick();
      processed += r.processed;
      clock.advance(100);
      if (syncQueueSize(db) === 0) break;
    }
    assert.ok(processed >= 2, `应至少处理 2 次，实际 ${processed}`);
    assert.equal(syncQueueSize(db), 0);
    const state = get<{ synced_version: number }>(db, `SELECT synced_version FROM sync_state WHERE shop_id = 'tmall-flagship' AND sku_id = 'TEE-BLK-M'`);
    assert.ok(Number(state?.synced_version) > 0);
    // 重试期间没有产生任何库存扣减
    const onHand = get<{ on_hand: number }>(db, `SELECT on_hand FROM stock WHERE sku_id = 'TEE-BLK-M'`);
    assert.equal(Number(onHand?.on_hand), 100);
  });

  test('同一版本重复推送被平台识别为重复键，不会二次生效', async () => {
    const { db } = freshDb(100);
    const platform = fakePlatform();
    const clock = futureNow();
    const worker = new SyncWorker(db, platform.push, { baseBackoffMs: 1, maxBackoffMs: 2, maxAttempts: 5, batchSize: 100, breakerThreshold: 5, now: clock.now });
    markSkuChanged(db, 'TEE-BLK-M', 'test');
    await worker.tick();
    const firstKey = platform.calls.find((c) => c.skuId === 'TEE-BLK-M' && c.shopId === 'tmall-flagship')!.idempotencyKey;
    const appliedBefore = platform.applied;
    // 手动把任务改回 PENDING 模拟重复投递
    run(db, `UPDATE sync_jobs SET status = 'PENDING', next_attempt_at = 0 WHERE sku_id = 'TEE-BLK-M' AND shop_id = 'tmall-flagship'`);
    clock.advance(1000);
    await worker.tick();
    const dup = platform.calls.filter((c) => c.idempotencyKey === firstKey);
    assert.ok(dup.length >= 2, '应发生重复推送');
    // 同一幂等键只会生效一次：新增 applied 为 0
    assert.equal(platform.applied, appliedBefore, `重复推送不应再次生效，实际增加 ${platform.applied - appliedBefore}`);
  });

  test('新版本覆盖旧任务，旧版本不会把库存刷回去', async () => {
    const { db } = freshDb(100);
    const platform = fakePlatform({ alwaysFail: false });
    const clock = futureNow();
    const worker = new SyncWorker(db, platform.push, { baseBackoffMs: 1, maxBackoffMs: 2, maxAttempts: 10, batchSize: 100, breakerThreshold: 5, now: clock.now });
    markSkuChanged(db, 'TEE-BLK-M', 'v1');
    // 在第一次 tick 前再产生一个新版本
    markSkuChanged(db, 'TEE-BLK-M', 'v2');
    const rows = all<{ version: number; available: number; status: string }>(db, `SELECT version, available, status FROM sync_jobs WHERE sku_id = 'TEE-BLK-M' AND shop_id = 'tmall-flagship'`);
    assert.equal(rows.length, 1, '同一店铺同一 SKU 只应有一个合并任务');
    await worker.tick();
    const job = get<{ version: number; status: string }>(db, `SELECT version, status FROM sync_jobs WHERE sku_id = 'TEE-BLK-M'`);
    assert.equal(job?.status, 'DONE');
    const tmallCalls = platform.calls.filter((c) => c.shopId === 'tmall-flagship' && c.skuId === 'TEE-BLK-M');
    assert.equal(tmallCalls.at(-1)!.version, Number(job!.version));
  });

  test('持续失败进入死信并告警，不阻塞其他 SKU', async () => {
    const { db } = freshDb(100);
    const platform = fakePlatform({ alwaysFail: true });
    const clock = futureNow();
    const worker = new SyncWorker(db, platform.push, { baseBackoffMs: 1, maxBackoffMs: 2, maxAttempts: 3, batchSize: 100, breakerThreshold: 2, now: clock.now });
    markSkuChanged(db, 'TEE-BLK-M', 'test');
    for (let i = 0; i < 8; i++) {
      await worker.tick();
      clock.advance(10_000);
    }
    const dead = all<{ sku_id: string }>(db, `SELECT sku_id FROM sync_jobs WHERE status = 'DEAD'`);
    assert.ok(dead.length > 0, '应有死信任务');
    const alert = get(db, `SELECT * FROM alerts WHERE type = 'SYNC_DEAD'`);
    assert.ok(alert, '应产生同步死信告警');
    // 死信不影响库存本身
    const onHand = get<{ on_hand: number }>(db, `SELECT on_hand FROM stock WHERE sku_id = 'TEE-BLK-M'`);
    assert.equal(Number(onHand?.on_hand), 100);
  });

  test('对账修复：滞后 SKU 重新入队', async () => {
    const { db } = freshDb(50);
    const platform = fakePlatform();
    const clock = futureNow();
    const worker = new SyncWorker(db, platform.push, { baseBackoffMs: 1, maxBackoffMs: 2, maxAttempts: 5, batchSize: 100, breakerThreshold: 5, now: clock.now });
    markSkuChanged(db, 'TEE-BLK-M', 'test');
    // 丢弃待推送任务，模拟“同步没跑”
    run(db, `DELETE FROM sync_jobs`);
    const before = syncHealth(db).reduce((s, x) => s + x.laggingSkus, 0);
    assert.ok(before > 0, '应存在滞后');
    const repaired = repairLaggingSync(db);
    assert.ok(repaired > 0);
    await worker.drain(50);
    const after = syncHealth(db).reduce((s, x) => s + x.laggingSkus, 0);
    assert.equal(after, 0, '修复后不应有滞后');
  });

  test('限流响应（429）按可重试失败处理', async () => {
    const { db } = freshDb(20);
    let calls = 0;
    const push = async () => {
      calls++;
      if (calls <= 2) return { ok: false, retryable: true, error: 'RATE_LIMITED' };
      return { ok: true, retryable: false };
    };
    const clock = futureNow();
    const worker = new SyncWorker(db, push, { baseBackoffMs: 1, maxBackoffMs: 2, maxAttempts: 5, batchSize: 100, breakerThreshold: 5, now: clock.now });
    markSkuChanged(db, 'TEE-BLK-M', 'test');
    for (let i = 0; i < 10 && syncQueueSize(db) > 0; i++) {
      await worker.tick();
      clock.advance(1000);
    }
    assert.equal(syncQueueSize(db), 0);
    assert.ok(calls >= 3);
  });
});
