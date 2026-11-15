import { type DB, all, get, run } from './db.js';
import { getSetting, setSetting } from './db.js';
import { listShops } from './catalog.js';
import { raiseAlert } from './alerts.js';

export interface SyncPushResult {
  ok: boolean;
  retryable: boolean;
  duplicated?: boolean;
  stale?: boolean;
  error?: string;
}

export type PushFn = (job: {
  channel: string;
  shopId: string;
  skuId: string;
  version: number;
  available: number;
  idempotencyKey: string;
}) => Promise<SyncPushResult>;

export interface SyncWorkerOptions {
  baseBackoffMs: number;
  maxBackoffMs: number;
  maxAttempts: number;
  batchSize: number;
  breakerThreshold: number;
  now?: () => number;
}

export const DEFAULT_SYNC_OPTIONS: SyncWorkerOptions = {
  baseBackoffMs: 20,
  maxBackoffMs: 1500,
  maxAttempts: 10,
  batchSize: 64,
  breakerThreshold: 6,
};

/**
 * 库存同步补偿器。
 *
 * 约束：“失败能补偿，但不重复扣”。
 * - 库存扣减只发生在预占/支付那个事务里，同步只是把“当前可用数”推给平台；
 *   重试推送不会再次扣减，因此天然不重复扣。
 * - 任务按 (shopId, skuId) 合并，新版本覆盖旧任务；平台按 version 幂等去重，
 *   重复推送返回 duplicated，旧版本返回 stale（平台自己保留新值）。
 * - 失败指数退避 + 抖动；超过上限进 DEAD 并告警，可通过 /sync/repair 重新入队。
 * - 平台上架/恢复后必然存在短暂滞后，checkBreaker 会统计滞后 SKU 数并告警。
 */
export class SyncWorker {
  constructor(
    private readonly db: DB,
    private readonly push: PushFn,
    private readonly opts: SyncWorkerOptions = DEFAULT_SYNC_OPTIONS,
  ) {}

  private now(): number {
    return this.opts.now ? this.opts.now() : Date.now();
  }

  async tick(): Promise<{ processed: number; succeeded: number; failed: number; dead: number }> {
    const db = this.db;
    const now = this.now();
    const jobs = all<{
      id: number; channel: string; shop_id: string; sku_id: string; version: number;
      available: number; attempts: number;
    }>(
      db,
      `SELECT id, channel, shop_id, sku_id, version, available, attempts FROM sync_jobs
       WHERE status IN ('PENDING','RETRYING') AND next_attempt_at <= ?
       ORDER BY id LIMIT ?`,
      now,
      this.opts.batchSize,
    );

    let succeeded = 0;
    let failed = 0;
    let dead = 0;

    for (const job of jobs) {
      const key = `${job.shop_id}:${job.sku_id}:${job.version}`;
      let result: SyncPushResult;
      try {
        result = await this.push({
          channel: job.channel,
          shopId: job.shop_id,
          skuId: job.sku_id,
          version: job.version,
          available: job.available,
          idempotencyKey: key,
        });
      } catch (err) {
        result = { ok: false, retryable: true, error: String((err as Error).message ?? err) };
      }

      const at = this.now();
      // 推送期间可能有更新版本写入同一任务行（按 shopId+skuId 合并）。只有行内版本仍是
      // 本次推送的版本时才能标记完成，否则保留 PENDING 让下一轮推送新值，避免丢失更新。
      const currentVersion = Number(
        get<{ version: number }>(db, 'SELECT version FROM sync_jobs WHERE id = ?', job.id)?.version ?? job.version,
      );
      const stillSameVersion = currentVersion === Number(job.version);
      if (result.ok) {
        succeeded++;
        run(
          db,
          `UPDATE sync_jobs SET status = CASE WHEN version = ? THEN 'DONE' ELSE 'PENDING' END,
             updated_at = ?, last_error = NULL WHERE id = ?`,
          job.version, at, job.id,
        );
        run(
          db,
          `INSERT INTO sync_state(shop_id, sku_id, last_version, last_available, synced_version, synced_available, last_ok_at, updated_at)
           VALUES(?,?,?,?,?,?,?,?)
           ON CONFLICT(shop_id, sku_id) DO UPDATE SET
             last_version = MAX(sync_state.last_version, excluded.last_version),
             last_available = CASE WHEN excluded.last_version >= sync_state.last_version THEN excluded.last_available ELSE sync_state.last_available END,
             synced_version = CASE WHEN excluded.synced_version >= COALESCE(sync_state.synced_version,0) THEN excluded.synced_version ELSE sync_state.synced_version END,
             synced_available = CASE WHEN excluded.synced_version >= COALESCE(sync_state.synced_version,0) THEN excluded.synced_available ELSE sync_state.synced_available END,
             last_ok_at = excluded.last_ok_at, updated_at = excluded.updated_at`,
          job.shop_id, job.sku_id, job.version, job.available, job.version, job.available, at, at,
        );
        run(
          db,
          'INSERT INTO sync_log(channel, shop_id, sku_id, idempotency_key, version, available, status, detail, at) VALUES(?,?,?,?,?,?,?,?,?)',
          job.channel, job.shop_id, job.sku_id, key, job.version, job.available, 'OK',
          result.duplicated ? 'platform duplicate-key' : result.stale ? 'platform kept newer' : '', at,
        );
        run(
          db,
          `INSERT INTO platform_health(shop_id, ok, last_ok_at) VALUES(?,1,?)
           ON CONFLICT(shop_id) DO UPDATE SET ok = 1, last_ok_at = excluded.last_ok_at`,
          job.shop_id, at,
        );
        continue;
      }

      failed++;
      const attempts = Number(job.attempts) + 1;
      const isDead = !result.retryable || attempts >= this.opts.maxAttempts;
      if (isDead) dead++;
      const backoff = Math.min(this.opts.maxBackoffMs, this.opts.baseBackoffMs * 2 ** Math.min(attempts, 10));
      const jitter = Math.floor(backoff * 0.25 * Math.random());
      run(
        db,
        'UPDATE sync_jobs SET status = ?, attempts = ?, next_attempt_at = ?, updated_at = ?, last_error = ? WHERE id = ?',
        isDead ? 'DEAD' : 'RETRYING', attempts, this.now() + backoff + jitter, this.now(), result.error ?? 'unknown', job.id,
      );
      run(
        db,
        'INSERT INTO sync_log(channel, shop_id, sku_id, idempotency_key, version, available, status, detail, at) VALUES(?,?,?,?,?,?,?,?,?)',
        job.channel, job.shop_id, job.sku_id, key, job.version, job.available,
        isDead ? 'DEAD' : 'RETRY', result.error ?? '', this.now(),
      );
      run(
        db,
        `INSERT INTO platform_health(shop_id, ok, last_error, last_fail_at) VALUES(?,0,?,?)
         ON CONFLICT(shop_id) DO UPDATE SET ok = 0, last_error = excluded.last_error, last_fail_at = excluded.last_fail_at`,
        job.shop_id, result.error ?? 'unknown', this.now(),
      );
      if (isDead) {
        raiseAlert(db, {
          type: 'SYNC_DEAD', skuId: job.sku_id, shopId: job.shop_id, severity: 'CRITICAL',
          message: `渠道 ${job.shop_id} SKU ${job.sku_id} 版本 ${job.version} 同步 ${attempts} 次仍失败：${result.error ?? ''}`,
          dedupeKey: `syncdead:${job.shop_id}:${job.sku_id}`,
        });
      }
    }

    if (jobs.length > 0) setSetting(db, 'sync_last_tick', String(this.now()));
    if (dead > 0) this.checkBreaker();
    return { processed: jobs.length, succeeded, failed, dead };
  }

  /**
   * 跑空同步队列。必须在所有库存变更停止后调用：
   * 先处理到期任务，再把仍在退避中的任务强制到期，直到队列为空。
   * 保留 DEAD 任务原状（需要人工/对账修复），但默认会把它们放回队列重试一次。
   */
  async drain(
    maxSteps = 400,
    opts: { reviveDead?: boolean; forceDue?: boolean } = { reviveDead: true, forceDue: true },
  ): Promise<{ steps: number; processed: number; succeeded: number; failed: number; dead: number }> {
    const db = this.db;
    if (opts.reviveDead) {
      run(db, `UPDATE sync_jobs SET status = 'PENDING', attempts = 0, next_attempt_at = 0 WHERE status = 'DEAD'`);
    }
    let steps = 0;
    const total = { steps: 0, processed: 0, succeeded: 0, failed: 0, dead: 0 };
    let idleRounds = 0;
    while (steps < maxSteps) {
      const r = await this.tick();
      steps++;
      total.steps = steps;
      total.processed += r.processed;
      total.succeeded += r.succeeded;
      total.failed += r.failed;
      total.dead += r.dead;
      if (syncQueueSize(db) === 0) break;
      // 还有任务但都卡在退避里（processed=0）时，拉平到当前时间继续跑
      if (r.processed === 0) {
        idleRounds++;
        if (opts.forceDue) {
          run(db, `UPDATE sync_jobs SET next_attempt_at = ? WHERE status IN ('PENDING','RETRYING')`, this.now());
        } else if (idleRounds > 5) {
          break;
        }
      } else {
        idleRounds = 0;
      }
      await new Promise((resolve) => setTimeout(resolve, 5));
    }
    return total;
  }

  private checkBreaker(): void {
    const db = this.db;
    for (const shop of listShops(db).filter((s) => s.active)) {
      const stats = get<{ dead: number }>(
        db,
        `SELECT COALESCE(SUM(CASE WHEN status = 'DEAD' THEN 1 ELSE 0 END),0) AS dead FROM sync_jobs WHERE shop_id = ?`,
        shop.id,
      );
      if (stats && Number(stats.dead) >= this.opts.breakerThreshold) {
        raiseAlert(db, {
          type: 'SYNC_BREAKER', shopId: shop.id, severity: 'CRITICAL',
          message: `渠道 ${shop.name} 有 ${stats.dead} 个同步任务进入死信，渠道库存可能滞后`,
          dedupeKey: `breaker:${shop.id}`,
        });
      }
    }
  }
}

/** 对账：本地最新版本 > 平台已确认版本 的 SKU，重新入队补偿 */
export function repairLaggingSync(db: DB): number {
  const rows = all<{
    shop_id: string; sku_id: string; last_version: number; last_available: number;
    synced_version: number; channel: string;
  }>(
    db,
    `SELECT st.shop_id, st.sku_id, st.last_version, st.last_available,
            COALESCE(st.synced_version,0) AS synced_version, s.channel
     FROM sync_state st JOIN shops s ON s.id = st.shop_id
     WHERE st.last_version > COALESCE(st.synced_version, 0)`,
  );
  let repaired = 0;
  const now = Date.now();
  for (const r of rows) {
    run(
      db,
      `INSERT INTO sync_jobs(channel, shop_id, sku_id, version, available, status, attempts, next_attempt_at, created_at, updated_at)
       VALUES(?,?,?,?,?,'PENDING',0,0,?,?)
       ON CONFLICT(shop_id, sku_id) DO UPDATE SET version = excluded.version, available = excluded.available,
         status = 'PENDING', attempts = 0, next_attempt_at = 0, updated_at = excluded.updated_at`,
      r.channel, r.shop_id, r.sku_id, r.last_version, r.last_available, now, now,
    );
    repaired++;
  }
  if (repaired > 0) {
    raiseAlert(db, {
      type: 'SYNC_REPAIRED', severity: 'WARN',
      message: `对账发现 ${repaired} 个渠道库存滞后，已重新入队补偿`,
      dedupeKey: 'sync:repair',
    });
  }
  return repaired;
}

export function syncHealth(db: DB) {
  return listShops(db).map((shop) => {
    const counts = get<{ pending: number; retrying: number; dead: number; done: number }>(
      db,
      `SELECT
         COALESCE(SUM(CASE WHEN status='PENDING' THEN 1 ELSE 0 END),0) AS pending,
         COALESCE(SUM(CASE WHEN status='RETRYING' THEN 1 ELSE 0 END),0) AS retrying,
         COALESCE(SUM(CASE WHEN status='DEAD' THEN 1 ELSE 0 END),0) AS dead,
         COALESCE(SUM(CASE WHEN status='DONE' THEN 1 ELSE 0 END),0) AS done
       FROM sync_jobs WHERE shop_id = ?`,
      shop.id,
    );
    const health = get<{ ok: number; last_error: string | null; last_ok_at: number; last_fail_at: number }>(
      db, 'SELECT * FROM platform_health WHERE shop_id = ?', shop.id,
    );
    const lagging = Number(
      get<{ n: number }>(
        db,
        `SELECT COUNT(*) AS n FROM sync_state WHERE shop_id = ? AND last_version > COALESCE(synced_version,0)`,
        shop.id,
      )?.n ?? 0,
    );
    const logs = all<{ status: string }>(db, 'SELECT status FROM sync_log WHERE shop_id = ? ORDER BY id DESC LIMIT 200', shop.id);
    const okCount = logs.filter((l) => l.status === 'OK').length;
    const retryCount = logs.filter((l) => l.status === 'RETRY' || l.status === 'DEAD').length;
    return {
      shopId: shop.id,
      name: shop.name,
      channel: shop.channel,
      pending: Number(counts?.pending ?? 0),
      retrying: Number(counts?.retrying ?? 0),
      dead: Number(counts?.dead ?? 0),
      done: Number(counts?.done ?? 0),
      laggingSkus: lagging,
      ok: health ? Number(health.ok) === 1 : true,
      lastError: health?.last_error ?? null,
      recent: { ok: okCount, retry: retryCount },
    };
  });
}

export function syncQueueSize(db: DB): number {
  return Number(get<{ n: number }>(db, `SELECT COUNT(*) AS n FROM sync_jobs WHERE status IN ('PENDING','RETRYING')`)?.n ?? 0);
}

export function lastSyncTick(db: DB): number {
  return Number(getSetting(db, 'sync_last_tick', '0'));
}
