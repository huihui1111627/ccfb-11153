import { DatabaseSync, type SQLInputValue, type StatementResultingChanges } from 'node:sqlite';

export type DB = DatabaseSync;

const SCHEMA = `
CREATE TABLE IF NOT EXISTS settings (
  key TEXT PRIMARY KEY,
  value TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS skus (
  id TEXT PRIMARY KEY,
  title TEXT NOT NULL,
  is_bundle INTEGER NOT NULL DEFAULT 0,
  safety_stock INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS bundle_components (
  bundle_sku_id TEXT NOT NULL,
  component_sku_id TEXT NOT NULL,
  qty INTEGER NOT NULL,
  PRIMARY KEY (bundle_sku_id, component_sku_id)
);

CREATE TABLE IF NOT EXISTS shops (
  id TEXT PRIMARY KEY,
  channel TEXT NOT NULL,
  name TEXT NOT NULL,
  alloc_mode TEXT NOT NULL CHECK (alloc_mode IN ('fixed','ratio')),
  alloc_value REAL NOT NULL,
  is_small INTEGER NOT NULL DEFAULT 0,
  priority INTEGER NOT NULL DEFAULT 0,
  active INTEGER NOT NULL DEFAULT 1,
  base_url TEXT NOT NULL DEFAULT '',
  fail_safe INTEGER NOT NULL DEFAULT 1
);

CREATE TABLE IF NOT EXISTS stock (
  sku_id TEXT PRIMARY KEY,
  on_hand INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS allocations (
  shop_id TEXT NOT NULL,
  sku_id TEXT NOT NULL,
  allocated INTEGER NOT NULL DEFAULT 0,
  reserved INTEGER NOT NULL DEFAULT 0,
  sold INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (shop_id, sku_id)
);

CREATE TABLE IF NOT EXISTS shared (
  sku_id TEXT PRIMARY KEY,
  reserved INTEGER NOT NULL DEFAULT 0,
  sold INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS orders (
  id TEXT PRIMARY KEY,
  channel TEXT NOT NULL,
  shop_id TEXT NOT NULL,
  external_order_no TEXT NOT NULL,
  status TEXT NOT NULL,
  reject_reason TEXT,
  created_at INTEGER NOT NULL,
  paid_at INTEGER,
  closed_at INTEGER,
  version INTEGER NOT NULL DEFAULT 0,
  total_qty INTEGER NOT NULL DEFAULT 0,
  reserved_qty INTEGER NOT NULL DEFAULT 0,
  paid_qty INTEGER NOT NULL DEFAULT 0,
  idempotency_key TEXT NOT NULL DEFAULT '',
  event_seq INTEGER NOT NULL DEFAULT 0,
  UNIQUE(channel, shop_id, external_order_no)
);
CREATE INDEX IF NOT EXISTS idx_orders_status ON orders(status, created_at);

CREATE TABLE IF NOT EXISTS order_items (
  order_id TEXT NOT NULL,
  requested_sku_id TEXT NOT NULL,
  component_sku_id TEXT NOT NULL,
  qty INTEGER NOT NULL,
  PRIMARY KEY(order_id, component_sku_id)
);

CREATE TABLE IF NOT EXISTS reservations (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  order_id TEXT NOT NULL,
  sku_id TEXT NOT NULL,
  qty INTEGER NOT NULL,
  source TEXT NOT NULL CHECK (source IN ('CHANNEL','SHARED')),
  shop_id TEXT NOT NULL DEFAULT '',
  state TEXT NOT NULL DEFAULT 'ACTIVE' CHECK (state IN ('ACTIVE','PAID','RELEASED','REFUNDED')),
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_reservations_order ON reservations(order_id);
CREATE INDEX IF NOT EXISTS idx_reservations_state ON reservations(sku_id, state);

CREATE TABLE IF NOT EXISTS order_events (
  message_id TEXT NOT NULL,
  channel TEXT NOT NULL,
  shop_id TEXT NOT NULL,
  external_order_no TEXT NOT NULL,
  event_type TEXT NOT NULL,
  seq INTEGER NOT NULL,
  occurred_at INTEGER NOT NULL,
  payload TEXT NOT NULL,
  received_at INTEGER NOT NULL,
  processed INTEGER NOT NULL DEFAULT 0,
  result TEXT,
  conflict INTEGER NOT NULL DEFAULT 0,
  UNIQUE(channel, shop_id, external_order_no, seq)
);
CREATE INDEX IF NOT EXISTS idx_events_order ON order_events(channel, shop_id, external_order_no);

CREATE TABLE IF NOT EXISTS oversold_events (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  order_id TEXT NOT NULL,
  sku_id TEXT NOT NULL,
  qty INTEGER NOT NULL,
  reason TEXT NOT NULL,
  severity TEXT NOT NULL DEFAULT 'OVERSOLD',
  created_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS ledger (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  sku_id TEXT NOT NULL,
  channel TEXT NOT NULL,
  shop_id TEXT,
  reason TEXT NOT NULL,
  delta_on_hand INTEGER NOT NULL DEFAULT 0,
  delta_reserved INTEGER NOT NULL DEFAULT 0,
  delta_allocated INTEGER NOT NULL DEFAULT 0,
  delta_sold INTEGER NOT NULL DEFAULT 0,
  ref_type TEXT NOT NULL,
  ref_id TEXT NOT NULL,
  note TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_ledger_sku ON ledger(sku_id, id);
CREATE INDEX IF NOT EXISTS idx_ledger_ref ON ledger(ref_type, ref_id);
CREATE INDEX IF NOT EXISTS idx_ledger_channel ON ledger(channel, shop_id, id);

CREATE TABLE IF NOT EXISTS stocktakes (
  id TEXT PRIMARY KEY,
  sku_id TEXT NOT NULL,
  counted INTEGER NOT NULL,
  before_qty INTEGER NOT NULL,
  delta INTEGER NOT NULL,
  rule TEXT NOT NULL DEFAULT 'pro_rata',
  note TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS alerts (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  type TEXT NOT NULL,
  sku_id TEXT,
  shop_id TEXT,
  severity TEXT NOT NULL,
  message TEXT NOT NULL,
  dedupe_key TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL DEFAULT 'OPEN',
  created_at INTEGER NOT NULL,
  resolved_at INTEGER
);
CREATE INDEX IF NOT EXISTS idx_alerts_status ON alerts(status, id);
CREATE UNIQUE INDEX IF NOT EXISTS idx_alerts_dedupe ON alerts(dedupe_key) WHERE status = 'OPEN';

CREATE TABLE IF NOT EXISTS sync_state (
  shop_id TEXT NOT NULL,
  sku_id TEXT NOT NULL,
  last_version INTEGER NOT NULL DEFAULT 0,
  last_available INTEGER NOT NULL DEFAULT -1,
  synced_version INTEGER NOT NULL DEFAULT 0,
  synced_available INTEGER NOT NULL DEFAULT -1,
  last_ok_at INTEGER NOT NULL DEFAULT 0,
  updated_at INTEGER NOT NULL,
  PRIMARY KEY (shop_id, sku_id)
);

CREATE TABLE IF NOT EXISTS sync_jobs (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  channel TEXT NOT NULL,
  shop_id TEXT NOT NULL,
  sku_id TEXT NOT NULL,
  version INTEGER NOT NULL,
  available INTEGER NOT NULL,
  status TEXT NOT NULL DEFAULT 'PENDING',
  attempts INTEGER NOT NULL DEFAULT 0,
  next_attempt_at INTEGER NOT NULL DEFAULT 0,
  last_error TEXT,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  UNIQUE(shop_id, sku_id)
);
CREATE INDEX IF NOT EXISTS idx_sync_jobs_status ON sync_jobs(status, next_attempt_at);

CREATE TABLE IF NOT EXISTS sync_log (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  channel TEXT NOT NULL,
  shop_id TEXT NOT NULL,
  sku_id TEXT NOT NULL,
  idempotency_key TEXT NOT NULL,
  version INTEGER NOT NULL,
  available INTEGER NOT NULL,
  status TEXT NOT NULL,
  detail TEXT NOT NULL DEFAULT '',
  at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS platform_health (
  shop_id TEXT PRIMARY KEY,
  ok INTEGER NOT NULL DEFAULT 1,
  last_error TEXT,
  last_ok_at INTEGER NOT NULL DEFAULT 0,
  last_fail_at INTEGER NOT NULL DEFAULT 0
);
`;

export function openDb(path = ':memory:'): DB {
  const db = new DatabaseSync(path);
  db.exec('PRAGMA journal_mode = WAL');
  db.exec('PRAGMA foreign_keys = ON');
  db.exec('PRAGMA busy_timeout = 5000');
  db.exec(SCHEMA);
  return db;
}

/** 同步事务。所有库存变更必须走这里，保证预占/扣减的原子性。 */
export function tx<T>(db: DB, fn: () => T): T {
  if (db.isTransaction) return fn();
  db.exec('BEGIN IMMEDIATE');
  try {
    const result = fn();
    db.exec('COMMIT');
    return result;
  } catch (err) {
    try {
      db.exec('ROLLBACK');
    } catch {
      /* ignore rollback errors */
    }
    throw err;
  }
}

export function all<T = Record<string, unknown>>(db: DB, sql: string, ...params: SQLInputValue[]): T[] {
  return db.prepare(sql).all(...params) as unknown as T[];
}

export function get<T = Record<string, unknown>>(db: DB, sql: string, ...params: SQLInputValue[]): T | undefined {
  return db.prepare(sql).get(...params) as unknown as T | undefined;
}

export function run(db: DB, sql: string, ...params: SQLInputValue[]): StatementResultingChanges {
  return db.prepare(sql).run(...params);
}

export function getSetting(db: DB, key: string, fallback: string): string {
  const row = get<{ value: string }>(db, 'SELECT value FROM settings WHERE key = ?', key);
  return row ? row.value : fallback;
}

export function setSetting(db: DB, key: string, value: string): void {
  run(db, 'INSERT INTO settings(key, value) VALUES(?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value', key, value);
}

/** 全局自增版本号，库存同步的幂等键基于它。 */
export function nextGlobalVersion(db: DB): number {
  const current = Number(getSetting(db, 'sync_version', '0'));
  const next = current + 1;
  setSetting(db, 'sync_version', String(next));
  return next;
}
