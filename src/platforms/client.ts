import { setTimeout as sleep } from 'node:timers/promises';

export interface PushOutcome {
  ok: boolean;
  duplicated?: boolean;
  stale?: boolean;
  error?: string;
}

/**
 * 向模拟平台推库存。真实世界会遇到：超时、429 限流、503、连接重置。
 * 这里把网络层异常统一翻译成 {ok:false, error}，由 SyncWorker 决定退避重试。
 */
export function makeHttpInventoryPusher(timeoutMs = 1500) {
  return async (job: {
    channel: string;
    shopId: string;
    skuId: string;
    version: number;
    available: number;
    idempotencyKey: string;
  }): Promise<PushOutcome> => {
    const baseUrl = shopBaseUrls.get(job.shopId);
    if (!baseUrl) return { ok: false, error: `店铺 ${job.shopId} 未配置平台地址` };
    const controller = new AbortController();
    const timer = setTimeout(() => controller.abort(), timeoutMs);
    try {
      const res = await fetch(`${baseUrl}/inventory`, {
        method: 'POST',
        headers: { 'content-type': 'application/json' },
        body: JSON.stringify({
          skuId: job.skuId,
          available: job.available,
          version: job.version,
          idempotencyKey: job.idempotencyKey,
        }),
        signal: controller.signal,
      });
      const text = await res.text();
      let body: Record<string, unknown> = {};
      try {
        body = JSON.parse(text) as Record<string, unknown>;
      } catch {
        /* 非 JSON 响应 */
      }
      if (res.status === 429) return { ok: false, error: 'RATE_LIMITED' };
      if (!res.ok) return { ok: false, error: `HTTP_${res.status}:${String(body.error ?? text.slice(0, 80))}` };
      return {
        ok: true,
        duplicated: body.duplicated === true,
        stale: body.staleVersion === true,
      };
    } catch (err) {
      const e = err as Error;
      if (e.name === 'AbortError') return { ok: false, error: 'TIMEOUT' };
      return { ok: false, error: `NETWORK:${e.message}` };
    } finally {
      clearTimeout(timer);
    }
  };
}

const shopBaseUrls = new Map<string, string>();

/** 注册店铺对应的平台地址（库存推送用） */
export function registerShopBaseUrl(shopId: string, baseUrl: string): void {
  shopBaseUrls.set(shopId, baseUrl);
}

/** 平台调用中台的地址（下单预占用） */
let hubBase = '';
export function setHubBaseUrl(url: string): void {
  hubBase = url;
}

export interface HubReserveResponse {
  ok: boolean;
  orderId?: string;
  status?: string;
  reason?: string;
  code?: string;
}

/**
 * 平台侧同步调用中台的预占接口（失败的渠道会触发平台重试或拒单）。
 * 这是渠道接中台的标准做法：下单时同步预占，异步收支付/取消消息。
 */
export async function reserveAtHub(
  body: { shopId: string; externalOrderNo: string; lines: Array<{ skuId: string; qty: number }> },
  opts: { baseUrl?: string; timeoutMs?: number; retries?: number } = {},
): Promise<HubReserveResponse> {
  const base = opts.baseUrl ?? hubBase;
  const retries = opts.retries ?? 2;
  const timeoutMs = opts.timeoutMs ?? 1500;
  let last: HubReserveResponse = { ok: false, reason: 'NO_BASE_URL' };
  for (let attempt = 0; attempt <= retries; attempt++) {
    const controller = new AbortController();
    const timer = setTimeout(() => controller.abort(), timeoutMs);
    try {
      const res = await fetch(`${base}/api/orders`, {
        method: 'POST',
        headers: { 'content-type': 'application/json' },
        body: JSON.stringify(body),
        signal: controller.signal,
      });
      const data = (await res.json()) as HubReserveResponse;
      if (res.status === 201 || (res.status === 200 && data.ok)) return data;
      if (res.status === 409) return { ...data, ok: false, reason: data.reason ?? 'SOLD_OUT' };
      if (res.status === 429 || res.status >= 500) {
        last = { ok: false, reason: `HUB_${res.status}` };
      } else {
        return { ...data, ok: false, reason: data.reason ?? `HUB_${res.status}` };
      }
    } catch (err) {
      last = { ok: false, reason: (err as Error).name === 'AbortError' ? 'HUB_TIMEOUT' : 'HUB_NETWORK' };
    } finally {
      clearTimeout(timer);
    }
    await sleep(30 + attempt * 60);
  }
  return last;
}
