import { createServer, type IncomingMessage, type Server, type ServerResponse } from 'node:http';
import { setTimeout as sleep } from 'node:timers/promises';
import type { AddressInfo } from 'node:net';
import type { ChannelCode, OrderEventType, OrderLine } from '../core/types.js';
import { reserveAtHub } from './client.js';

export interface ChaosConfig {
  /** 令牌桶：每秒请求数 */
  rps: number;
  burst: number;
  latencyMinMs: number;
  latencyMaxMs: number;
  /** 请求发出去后 hang 住不返回（触发客户端超时） */
  timeoutRate: number;
  timeoutHangMs: number;
  /** 5xx 比例 */
  errorRate: number;
  /** 429 以外的瞬时错误用 503 */
  busyRate: number;
  /** webhook 重复投递比例 */
  duplicateRate: number;
  /** 乱序比例（延迟超过其它事件） */
  reorderRate: number;
  reorderMinDelayMs: number;
  reorderMaxDelayMs: number;
  /** webhook 首投失败比例（平台重试） */
  pushFailRate: number;
  /** 预占成功后响应丢失的比例（平台以为失败，用户重新下单） */
  lostAckRate: number;
}

export const DEFAULT_CHAOS: ChaosConfig = {
  rps: 400,
  burst: 500,
  latencyMinMs: 3,
  latencyMaxMs: 30,
  timeoutRate: 0.015,
  timeoutHangMs: 900,
  errorRate: 0.015,
  busyRate: 0.01,
  duplicateRate: 0.1,
  reorderRate: 0.2,
  reorderMinDelayMs: 30,
  reorderMaxDelayMs: 400,
  pushFailRate: 0.04,
  lostAckRate: 0.01,
};

export interface MockPlatformConfig {
  channel: ChannelCode;
  shopId: string;
  /** 平台预占中台的基础地址 */
  hubBaseUrl: string;
  chaos?: Partial<ChaosConfig>;
}

interface MockOrderState {
  orderNo: string;
  lines: OrderLine[];
  orderId: string | null;
  status: 'RESERVED' | 'REJECTED' | 'PAID' | 'CANCELLED' | 'PENDING';
  createdAt: number;
  soldCache: number;
}

export interface MockStats {
  channel: ChannelCode;
  shopId: string;
  httpRequests: number;
  httpRateLimited: number;
  httpTimeouts: number;
  httpErrors: number;
  inventoryPushes: number;
  inventoryApplied: number;
  inventoryStaleIgnored: number;
  inventoryDuplicateKey: number;
  inventoryRejectedByLocal: number;
  ordersCreated: number;
  ordersRejected: number;
  ordersPaymentFailed: number;
  ordersPaid: number;
  ordersCancelled: number;
  webhookDeliveries: number;
  webhookDuplicates: number;
  webhookRetries: number;
  webhookFailures: number;
  oversellBlockedByCache: number;
}

class TokenBucket {
  private tokens: number;
  private last: number;
  constructor(private readonly rate: number, private readonly capacity: number) {
    this.tokens = capacity;
    this.last = Date.now();
  }
  take(): boolean {
    const now = Date.now();
    this.tokens = Math.min(this.capacity, this.tokens + ((now - this.last) / 1000) * this.rate);
    this.last = now;
    if (this.tokens < 1) return false;
    this.tokens -= 1;
    return true;
  }
}

function json(res: ServerResponse, status: number, body: unknown): void {
  const data = JSON.stringify(body);
  res.writeHead(status, {
    'content-type': 'application/json; charset=utf-8',
    'content-length': Buffer.byteLength(data),
  });
  res.end(data);
}

async function readBody(req: IncomingMessage): Promise<string> {
  const chunks: Buffer[] = [];
  for await (const chunk of req) chunks.push(chunk as Buffer);
  return Buffer.concat(chunks).toString('utf8');
}

function rand(min: number, max: number): number {
  return min + Math.random() * (max - min);
}

/**
 * 单个渠道店铺的模拟平台。
 *
 * 真实平台的工作方式：
 * - 库存是异步推送过来的本地缓存，永远滞后于中台；下单时平台只做缓存校验；
 * - 下单必须先在中台同步预占（否则大促必超卖），预占成功才允许支付；
 * - 支付/取消这类状态变更会被平台以 webhook 推到中台，可能重复、可能乱序；
 * - 接口有限流、偶发超时/5xx。
 */
export class MockPlatform {
  private server: Server;
  private bucket: TokenBucket;
  readonly chaos: ChaosConfig;
  private inventory = new Map<string, { available: number; version: number; updatedAt: number }>();
  private idempotency = new Map<string, { version: number; available: number; at: number }>();
  private orders = new Map<string, MockOrderState>();
  private timers = new Set<NodeJS.Timeout>();
  private inflight = 0;
  private pendingOrders = 0;
  private orderSeq = 1;
  readonly stats: MockStats;
  port = 0;

  constructor(readonly config: MockPlatformConfig) {
    this.chaos = { ...DEFAULT_CHAOS, ...config.chaos };
    this.bucket = new TokenBucket(this.chaos.rps, this.chaos.burst);
    this.stats = {
      channel: config.channel,
      shopId: config.shopId,
      httpRequests: 0,
      httpRateLimited: 0,
      httpTimeouts: 0,
      httpErrors: 0,
      inventoryPushes: 0,
      inventoryApplied: 0,
      inventoryStaleIgnored: 0,
      inventoryDuplicateKey: 0,
      inventoryRejectedByLocal: 0,
      ordersCreated: 0,
      ordersRejected: 0,
      ordersPaymentFailed: 0,
      ordersPaid: 0,
      ordersCancelled: 0,
      webhookDeliveries: 0,
      webhookDuplicates: 0,
      webhookRetries: 0,
      webhookFailures: 0,
      oversellBlockedByCache: 0,
    };
    this.server = createServer((req, res) => {
      this.handle(req, res).catch((err) => json(res, 500, { ok: false, error: String(err) }));
    });
  }

  async listen(port = 0): Promise<number> {
    await new Promise<void>((resolve) => this.server.listen(port, '127.0.0.1', resolve));
    this.port = (this.server.address() as AddressInfo).port;
    return this.port;
  }

  async close(): Promise<void> {
    for (const t of this.timers) clearTimeout(t);
    this.timers.clear();
    await new Promise<void>((resolve, reject) => this.server.close((err) => (err ? reject(err) : resolve())));
  }

  private schedule(fn: () => void, ms: number): void {
    const timer = setTimeout(() => {
      this.timers.delete(timer);
      fn();
    }, ms);
    this.timers.add(timer);
    timer.unref?.();
  }

  get baseUrl(): string {
    return `http://127.0.0.1:${this.port}`;
  }

  get localInventory(): Map<string, { available: number; version: number }> {
    return new Map([...this.inventory].map(([k, v]) => [k, { available: v.available, version: v.version }]));
  }

  get orderList(): MockOrderState[] {
    return [...this.orders.values()];
  }

  get pendingTimers(): number {
    return this.timers.size;
  }

  /** 未完成的异步工作：定时器 + 在途 webhook + 在途下单 */
  get pendingWork(): number {
    return this.timers.size + this.inflight + this.pendingOrders;
  }

  /**
   * 模拟用户下单。返回平台侧结果：
   * 平台先查本地缓存（可能误判有货）→ 同步调中台预占 → 结果落库，
   * 再异步推送 ORDER_CREATED；随后按概率支付或取消。
   */
  async placeOrder(
    lines: OrderLine[],
    opts: { externalOrderNo?: string; payProb?: number; payDelayMs?: number; cancelProb?: number } = {},
  ): Promise<MockOrderState> {
    this.pendingOrders++;
    try {
      return await this.placeOrderInner(lines, opts);
    } finally {
      this.pendingOrders--;
    }
  }

  private async placeOrderInner(
    lines: OrderLine[],
    opts: { externalOrderNo?: string; payProb?: number; payDelayMs?: number; cancelProb?: number } = {},
  ): Promise<MockOrderState> {
    const orderNo =
      opts.externalOrderNo ?? `${this.config.shopId.split('-')[0]!.toUpperCase()}${String(++this.orderSeq).padStart(8, '0')}`;
    const local = lines.map((l) => ({ ...l, cached: this.inventory.get(l.skuId)?.available ?? 0 }));
    const cacheShort = local.find((l) => l.cached < l.qty);
    if (cacheShort) {
      // 本地缓存没货：平台直接拦截，不占用中台库存
      this.stats.oversellBlockedByCache++;
      const state: MockOrderState = { orderNo, lines, orderId: null, status: 'REJECTED', createdAt: Date.now(), soldCache: 0 };
      this.orders.set(orderNo, state);
      this.stats.ordersRejected++;
      return state;
    }

    this.stats.ordersCreated++;
    const reserve = await reserveAtHub(
      { shopId: this.config.shopId, externalOrderNo: orderNo, lines },
      { baseUrl: this.config.hubBaseUrl, timeoutMs: 2000, retries: 2 },
    );

    // 预占响应丢失：中台可能成功预占，但平台看到的是超时 → 需要平台重试/对账；
    // 这里模拟“平台发送了取消”，让中台的预占能被释放，避免虚假占用。
    const lostAck = reserve.ok && Math.random() < this.chaos.lostAckRate;

    if (!reserve.ok) {
      this.stats.ordersRejected++;
      const state: MockOrderState = { orderNo, lines, orderId: null, status: 'REJECTED', createdAt: Date.now(), soldCache: 0 };
      this.orders.set(orderNo, state);
      return state;
    }

    const state: MockOrderState = {
      orderNo,
      lines,
      orderId: reserve.orderId ?? null,
      status: 'RESERVED',
      createdAt: Date.now(),
      soldCache: 0,
    };
    this.orders.set(orderNo, state);

    if (lostAck) {
      // 平台认为下单失败：推一条取消，中台释放预占
      state.status = 'CANCELLED';
      state.orderId = reserve.orderId ?? null;
      this.stats.ordersRejected++;
      this.pushEvent(orderNo, 'ORDER_CREATED', 1, lines);
      this.pushEvent(orderNo, 'ORDER_CANCELLED', 3);
      return state;
    }

    this.pushEvent(orderNo, 'ORDER_CREATED', 1, lines);

    const cancelProb = opts.cancelProb ?? 0.1;
    if (Math.random() < cancelProb) {
      this.schedule(() => this.cancelOrder(orderNo), rand(40, 400));
      return state;
    }

    const payProb = opts.payProb ?? 0.86;
    if (Math.random() < payProb) {
      this.schedule(() => this.markPaid(orderNo), opts.payDelayMs ?? rand(60, 800));
    }
    return state;
  }

  markPaid(orderNo: string): void {
    const state = this.orders.get(orderNo);
    if (!state || state.status !== 'RESERVED') return;
    state.status = 'PAID';
    this.stats.ordersPaid++;
    for (const l of state.lines) {
      const inv = this.inventory.get(l.skuId);
      if (inv) inv.available = Math.max(0, inv.available - l.qty);
      state.soldCache += l.qty;
    }
    this.pushEvent(orderNo, 'ORDER_PAID', 2);
  }

  cancelOrder(orderNo: string): void {
    const state = this.orders.get(orderNo);
    if (!state || state.status === 'CANCELLED' || state.status === 'PAID' || state.status === 'REJECTED') return;
    state.status = 'CANCELLED';
    this.stats.ordersCancelled++;
    this.pushEvent(orderNo, 'ORDER_CANCELLED', 3);
  }

  /** 走平台自己的 HTTP 入口下单（经过限流/超时/5xx 层），模拟真实用户请求 */
  async httpPlaceOrder(
    lines: OrderLine[],
    opts: { payProb?: number; payDelayMs?: number; cancelProb?: number; retries?: number } = {},
  ): Promise<{ status: number; orderNo?: string; state?: string; error?: string; retried?: number }> {
    const retries = opts.retries ?? 2;
    let last: { status: number; error?: string } = { status: 0, error: 'UNKNOWN' };
    for (let attempt = 0; attempt <= retries; attempt++) {
      const controller = new AbortController();
      const timer = setTimeout(() => controller.abort(), 3000);
      try {
        const res = await fetch(`${this.baseUrl}/orders`, {
          method: 'POST',
          headers: { 'content-type': 'application/json' },
          body: JSON.stringify({ lines, payProb: opts.payProb, payDelayMs: opts.payDelayMs, cancelProb: opts.cancelProb }),
          signal: controller.signal,
        });
        const text = await res.text();
        let body: { ok?: boolean; orderNo?: string; status?: string; error?: string; code?: string } = {};
        try {
          body = JSON.parse(text);
        } catch {
          /* ignore */
        }
        if (res.ok) return { status: res.status, orderNo: body.orderNo, state: body.status, retried: attempt };
        last = { status: res.status, error: body.code ?? body.error ?? `HTTP_${res.status}` };
        // 429/5xx/超时：用户侧会重试；4xx 其余错误直接失败
        if (res.status !== 429 && res.status < 500) return { ...last, retried: attempt };
      } catch (err) {
        last = { status: 0, error: (err as Error).name === 'AbortError' ? 'TIMEOUT' : 'NETWORK' };
      } finally {
        clearTimeout(timer);
      }
      await sleep(20 + attempt * 40);
    }
    return { ...last, retried: retries };
  }

  // -------------------------------------------------------------------------
  // HTTP 接口
  // -------------------------------------------------------------------------

  private async chaosLayer(req: IncomingMessage, res: ServerResponse): Promise<boolean> {
    this.stats.httpRequests++;
    if (!this.bucket.take()) {
      this.stats.httpRateLimited++;
      res.setHeader('retry-after', '1');
      json(res, 429, { ok: false, code: 'RATE_LIMITED', error: '请求过于频繁' });
      return false;
    }
    if (Math.random() < this.chaos.timeoutRate) {
      this.stats.httpTimeouts++;
      // hang 住：客户端超时，但服务端稍后仍可能处理完（真实超时语义）
      await sleep(this.chaos.timeoutHangMs);
    } else {
      await sleep(rand(this.chaos.latencyMinMs, this.chaos.latencyMaxMs));
    }
    if (Math.random() < this.chaos.errorRate) {
      this.stats.httpErrors++;
      json(res, 503, { ok: false, code: 'UPSTREAM_BUSY', error: '平台繁忙' });
      return false;
    }
    if (Math.random() < this.chaos.busyRate) {
      this.stats.httpErrors++;
      json(res, 500, { ok: false, code: 'INTERNAL', error: '内部错误' });
      return false;
    }
    return true;
  }

  private async handle(req: IncomingMessage, res: ServerResponse): Promise<void> {
    if (!(await this.chaosLayer(req, res))) return;
    const url = new URL(req.url ?? '/', 'http://localhost');
    const path = url.pathname;
    const method = req.method ?? 'GET';

    if (method === 'POST' && path === '/inventory') {
      const body = JSON.parse(await readBody(req)) as {
        skuId: string; available: number; version: number; idempotencyKey: string;
      };
      this.stats.inventoryPushes++;
      if (this.idempotency.has(body.idempotencyKey)) {
        this.stats.inventoryDuplicateKey++;
        json(res, 200, { ok: true, duplicated: true, version: this.idempotency.get(body.idempotencyKey)!.version });
        return;
      }
      const existing = this.inventory.get(body.skuId);
      if (existing && Number(body.version) <= existing.version) {
        this.stats.inventoryStaleIgnored++;
        this.idempotency.set(body.idempotencyKey, { version: Number(body.version), available: Number(body.available), at: Date.now() });
        json(res, 200, { ok: true, applied: false, staleVersion: true, version: existing.version, available: existing.available });
        return;
      }
      this.inventory.set(body.skuId, { available: Number(body.available), version: Number(body.version), updatedAt: Date.now() });
      this.idempotency.set(body.idempotencyKey, { version: Number(body.version), available: Number(body.available), at: Date.now() });
      this.stats.inventoryApplied++;

      // 平台把货卖出去一部分后，缓存又下降（模拟平台缓存独立演化）
      json(res, 200, { ok: true, applied: true, version: body.version, available: body.available });
      return;
    }

    if (method === 'GET' && path === '/inventory') {
      const skuId = url.searchParams.get('skuId') ?? '';
      const inv = this.inventory.get(skuId);
      json(res, 200, { ok: true, skuId, available: inv?.available ?? 0, version: inv?.version ?? 0 });
      return;
    }

    if (method === 'GET' && path === '/stats') {
      json(res, 200, { ok: true, stats: this.stats, chaos: this.chaos, inventory: [...this.inventory.entries()] });
      return;
    }

    if (method === 'GET' && path === '/orders') {
      json(res, 200, {
        ok: true,
        orders: this.orderList.map((o) => ({ orderNo: o.orderNo, orderId: o.orderId, status: o.status, paid: o.status === 'PAID' })),
      });
      return;
    }

    if (method === 'POST' && path === '/orders') {
      // 压测驱动用：模拟一位用户一键下单
      const body = JSON.parse(await readBody(req)) as {
        lines: OrderLine[]; payProb?: number; payDelayMs?: number; cancelProb?: number;
      };
      const order = await this.placeOrder(body.lines, {
        payProb: body.payProb, payDelayMs: body.payDelayMs, cancelProb: body.cancelProb,
      });
      json(res, 202, { ok: true, orderNo: order.orderNo, status: order.status });
      return;
    }

    if (method === 'POST' && path === '/chaos') {
      const body = JSON.parse(await readBody(req)) as Partial<ChaosConfig>;
      Object.assign(this.chaos, body);
      json(res, 200, { ok: true, chaos: this.chaos });
      return;
    }

    json(res, 404, { ok: false, error: `未知接口 ${method} ${path}` });
  }

  // -------------------------------------------------------------------------
  // 事件推送：重复 + 乱序 + 首投失败重试
  // -------------------------------------------------------------------------

  private pushEvent(orderNo: string, type: OrderEventType, seq: number, lines?: OrderLine[]): void {
    const event = {
      messageId: `${this.config.shopId}:${orderNo}:${seq}:${Math.random().toString(36).slice(2, 10)}`,
      channel: this.config.channel,
      shopId: this.config.shopId,
      externalOrderNo: orderNo,
      type,
      seq,
      occurredAt: Date.now(),
      lines,
    };
    const baseDelay = Math.random() < this.chaos.reorderRate
      ? rand(this.chaos.reorderMinDelayMs, this.chaos.reorderMaxDelayMs)
      : rand(0, 12);

    this.schedule(() => void this.deliver(event, 0), baseDelay);
    if (Math.random() < this.chaos.duplicateRate) {
      // 平台重复推送同一条事件（常见于重试/幂等键失效）
      this.schedule(() => {
        this.stats.webhookDuplicates++;
        void this.deliver(event, 2);
      }, baseDelay + rand(10, 120));
    }
  }

  private async deliver(event: unknown, attempt: number): Promise<void> {
    this.stats.webhookDeliveries++;
    this.inflight++;
    try {
      await this.deliverInner(event, attempt);
    } finally {
      this.inflight--;
    }
  }

  private async deliverInner(event: unknown, attempt: number): Promise<void> {
    if (attempt === 0 && Math.random() < this.chaos.pushFailRate) {
      this.stats.webhookFailures++;
      this.stats.webhookRetries++;
      this.schedule(() => void this.deliver(event, attempt + 1), rand(60, 250));
      return;
    }
    const controller = new AbortController();
    const timer = setTimeout(() => controller.abort(), 2500);
    try {
      const res = await fetch(`${this.config.hubBaseUrl}/webhooks/orders`, {
        method: 'POST',
        headers: { 'content-type': 'application/json' },
        body: JSON.stringify(event),
        signal: controller.signal,
      });
      await res.text();
      if (res.status >= 500) throw new Error(`HUB_${res.status}`);
    } catch {
      this.stats.webhookFailures++;
      if (attempt < 5) {
        this.stats.webhookRetries++;
        this.schedule(() => void this.deliver(event, attempt + 1), rand(80, 400));
      }
    } finally {
      clearTimeout(timer);
    }
  }
}
