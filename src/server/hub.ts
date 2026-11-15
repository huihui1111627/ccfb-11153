import { createServer, type IncomingMessage, type Server, type ServerResponse } from 'node:http';
import type { AddressInfo } from 'node:net';
import type { DB } from '../core/db.js';
import { all, get, run } from '../core/db.js';
import { InventoryEngine, OrderValidationError, explodeLines, getOrderView } from '../core/engine.js';
import { OrderEventProcessor } from '../core/events.js';
import {
  allocationMatrix,
  bundleComponents,
  channelUnreserved,
  getOnHand,
  listShops,
  listSkus,
  getSku,
  markSkuChanged,
  sharedPoolQty,
  skuSellable,
  upsertShop,
  upsertSku,
} from '../core/catalog.js';
import { applyStocktake } from '../core/stocktake.js';
import { syncHealth, SyncWorker, type PushFn } from '../core/sync.js';
import { resolveAlert } from '../core/alerts.js';
import type { ChannelCode, OrderEventPayload } from '../core/types.js';

export interface HubOptions {
  db: DB;
  /** 把某渠道某 SKU 的可用库存推到平台。job 带幂等键与版本号。 */
  pushInventory?: (job: {
    channel: string;
    shopId: string;
    skuId: string;
    version: number;
    available: number;
    idempotencyKey: string;
  }) => Promise<{ ok: boolean; duplicated?: boolean; stale?: boolean; error?: string }>;
  timeoutMs?: number;
}

function json(res: ServerResponse, status: number, body: unknown): void {
  const data = JSON.stringify(body);
  res.writeHead(status, {
    'content-type': 'application/json; charset=utf-8',
    'content-length': Buffer.byteLength(data),
    'access-control-allow-origin': '*',
  });
  res.end(data);
}

async function readJson<T>(req: IncomingMessage): Promise<T> {
  const chunks: Buffer[] = [];
  for await (const chunk of req) chunks.push(chunk as Buffer);
  const text = Buffer.concat(chunks).toString('utf8');
  if (!text) throw new HttpError(400, 'EMPTY_BODY', '请求体为空');
  try {
    return JSON.parse(text) as T;
  } catch {
    throw new HttpError(400, 'BAD_JSON', '请求体不是合法 JSON');
  }
}

export class HttpError extends Error {
  constructor(readonly status: number, readonly code: string, message: string) {
    super(message);
  }
}

interface RequestCtx {
  req: IncomingMessage;
  res: ServerResponse;
  url: URL;
  body: () => Promise<string>;
}

/** 渠道开放 API + 平台 webhook + 运营后台 API 合一的中台服务 */
export class InventoryHubServer {
  readonly engine: InventoryEngine;
  readonly processor: OrderEventProcessor;
  readonly syncWorker: SyncWorker;
  private server: Server;

  constructor(private readonly opts: HubOptions) {
    this.engine = new InventoryEngine(opts.db);
    this.processor = new OrderEventProcessor(opts.db, this.engine);
    this.syncWorker = new SyncWorker(opts.db, this.makePusher());
    this.server = createServer((req, res) => {
      this.route(req, res).catch((err) => {
        if (err instanceof HttpError) {
          json(res, err.status, { ok: false, code: err.code, error: err.message });
        } else if (err instanceof OrderValidationError) {
          json(res, 400, { ok: false, code: 'VALIDATION', error: err.message });
        } else {
          json(res, 500, { ok: false, code: 'INTERNAL', error: String((err as Error).message ?? err) });
        }
      });
    });
  }

  private makePusher(): PushFn {
    return async (job) => {
      if (!this.opts.pushInventory) return { ok: false, retryable: true, error: '渠道未配置推送函数' };
      try {
        const r = await this.opts.pushInventory(job);
        if (!r || !r.ok) return { ok: false, retryable: true, error: r?.error ?? '推送失败' };
        return { ok: true, retryable: false, duplicated: r.duplicated, stale: r.stale };
      } catch (err) {
        return { ok: false, retryable: true, error: String((err as Error).message ?? err) };
      }
    };
  }

  async listen(port = 0): Promise<number> {
    await new Promise<void>((resolve) => this.server.listen(port, '127.0.0.1', resolve));
    return (this.server.address() as AddressInfo).port;
  }

  async close(): Promise<void> {
    await new Promise<void>((resolve, reject) => this.server.close((err) => (err ? reject(err) : resolve())));
  }

  private async route(req: IncomingMessage, res: ServerResponse): Promise<void> {
    const url = new URL(req.url ?? '/', 'http://localhost');
    const path = url.pathname;
    const method = req.method ?? 'GET';

    // ---- 平台 webhook ----
    if (method === 'POST' && path === '/webhooks/orders') {
      const event = await readJson<OrderEventPayload>(req);
      if (!event.channel || !event.shopId || !event.externalOrderNo || !event.type) {
        throw new HttpError(400, 'BAD_EVENT', '事件缺少必要字段');
      }
      const result = this.processor.process(event);
      json(res, 200, { ...result, ok: true });
      return;
    }

    // ---- 渠道库存拉取 ----
    if (method === 'GET' && path === '/api/inventory') {
      const shopId = url.searchParams.get('shopId') ?? '';
      const shop = listShops(this.opts.db).find((s) => s.id === shopId);
      if (!shop) throw new HttpError(404, 'SHOP_NOT_FOUND', `店铺不存在: ${shopId}`);
      const skuId = url.searchParams.get('skuId');
      const items = skuId
        ? [{ skuId, available: Math.max(0, channelUnreserved(this.opts.db, shopId, skuId)) }]
        : listSkus(this.opts.db).map((s) => ({ skuId: s.id, available: Math.max(0, channelUnreserved(this.opts.db, shopId, s.id)) }));
      json(res, 200, { ok: true, shopId, items });
      return;
    }

    // ---- 渠道下单：预占 ----
    if (method === 'POST' && path === '/api/orders') {
      const body = await readJson<{
        shopId: string;
        externalOrderNo: string;
        lines: Array<{ skuId: string; qty: number }>;
        idempotencyKey?: string;
      }>(req);
      const shop = listShops(this.opts.db).find((s) => s.id === body.shopId);
      if (!shop) throw new HttpError(404, 'SHOP_NOT_FOUND', `店铺不存在: ${body.shopId}`);
      if (!Array.isArray(body.lines) || body.lines.length === 0) throw new HttpError(400, 'BAD_LINES', '订单行不能为空');
      const result = this.engine.reserve({
        shopId: shop.id,
        channel: shop.channel,
        externalOrderNo: body.externalOrderNo,
        lines: body.lines,
        idempotencyKey: body.idempotencyKey,
      });
      const status = result.ok ? 201 : 409;
      json(res, status, result);
      return;
    }

    if (method === 'POST' && path === '/api/orders/pay') {
      const body = await readJson<{ orderId: string }>(req);
      const result = this.engine.pay(body.orderId);
      json(res, result.ok ? 200 : 409, result);
      return;
    }

    if (method === 'POST' && path === '/api/orders/cancel') {
      const body = await readJson<{ orderId: string; reason?: string }>(req);
      const result = this.engine.release(body.orderId, body.reason ?? '渠道取消');
      json(res, result.ok ? 200 : 409, result);
      return;
    }

    if (method === 'POST' && path === '/api/orders/refund') {
      const body = await readJson<{ orderId: string }>(req);
      const result = this.engine.refund(body.orderId);
      json(res, result.ok ? 200 : 409, result);
      return;
    }

    if (method === 'GET' && path.startsWith('/api/orders/')) {
      const orderId = decodeURIComponent(path.slice('/api/orders/'.length));
      const view = getOrderView(this.opts.db, orderId);
      if (!view) throw new HttpError(404, 'ORDER_NOT_FOUND', '订单不存在');
      json(res, 200, { ok: true, order: view });
      return;
    }

    // ---- 运营后台 API ----
    if (path.startsWith('/admin/api/')) {
      await this.admin(method, path.slice('/admin/api'.length), url, req, res);
      return;
    }

    if (method === 'GET' && (path === '/' || path === '/admin')) {
      res.writeHead(302, { location: '/admin/' });
      res.end();
      return;
    }

    // 静态后台页面
    if (method === 'GET' && path.startsWith('/admin/')) {
      await serveAdminFile(path, res);
      return;
    }

    json(res, 404, { ok: false, code: 'NOT_FOUND', error: `未知路由 ${method} ${path}` });
  }

  private async admin(
    method: string,
    path: string,
    url: URL,
    req: IncomingMessage,
    res: ServerResponse,
  ): Promise<void> {
    const db = this.opts.db;

    if (method === 'GET' && path === '/overview') {
      json(res, 200, { ok: true, data: this.overview() });
      return;
    }

    if (method === 'GET' && path === '/inventory') {
      json(res, 200, { ok: true, data: allocationMatrix(db) });
      return;
    }

    if (method === 'GET' && path === '/skus') {
      json(res, 200, { ok: true, data: listSkus(db) });
      return;
    }

    if (method === 'POST' && path === '/skus') {
      const body = await readJson<{
        id: string; title: string; safetyStock?: number; components?: Array<{ skuId: string; qty: number }>;
      }>(req);
      upsertSku(db, body);
      json(res, 200, { ok: true });
      return;
    }

    if (method === 'GET' && path === '/shops') {
      json(res, 200, { ok: true, data: listShops(db) });
      return;
    }

    if (method === 'POST' && path === '/shops') {
      const body = await readJson<Parameters<typeof upsertShop>[1]>(req);
      upsertShop(db, body);
      json(res, 200, { ok: true });
      return;
    }

    if (method === 'POST' && path === '/allocate') {
      const body = await readJson<{ skuId: string }>(req);
      const { rebalanceAllocation } = await import('../core/catalog.js');
      rebalanceAllocation(db, body.skuId, '后台手动重分配', `MANUAL:${Date.now()}`);
      markSkuChanged(db, body.skuId, 'manual_allocate');
      json(res, 200, { ok: true, data: allocationMatrix(db).find((r) => r.skuId === body.skuId) });
      return;
    }

    if (method === 'POST' && path === '/stocktake') {
      const body = await readJson<{ stocktakeId?: string; skuId: string; counted: number; rule?: 'pro_rata' | 'shared_first' | 'channel_first'; note?: string }>(req);
      const result = applyStocktake(db, this.engine, {
        stocktakeId: body.stocktakeId ?? `ST-${Date.now()}-${Math.random().toString(36).slice(2, 6)}`,
        skuId: body.skuId,
        counted: body.counted,
        rule: body.rule,
        note: body.note,
      });
      json(res, 200, { ok: true, data: result });
      return;
    }

    if (method === 'GET' && path === '/ledger') {
      const skuId = url.searchParams.get('skuId') ?? '';
      const limit = Number(url.searchParams.get('limit') ?? '200');
      const refType = url.searchParams.get('refType');
      const refId = url.searchParams.get('refId');
      let rows;
      if (refType && refId) rows = this.engine.traceByRef(refType, refId);
      else if (skuId) rows = this.engine.ledgerFor(skuId, limit);
      else rows = all(db, 'SELECT * FROM ledger ORDER BY id DESC LIMIT ?', limit);
      json(res, 200, { ok: true, data: rows });
      return;
    }

    if (method === 'GET' && path === '/orders') {
      const limit = Number(url.searchParams.get('limit') ?? '100');
      const channel = url.searchParams.get('channel');
      const status = url.searchParams.get('status');
      const rows = all(
        db,
        `SELECT * FROM orders ${channel || status ? 'WHERE' : ''}
           ${channel ? 'channel = ?' : ''} ${channel && status ? 'AND' : ''} ${status ? 'status = ?' : ''}
         ORDER BY created_at DESC LIMIT ?`,
        ...[channel, status].filter((v): v is string => !!v),
        limit,
      );
      json(res, 200, { ok: true, data: rows });
      return;
    }

    if (method === 'GET' && path === '/alerts') {
      const status = url.searchParams.get('status') ?? 'OPEN';
      const rows = all(db, 'SELECT * FROM alerts WHERE status = ? ORDER BY id DESC LIMIT 200', status);
      json(res, 200, { ok: true, data: rows });
      return;
    }

    if (method === 'POST' && path === '/alerts/resolve') {
      const body = await readJson<{ id: number }>(req);
      resolveAlert(db, body.id);
      json(res, 200, { ok: true });
      return;
    }

    if (method === 'GET' && path === '/sync') {
      json(res, 200, { ok: true, data: syncHealth(db) });
      return;
    }

    if (method === 'POST' && path === '/sync/run') {
      const r = await this.syncWorker.drain(500);
      json(res, 200, { ok: true, data: r });
      return;
    }

    if (method === 'POST' && path === '/sync/repair') {
      const { repairLaggingSync } = await import('../core/sync.js');
      json(res, 200, { ok: true, data: { repaired: repairLaggingSync(db) } });
      return;
    }

    if (method === 'GET' && path === '/reconcile') {
      json(res, 200, { ok: true, data: reconcile(db) });
      return;
    }

    if (method === 'GET' && path === '/trace') {
      const skuId = url.searchParams.get('skuId');
      if (!skuId) throw new HttpError(400, 'BAD_REQUEST', '缺少 skuId');
      json(res, 200, { ok: true, data: traceSku(db, skuId) });
      return;
    }

    if (method === 'GET' && path === '/platforms') {
      json(res, 200, { ok: true, data: this.platformsSnapshot() });
      return;
    }

    throw new HttpError(404, 'NOT_FOUND', `未知管理接口 ${path}`);
  }

  overview() {
    const db = this.opts.db;
    const orders = get<{
      total: number; reserved: number; paid: number; released: number; cancelled: number; failed: number;
    }>(
      db,
      `SELECT COUNT(*) AS total,
        COALESCE(SUM(CASE WHEN status='RESERVED' THEN 1 ELSE 0 END),0) AS reserved,
        COALESCE(SUM(CASE WHEN status='PAID' THEN 1 ELSE 0 END),0) AS paid,
        COALESCE(SUM(CASE WHEN status='RELEASED' THEN 1 ELSE 0 END),0) AS released,
        COALESCE(SUM(CASE WHEN status='CANCELLED' THEN 1 ELSE 0 END),0) AS cancelled,
        COALESCE(SUM(CASE WHEN status='FAILED' THEN 1 ELSE 0 END),0) AS failed
       FROM orders`,
    );
    const oversold = Number(get<{ n: number }>(db, 'SELECT COUNT(*) AS n FROM oversold_events')?.n ?? 0);
    const openAlerts = Number(get<{ n: number }>(db, 'SELECT COUNT(*) AS n FROM alerts WHERE status = ?', 'OPEN')?.n ?? 0);
    const syncPending = Number(
      get<{ n: number }>(db, `SELECT COUNT(*) AS n FROM sync_jobs WHERE status IN ('PENDING','RETRYING')`)?.n ?? 0,
    );
    const skus = listSkus(db);
    return {
      skus: skus.length,
      onHandTotal: skus.filter((s) => !s.isBundle).reduce((sum, s) => sum + s.onHand, 0),
      sellableTotal: skus.filter((s) => !s.isBundle).reduce((sum, s) => sum + s.sellable, 0),
      orders: orders ?? { total: 0, reserved: 0, paid: 0, released: 0, cancelled: 0, failed: 0 },
      oversold,
      openAlerts,
      syncPending,
      shops: listShops(db).length,
    };
  }

  platformsSnapshot() {
    const db = this.opts.db;
    return listShops(db).map((shop) => {
      const lag = all<{ sku_id: string; last_version: number; synced_version: number; last_available: number; synced_available: number }>(
        db,
        `SELECT sku_id, last_version, COALESCE(synced_version,0) AS synced_version, last_available, COALESCE(synced_available,-1) AS synced_available
         FROM sync_state WHERE shop_id = ?`,
        shop.id,
      );
      const local = Object.fromEntries(
        lag.map((l) => [l.sku_id, { version: l.synced_version, available: l.synced_available, desired: l.last_available }]),
      );
      return { shop, inventory: local };
    });
  }
}

export function reconcile(db: DB) {
  // 账实核对：实物 on_hand 必须等于 所有渠道 allocated 之和 + 共享池未分配量
  const skus = listSkus(db);
  const discrepancies: Array<{
    skuId: string; onHand: number; channelAllocated: number; shared: number; diff: number;
    ledgerOnHand: number; ledgerDiff: number;
  }> = [];
  for (const sku of skus) {
    const onHand = getOnHand(db, sku.id);
    const allocated = Number(
      get<{ total: number }>(db, 'SELECT COALESCE(SUM(allocated),0) AS total FROM allocations WHERE sku_id = ?', sku.id)?.total ?? 0,
    );
    const shared = sharedPoolQty(db, sku.id);
    const diff = onHand - (allocated + shared);
    const ledger = get<{ total: number }>(
      db,
      'SELECT COALESCE(SUM(delta_on_hand),0) AS total FROM ledger WHERE sku_id = ?',
      sku.id,
    );
    const ledgerOnHand = Number(ledger?.total ?? 0);
    if (diff !== 0 || ledgerOnHand !== onHand) {
      discrepancies.push({ skuId: sku.id, onHand, channelAllocated: allocated, shared, diff, ledgerOnHand, ledgerDiff: ledgerOnHand - onHand });
    }
  }
  const oversoldRows = all<{ order_id: string; sku_id: string; qty: number; reason: string }>(
    db, 'SELECT order_id, sku_id, qty, reason FROM oversold_events ORDER BY id',
  );
  const oversoldQty = oversoldRows.reduce((s, r) => s + Number(r.qty), 0);
  return { discrepancies, oversoldRows, oversoldQty, ok: discrepancies.length === 0 && oversoldRows.length === 0 };
}

export function traceSku(db: DB, skuId: string) {
  const sku = getSku(db, skuId);
  const entries = all<{
    id: number; channel: string; shop_id: string | null; reason: string; delta_on_hand: number;
    delta_reserved: number; delta_allocated: number; delta_sold: number; ref_type: string; ref_id: string;
    note: string; created_at: number;
  }>(db, 'SELECT * FROM ledger WHERE sku_id = ? ORDER BY id', skuId);
  const bundles = all<{ bundle_sku_id: string }>(db, 'SELECT bundle_sku_id FROM bundle_components WHERE component_sku_id = ?', skuId);
  return {
    skuId,
    sku: sku ?? null,
    entryCount: entries.length,
    current: {
      onHand: getOnHand(db, skuId),
      channelUnreserved: listShops(db).reduce((s, shop) => s + channelUnreserved(db, shop.id, skuId), 0),
      shared: sharedPoolQty(db, skuId),
      sellable: skuSellable(db, skuId),
      components: bundleComponents(db, skuId),
    },
    usedByBundles: bundles.map((b) => b.bundle_sku_id),
    entries: entries.map((e) => ({
      id: e.id,
      channel: e.channel,
      shopId: e.shop_id,
      reason: e.reason,
      deltaOnHand: e.delta_on_hand,
      deltaReserved: e.delta_reserved,
      deltaAllocated: e.delta_allocated,
      deltaSold: e.delta_sold,
      refType: e.ref_type,
      refId: e.ref_id,
      note: e.note,
      createdAt: e.created_at,
    })),
  };
}

async function serveAdminFile(path: string, res: ServerResponse): Promise<void> {
  const { readFile } = await import('node:fs/promises');
  const { fileURLToPath } = await import('node:url');
  const { dirname, join, normalize } = await import('node:path');
  const here = dirname(fileURLToPath(import.meta.url));
  const rel = path.replace(/^\/admin\/?/, '') || 'index.html';
  const target = normalize(join(here, '..', 'web', rel));
  if (!target.includes(`${join(here, '..', 'web')}`)) {
    json(res, 403, { ok: false, error: 'forbidden' });
    return;
  }
  try {
    const data = await readFile(target);
    const type = target.endsWith('.html') ? 'text/html; charset=utf-8' : target.endsWith('.css') ? 'text/css' : 'application/javascript; charset=utf-8';
    res.writeHead(200, { 'content-type': type });
    res.end(data);
  } catch {
    json(res, 404, { ok: false, error: 'not found' });
  }
}

/**
 * 轻量守恒断言：渠道分配之和 + 共享池 == 实物，且 0 <= 预占 <= 分配，共享池 >= 0。
 * 返回违规列表（空数组表示健康），用于压测过程中持续巡检。
 */
export function invariantViolations(db: DB): Array<{ skuId: string; rule: string; detail: string }> {
  const out: Array<{ skuId: string; rule: string; detail: string }> = [];
  for (const row of allocationMatrix(db)) {
    const sum = row.channels.reduce((s, c) => s + c.allocated, 0);
    if (row.onHand !== sum + row.sharedQty) {
      out.push({ skuId: row.skuId, rule: 'balance', detail: `onHand=${row.onHand} allocSum=${sum} shared=${row.sharedQty}` });
    }
    if (row.sharedQty < 0) out.push({ skuId: row.skuId, rule: 'shared_nonneg', detail: `shared=${row.sharedQty}` });
    if (row.sharedReserved > Math.max(0, row.sharedQty)) {
      out.push({ skuId: row.skuId, rule: 'shared_reserved', detail: `reserved=${row.sharedReserved} pool=${row.sharedQty}` });
    }
    for (const c of row.channels) {
      if (c.reserved > c.allocated) {
        out.push({ skuId: row.skuId, rule: 'reserved_le_allocated', detail: `${c.shopId} allocated=${c.allocated} reserved=${c.reserved}` });
      }
      if (c.allocated < 0) out.push({ skuId: row.skuId, rule: 'allocated_nonneg', detail: `${c.shopId} allocated=${c.allocated}` });
    }
    const sku = getSku(db, row.skuId);
    if (sku && !sku.isBundle && row.onHand < 0) {
      out.push({ skuId: row.skuId, rule: 'onhand_nonneg', detail: `onHand=${row.onHand}` });
    }
  }
  return out;
}
