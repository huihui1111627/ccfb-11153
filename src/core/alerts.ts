import type { DB } from './db.js';
import { run, tx } from './db.js';

export interface AlertInput {
  type: string;
  skuId?: string | null;
  shopId?: string | null;
  severity: 'WARN' | 'CRITICAL';
  message: string;
  dedupeKey?: string;
}

/** 相同 dedupeKey 且仍 OPEN 的告警只保留一条，避免大促刷屏 */
export function raiseAlert(db: DB, input: AlertInput): void {
  const key = input.dedupeKey ?? `${input.type}:${input.skuId ?? ''}:${input.shopId ?? ''}`;
  tx(db, () => {
    const existing = db
      .prepare('SELECT id FROM alerts WHERE dedupe_key = ? AND status = ?')
      .get(key, 'OPEN') as { id: number } | undefined;
    if (existing) return;
    run(
      db,
      'INSERT INTO alerts(type, sku_id, shop_id, severity, message, dedupe_key, status, created_at) VALUES(?,?,?,?,?,?,?,?)',
      input.type,
      input.skuId ?? null,
      input.shopId ?? null,
      input.severity,
      input.message,
      key,
      'OPEN',
      Date.now(),
    );
  });
}

export function resolveAlert(db: DB, id: number): void {
  run(db, 'UPDATE alerts SET status = ?, resolved_at = ? WHERE id = ?', 'RESOLVED', Date.now(), id);
}
