import { type DB, all, get, run, tx } from './db.js';
import {
  applyOnHandDelta,
  bundleComponents,
  desiredAllocation,
  listShops,
  channelUnreserved,
  getOnHand,
  getSku,
  markSkuChanged,
  sharedPoolQty,
  sharedReserved,
  sharedUnreserved,
  skuSellable,
  writeLedger,
} from './catalog.js';
import { raiseAlert } from './alerts.js';
import type { ChannelCode, OrderLine } from './types.js';

export class OrderValidationError extends Error {}

export interface CreateOrderInput {
  orderId?: string;
  shopId: string;
  channel: ChannelCode;
  externalOrderNo: string;
  lines: OrderLine[];
  idempotencyKey?: string;
  now?: number;
}

export interface ReserveResult {
  ok: boolean;
  orderId: string;
  status: 'RESERVED' | 'FAILED' | 'EXISTING';
  reason?: string;
  duplicated?: boolean;
}

export interface OrderView {
  id: string;
  channel: string;
  shopId: string;
  externalOrderNo: string;
  status: string;
  rejectReason: string | null;
  totalQty: number;
  reservedQty: number;
  paidQty: number;
  createdAt: number;
  paidAt: number | null;
  closedAt: number | null;
  version: number;
  items: Array<{ requestedSkuId: string; componentSkuId: string; qty: number }>;
  reservations: Array<{ skuId: string; qty: number; source: string; state: string }>;
}

type ReservationDraft = { skuId: string; qty: number; source: 'CHANNEL' | 'SHARED' };

export function newOrderId(externalOrderNo: string, channel: string, shopId: string): string {
  return `O-${channel}-${shopId}-${externalOrderNo}`;
}

/** 把订单行按套装展开成“实际占用库存的子件” */
export function explodeLines(
  db: DB,
  lines: OrderLine[],
): Array<{ requestedSkuId: string; componentSkuId: string; qty: number }> {
  const out = new Map<string, { requestedSkuId: string; componentSkuId: string; qty: number }>();
  for (const line of lines) {
    if (!Number.isInteger(line.qty) || line.qty <= 0) throw new OrderValidationError(`数量非法: ${line.skuId}=${line.qty}`);
    const sku = getSku(db, line.skuId);
    if (!sku) throw new OrderValidationError(`SKU 不存在: ${line.skuId}`);
    const parts = sku.isBundle
      ? bundleComponents(db, line.skuId).map((c) => ({ componentSkuId: c.skuId, qty: c.qty * line.qty }))
      : [{ componentSkuId: line.skuId, qty: line.qty }];
    if (parts.length === 0) throw new OrderValidationError(`套装无子件配置: ${line.skuId}`);
    for (const p of parts) {
      const existing = out.get(p.componentSkuId);
      if (existing) existing.qty += p.qty;
      else out.set(p.componentSkuId, { requestedSkuId: line.skuId, componentSkuId: p.componentSkuId, qty: p.qty });
    }
  }
  return [...out.values()];
}

export function getOrderByExternal(db: DB, channel: string, shopId: string, externalOrderNo: string) {
  return get<{ id: string; status: string; reject_reason: string | null; shop_id: string; channel: string }>(
    db,
    'SELECT id, status, reject_reason, shop_id, channel FROM orders WHERE channel = ? AND shop_id = ? AND external_order_no = ?',
    channel,
    shopId,
    externalOrderNo,
  );
}

export function getOrderView(db: DB, orderId: string): OrderView | undefined {
  const order = get<{
    id: string; channel: string; shop_id: string; external_order_no: string; status: string;
    reject_reason: string | null; total_qty: number; reserved_qty: number; paid_qty: number;
    created_at: number; paid_at: number | null; closed_at: number | null; version: number;
  }>(db, 'SELECT * FROM orders WHERE id = ?', orderId);
  if (!order) return undefined;
  return {
    id: order.id,
    channel: order.channel,
    shopId: order.shop_id,
    externalOrderNo: order.external_order_no,
    status: order.status,
    rejectReason: order.reject_reason,
    totalQty: Number(order.total_qty),
    reservedQty: Number(order.reserved_qty),
    paidQty: Number(order.paid_qty),
    createdAt: Number(order.created_at),
    paidAt: order.paid_at === null ? null : Number(order.paid_at),
    closedAt: order.closed_at === null ? null : Number(order.closed_at),
    version: Number(order.version),
    items: all<{ requested_sku_id: string; component_sku_id: string; qty: number }>(
      db, 'SELECT requested_sku_id, component_sku_id, qty FROM order_items WHERE order_id = ?', orderId,
    ).map((r) => ({ requestedSkuId: r.requested_sku_id, componentSkuId: r.component_sku_id, qty: Number(r.qty) })),
    reservations: all<{ sku_id: string; qty: number; source: string; state: string }>(
      db, 'SELECT sku_id, qty, source, state FROM reservations WHERE order_id = ?', orderId,
    ).map((r) => ({ skuId: r.sku_id, qty: Number(r.qty), source: r.source, state: r.state })),
  };
}

export class InventoryEngine {
  constructor(readonly db: DB) {}

  /**
   * 下单预占：渠道分配优先，不足借共享池；全部行都能占到才成功。
   * 以 (channel, shopId, externalOrderNo) 做幂等：重复请求返回同一订单，不会重复预占。
   */
  reserve(input: CreateOrderInput): ReserveResult {
    const db = this.db;
    const now = input.now ?? Date.now();
    const items = explodeLines(db, input.lines);
    if (items.length === 0) throw new OrderValidationError('订单行不能为空');
    const orderId = input.orderId ?? newOrderId(input.externalOrderNo, input.channel, input.shopId);

    return tx(db, () => {
      const existing = getOrderByExternal(db, input.channel, input.shopId, input.externalOrderNo);
      if (existing) {
        if (existing.status === 'FAILED') {
          return { ok: false, orderId: existing.id, status: 'FAILED' as const, reason: existing.reject_reason ?? '预占失败', duplicated: true };
        }
        return { ok: true, orderId: existing.id, status: 'EXISTING' as const, duplicated: true };
      }

      const drafts: ReservationDraft[] = [];
      for (const item of items) {
        const need = item.qty;
        const own = channelUnreserved(db, input.shopId, item.componentSkuId);
        const takeOwn = Math.min(own, need);
        if (takeOwn > 0) drafts.push({ skuId: item.componentSkuId, qty: takeOwn, source: 'CHANNEL' });
        const rest = need - takeOwn;
        if (rest > 0) {
          const borrow = sharedUnreserved(db, item.componentSkuId);
          if (borrow < rest) {
            this.insertRejectedOrder(db, input, orderId, items, `库存不足: ${item.componentSkuId} 需 ${need} 可用 ${own + borrow}`, now);
            raiseAlert(db, {
              type: 'RESERVE_REJECT',
              skuId: item.componentSkuId,
              shopId: input.shopId,
              severity: 'WARN',
              message: `渠道 ${input.shopId} 订单 ${input.externalOrderNo} 库存不足（需 ${need}，可 ${own + borrow}），已拒单`,
              dedupeKey: `reject:${input.shopId}:${item.componentSkuId}`,
            });
            return { ok: false, orderId, status: 'FAILED' as const, reason: `库存不足: ${item.componentSkuId}` };
          }
          drafts.push({ skuId: item.componentSkuId, qty: rest, source: 'SHARED' });
        }
      }

      const totalQty = items.reduce((s, i) => s + i.qty, 0);
      run(
        db,
        `INSERT INTO orders(id, channel, shop_id, external_order_no, status, reject_reason, created_at, paid_at, closed_at,
           version, total_qty, reserved_qty, paid_qty, idempotency_key, event_seq)
         VALUES(?,?,?,?,'RESERVED',NULL,?,NULL,NULL,1,?,?,0,?,0)`,
        orderId, input.channel, input.shopId, input.externalOrderNo, now, totalQty, totalQty,
        input.idempotencyKey ?? `${input.channel}:${input.shopId}:${input.externalOrderNo}`,
      );
      for (const item of items) {
        run(
          db,
          'INSERT INTO order_items(order_id, requested_sku_id, component_sku_id, qty) VALUES(?,?,?,?)',
          orderId, item.requestedSkuId, item.componentSkuId, item.qty,
        );
      }

      for (const d of drafts) {
        if (d.source === 'CHANNEL') {
          run(db, 'UPDATE allocations SET reserved = reserved + ? WHERE shop_id = ? AND sku_id = ?', d.qty, input.shopId, d.skuId);
        } else {
          run(
            db,
            'INSERT INTO shared(sku_id, reserved, sold) VALUES(?,?,0) ON CONFLICT(sku_id) DO UPDATE SET reserved = reserved + excluded.reserved',
            d.skuId, d.qty,
          );
        }
        run(
          db,
          'INSERT INTO reservations(order_id, sku_id, qty, source, shop_id, state, created_at, updated_at) VALUES(?,?,?,?,?,?,?,?)',
          orderId, d.skuId, d.qty, d.source, input.shopId, 'ACTIVE', now, now,
        );
        writeLedger(db, {
          skuId: d.skuId,
          channel: input.channel,
          shopId: d.source === 'CHANNEL' ? input.shopId : null,
          reason: 'RESERVE',
          deltaReserved: d.qty,
          refType: 'ORDER',
          refId: orderId,
          note: d.source === 'CHANNEL' ? '渠道分配内预占' : '共享池预占',
        });
      }

      for (const item of items) markSkuChanged(db, item.componentSkuId, 'reserve');
      return { ok: true, orderId, status: 'RESERVED' as const };
    });
  }

  private insertRejectedOrder(
    db: DB,
    input: CreateOrderInput,
    orderId: string,
    items: Array<{ requestedSkuId: string; componentSkuId: string; qty: number }>,
    reason: string,
    now: number,
  ): void {
    run(
      db,
      `INSERT INTO orders(id, channel, shop_id, external_order_no, status, reject_reason, created_at, paid_at, closed_at,
         version, total_qty, reserved_qty, paid_qty, idempotency_key, event_seq)
       VALUES(?,?,?,?,'FAILED',?,?,NULL,NULL,1,?,0,0,?,0)`,
      orderId, input.channel, input.shopId, input.externalOrderNo, reason, now,
      items.reduce((s, i) => s + i.qty, 0),
      input.idempotencyKey ?? `${input.channel}:${input.shopId}:${input.externalOrderNo}`,
    );
    for (const item of items) {
      run(
        db,
        'INSERT INTO order_items(order_id, requested_sku_id, component_sku_id, qty) VALUES(?,?,?,?) ON CONFLICT(order_id, component_sku_id) DO NOTHING',
        orderId, item.requestedSkuId, item.componentSkuId, item.qty,
      );
    }
  }

  /**
   * 付款：预占转实扣。物理库存减少、渠道分配与预占同步减少、sold 增加。
   * 幂等：已支付订单重复调用直接返回 alreadyPaid。
   */
  pay(orderId: string, now = Date.now()): { ok: boolean; reason?: string; alreadyPaid?: boolean } {
    const db = this.db;
    return tx(db, () => {
      const order = get<{ id: string; status: string; shop_id: string; channel: string }>(
        db, 'SELECT id, status, shop_id, channel FROM orders WHERE id = ?', orderId,
      );
      if (!order) return { ok: false, reason: '订单不存在' };
      if (order.status === 'PAID') return { ok: true, alreadyPaid: true };
      if (order.status !== 'RESERVED') return { ok: false, reason: `订单状态 ${order.status} 不可支付` };

      const reservations = all<{ id: number; sku_id: string; qty: number; source: string }>(
        db, 'SELECT id, sku_id, qty, source FROM reservations WHERE order_id = ? AND state = ?', orderId, 'ACTIVE',
      );
      if (reservations.length === 0) return { ok: false, reason: '无有效预占' };

      let paidQty = 0;
      for (const r of reservations) {
        const skuId = r.sku_id;
        const qty = Number(r.qty);
        paidQty += qty;

        if (r.source === 'CHANNEL') {
          const row = get<{ allocated: number; reserved: number; sold: number }>(
            db, 'SELECT allocated, reserved, sold FROM allocations WHERE shop_id = ? AND sku_id = ?', order.shop_id, skuId,
          );
          if (!row || Number(row.allocated) < qty || Number(row.reserved) < qty) {
            // 防御：正常情况下 allocated >= reserved 恒成立（分配、盘点、收紧都保证）。
            // 若出现不一致，渠道能出多少出多少，缺口由共享池承担，保持守恒：
            //   onHand 减少 qty，渠道 allocated/sold 各变动 effective，其余走共享池 sold。
            const allocatedNow = row ? Number(row.allocated) : 0;
            const reservedNow = row ? Number(row.reserved) : 0;
            const fromChannel = Math.max(0, Math.min(qty, allocatedNow, reservedNow));
            const fromShared = qty - fromChannel;
            this.recordOversell(
              db, orderId, skuId, fromShared,
              `渠道分配不足（allocated=${allocatedNow}, reserved=${reservedNow}, need=${qty}），${fromShared} 件由共享池承担`,
            );
            if (fromChannel > 0) {
              run(
                db,
                'UPDATE allocations SET allocated = allocated - ?, reserved = reserved - ?, sold = sold + ? WHERE shop_id = ? AND sku_id = ?',
                fromChannel, fromChannel, fromChannel, order.shop_id, skuId,
              );
              writeLedger(db, {
                skuId, channel: order.channel, shopId: order.shop_id, reason: 'PAID',
                deltaReserved: -fromChannel, deltaAllocated: -fromChannel, deltaSold: fromChannel,
                refType: 'ORDER', refId: orderId, note: '支付扣减渠道分配（降级）',
              });
            }
            const leftoverReserved = reservedNow - fromChannel;
            if (leftoverReserved > 0) {
              run(db, 'UPDATE allocations SET reserved = reserved - ? WHERE shop_id = ? AND sku_id = ?', leftoverReserved, order.shop_id, skuId);
              writeLedger(db, {
                skuId, channel: order.channel, shopId: order.shop_id, reason: 'RELEASE',
                deltaReserved: -leftoverReserved, refType: 'ORDER', refId: orderId,
                note: '预占与分配不一致，清理渠道预占',
              });
            }
            if (fromShared > 0) {
              run(
                db,
                'INSERT INTO shared(sku_id, reserved, sold) VALUES(?,0,?) ON CONFLICT(sku_id) DO UPDATE SET sold = sold + excluded.sold',
                skuId, fromShared,
              );
              writeLedger(db, {
                skuId, channel: 'SHARED', reason: 'PAID', deltaSold: fromShared,
                refType: 'ORDER', refId: orderId, note: '支付扣减共享池（渠道分配不足）',
              });
            }
            applyOnHandDelta(db, skuId, -qty, {
              channel: order.channel, shopId: order.shop_id, reason: 'PAID',
              refType: 'ORDER', refId: orderId, note: '支付出库（渠道分配不足，降级）',
            });
            run(db, 'UPDATE reservations SET state = ?, updated_at = ? WHERE id = ?', 'PAID', now, r.id);
            continue;
          }
          run(
            db,
            'UPDATE allocations SET allocated = allocated - ?, reserved = reserved - ?, sold = sold + ? WHERE shop_id = ? AND sku_id = ?',
            qty, qty, qty, order.shop_id, skuId,
          );
          applyOnHandDelta(db, skuId, -qty, { channel: order.channel, shopId: order.shop_id, reason: 'PAID', refType: 'ORDER', refId: orderId, note: '支付出库（渠道分配）' });
          writeLedger(db, {
            skuId, channel: order.channel, shopId: order.shop_id, reason: 'PAID',
            deltaReserved: -qty, deltaAllocated: -qty, deltaSold: qty,
            refType: 'ORDER', refId: orderId, note: '支付扣减渠道分配',
          });
        } else {
          const pool = sharedPoolQty(db, skuId);
          const reserved = sharedReserved(db, skuId);
          if (pool < qty || reserved < qty) {
            this.recordOversell(db, orderId, skuId, qty, `共享池不足（pool=${pool}, reserved=${reserved}, need=${qty}）`);
          }
          run(db, 'UPDATE shared SET reserved = MAX(0, reserved - ?), sold = sold + ? WHERE sku_id = ?', qty, qty, skuId);
          applyOnHandDelta(db, skuId, -qty, { channel: order.channel, reason: 'PAID', refType: 'ORDER', refId: orderId, note: '支付出库（共享池）' });
          writeLedger(db, {
            skuId, channel: 'SHARED', reason: 'PAID', deltaReserved: -qty, deltaSold: qty,
            refType: 'ORDER', refId: orderId, note: '支付扣减共享池',
          });
        }
        run(db, 'UPDATE reservations SET state = ?, updated_at = ? WHERE id = ?', 'PAID', now, r.id);
      }

      run(db, 'UPDATE orders SET status = ?, paid_at = ?, paid_qty = ?, version = version + 1 WHERE id = ?', 'PAID', now, paidQty, orderId);
      for (const r of reservations) {
        markSkuChanged(db, r.sku_id, 'pay');
        // 支付后实物下降，立即检查安全线，不等后台定时器
        this.tightenSkuIfBelowSafety(r.sku_id);
      }
      return { ok: true };
    });
  }

  /**
   * 记录一次超卖。qty 是真实无法履约的件数（真正卖出去但无货可发）；
   * 渠道分配与实物不一致但最终由共享池/账面兜住的情况记为 OVERSOLD 类事件，
   * 因为存在支付成功却无货的风险。
   */
  private recordOversell(db: DB, orderId: string, skuId: string, qty: number, reason: string): void {
    if (qty <= 0) return;
    run(
      db,
      'INSERT INTO oversold_events(order_id, sku_id, qty, reason, severity, created_at) VALUES(?,?,?,?,?,?)',
      orderId, skuId, qty, reason, 'OVERSOLD', Date.now(),
    );
    raiseAlert(db, {
      type: 'OVERSOLD', skuId, severity: 'CRITICAL',
      message: `订单 ${orderId} 在 ${skuId} 出现 ${qty} 件超卖：${reason}`,
      dedupeKey: `oversold:${orderId}:${skuId}`,
    });
  }

  /** 释放预占（超时未付/平台取消）——只回滚预占，不影响实物 */
  release(orderId: string, reason: string, now = Date.now()): { ok: boolean; reason?: string; alreadyReleased?: boolean } {
    const db = this.db;
    return tx(db, () => {
      const order = get<{ id: string; status: string; shop_id: string; channel: string }>(
        db, 'SELECT id, status, shop_id, channel FROM orders WHERE id = ?', orderId,
      );
      if (!order) return { ok: false, reason: '订单不存在' };
      if (order.status === 'RELEASED' || order.status === 'FAILED' || order.status === 'CANCELLED') {
        return { ok: true, alreadyReleased: true };
      }
      if (order.status === 'PAID') return { ok: false, reason: '已支付订单不能释放，请走退款' };

      const reservations = all<{ id: number; sku_id: string; qty: number; source: string }>(
        db, 'SELECT id, sku_id, qty, source FROM reservations WHERE order_id = ? AND state = ?', orderId, 'ACTIVE',
      );
      for (const r of reservations) {
        const skuId = r.sku_id;
        const qty = Number(r.qty);
        if (r.source === 'CHANNEL') {
          run(db, 'UPDATE allocations SET reserved = MAX(0, reserved - ?) WHERE shop_id = ? AND sku_id = ?', qty, order.shop_id, skuId);
          writeLedger(db, {
            skuId, channel: order.channel, shopId: order.shop_id, reason: 'RELEASE',
            deltaReserved: -qty, refType: 'ORDER', refId: orderId, note: reason,
          });
        } else {
          const had = Math.min(qty, sharedReserved(db, skuId));
          run(db, 'UPDATE shared SET reserved = MAX(0, reserved - ?) WHERE sku_id = ?', qty, skuId);
          // 释放后共享池出现负值时，说明渠道分配占满了实物：把共享池预占对应的量从渠道分配回拨
          const pool = sharedPoolQty(db, skuId);
          if (pool < 0) {
            run(db, 'UPDATE allocations SET allocated = allocated + ? WHERE sku_id = ? AND allocated = (SELECT MAX(allocated) FROM allocations WHERE sku_id = ?)', pool, skuId, skuId);
          }
          writeLedger(db, {
            skuId, channel: 'SHARED', reason: 'RELEASE', deltaReserved: -had,
            refType: 'ORDER', refId: orderId, note: reason,
          });
        }
        run(db, 'UPDATE reservations SET state = ?, updated_at = ? WHERE id = ?', 'RELEASED', now, r.id);
        markSkuChanged(db, skuId, 'release');
      }
      run(db, 'UPDATE orders SET status = ?, closed_at = ?, version = version + 1 WHERE id = ?', 'RELEASED', now, orderId);
      return { ok: true };
    });
  }

  /** 支付后退款：实物回补，退款原路回渠道分配；渠道停用则回共享池（抵扣共享池 sold） */
  refund(orderId: string, now = Date.now()): { ok: boolean; reason?: string } {
    const db = this.db;
    return tx(db, () => {
      const order = get<{ id: string; status: string; shop_id: string; channel: string }>(
        db, 'SELECT id, status, shop_id, channel FROM orders WHERE id = ?', orderId,
      );
      if (!order) return { ok: false, reason: '订单不存在' };
      if (order.status === 'CANCELLED') return { ok: true };
      if (order.status !== 'PAID') return { ok: false, reason: `订单状态 ${order.status} 不可退款` };

      const reservations = all<{ id: number; sku_id: string; qty: number; source: string }>(
        db, 'SELECT id, sku_id, qty, source FROM reservations WHERE order_id = ? AND state = ?', orderId, 'PAID',
      );
      const shopActive = get<{ active: number }>(db, 'SELECT active FROM shops WHERE id = ?', order.shop_id)?.active === 1;
      for (const r of reservations) {
        const skuId = r.sku_id;
        const qty = Number(r.qty);
        applyOnHandDelta(db, skuId, qty, { channel: order.channel, shopId: order.shop_id, reason: 'REFUND', refType: 'ORDER', refId: orderId, note: '退款回补实物' });
        if (r.source === 'CHANNEL' && shopActive) {
          run(
            db,
            'UPDATE allocations SET allocated = allocated + ?, sold = MAX(0, sold - ?) WHERE shop_id = ? AND sku_id = ?',
            qty, qty, order.shop_id, skuId,
          );
          writeLedger(db, {
            skuId, channel: order.channel, shopId: order.shop_id, reason: 'REFUND',
            deltaAllocated: qty, deltaSold: -qty, refType: 'ORDER', refId: orderId, note: '退款回补渠道分配',
          });
        } else {
          run(db, 'UPDATE shared SET sold = MAX(0, sold - ?) WHERE sku_id = ?', qty, skuId);
          writeLedger(db, {
            skuId, channel: 'SHARED', reason: 'REFUND', deltaSold: -qty,
            refType: 'ORDER', refId: orderId, note: '退款回补共享池',
          });
        }
        run(db, 'UPDATE reservations SET state = ?, updated_at = ? WHERE id = ?', 'REFUNDED', now, r.id);
        markSkuChanged(db, skuId, 'refund');
      }
      run(db, 'UPDATE orders SET status = ?, closed_at = ?, version = version + 1 WHERE id = ?', 'CANCELLED', now, orderId);
      return { ok: true };
    });
  }

  /**
   * 预占已被超时释放，但平台随后确认了付款（消息延迟/网络抖动）。
   * 正确做法是“复活”：按原订单行重新预占并扣减；库存已不足则是真超卖，必须记账告警。
   * 返回 revived=false 表示无法复活，已登记超卖事件。
   */
  reviveAndPay(orderId: string, now = Date.now()): { revived: boolean; reason?: string } {
    const db = this.db;
    return tx(db, () => {
      const order = get<{ id: string; status: string; shop_id: string; channel: string; external_order_no: string }>(
        db, 'SELECT id, status, shop_id, channel, external_order_no FROM orders WHERE id = ?', orderId,
      );
      if (!order) return { revived: false, reason: '订单不存在' };
      if (order.status === 'PAID') return { revived: true };
      if (order.status !== 'RELEASED' && order.status !== 'CANCELLED') return { revived: false, reason: `状态 ${order.status} 不可复活` };

      const items = all<{ requested_sku_id: string; component_sku_id: string; qty: number }>(
        db, 'SELECT requested_sku_id, component_sku_id, qty FROM order_items WHERE order_id = ?', orderId,
      );
      const need = items.map((i) => ({ skuId: i.component_sku_id, qty: Number(i.qty) }));

      // 重新预占：渠道余量优先，其次共享池
      const drafts: Array<{ skuId: string; qty: number; source: 'CHANNEL' | 'SHARED' }> = [];
      for (const item of need) {
        const own = Math.min(channelUnreserved(db, order.shop_id, item.skuId), item.qty);
        const rest = item.qty - own;
        const borrow = rest > 0 ? sharedUnreserved(db, item.skuId) : 0;
        if (rest > borrow) {
          this.recordOversell(
            db, orderId, item.skuId, item.qty,
            `付款到达时预占已超时释放且库存不足（需 ${item.qty}，渠道余量 ${own}，共享池余量 ${borrow}）`,
          );
          run(db, 'UPDATE orders SET status = ?, reject_reason = ? WHERE id = ?', 'FAILED', '付款后无库存，已登记超卖', orderId);
          return { revived: false, reason: '库存不足' };
        }
        if (own > 0) drafts.push({ skuId: item.skuId, qty: own, source: 'CHANNEL' });
        if (rest > 0) drafts.push({ skuId: item.skuId, qty: rest, source: 'SHARED' });
      }

      for (const d of drafts) {
        if (d.source === 'CHANNEL') {
          run(db, 'UPDATE allocations SET allocated = allocated + ?, reserved = reserved + ? WHERE shop_id = ? AND sku_id = ?', d.qty, d.qty, order.shop_id, d.skuId);
        } else {
          run(db, 'INSERT INTO shared(sku_id, reserved, sold) VALUES(?,?,0) ON CONFLICT(sku_id) DO UPDATE SET reserved = reserved + excluded.reserved', d.skuId, d.qty);
        }
        run(
          db,
          'INSERT INTO reservations(order_id, sku_id, qty, source, shop_id, state, created_at, updated_at) VALUES(?,?,?,?,?,?,?,?)',
          orderId, d.skuId, d.qty, d.source, order.shop_id, 'ACTIVE', now, now,
        );
        writeLedger(db, {
          skuId: d.skuId, channel: order.channel, shopId: d.source === 'CHANNEL' ? order.shop_id : null,
          reason: 'RESERVE', deltaReserved: d.qty, refType: 'ORDER', refId: orderId,
          note: '付款晚到，重新预占',
        });
      }
      run(db, 'UPDATE orders SET status = ?, closed_at = NULL, version = version + 1 WHERE id = ?', 'RESERVED', orderId);
      const paid = this.pay(orderId, now);
      if (!paid.ok) return { revived: false, reason: paid.reason };
      return { revived: true };
    });
  }

  /**
   * 订单已被释放（预占回滚，实物未动），但事件集合里另有支付与取消事件。
   * 净效果 = 收款后又退款，实物不变；终态应记为 CANCELLED，而不是 RELEASED。
   * 这里只修正状态与流水备注，不动库存。
   */
  markCancelledAfterRelease(orderId: string, note: string, now = Date.now()): void {
    const db = this.db;
    run(db, 'UPDATE orders SET status = ?, closed_at = ?, version = version + 1 WHERE id = ?', 'CANCELLED', now, orderId);
    run(
      db,
      `INSERT INTO ledger(sku_id, channel, shop_id, reason, ref_type, ref_id, note, created_at)
       SELECT DISTINCT oi.component_sku_id, o.channel, o.shop_id, 'REFUND', 'ORDER', o.id, ?, ?
       FROM orders o JOIN order_items oi ON oi.order_id = o.id
       WHERE o.id = ?`,
      note, now, orderId,
    );
  }

  /** 释放超时未支付订单，返回释放数量 */
  releaseExpired(timeoutMs: number, now = Date.now()): number {
    const db = this.db;
    const rows = all<{ id: string }>(
      db, 'SELECT id FROM orders WHERE status = ? AND created_at <= ?', 'RESERVED', now - timeoutMs,
    );
    let count = 0;
    for (const row of rows) {
      if (this.release(row.id, '超时未支付自动释放', now).ok) count++;
    }
    return count;
  }

  /** 低于安全线时自动收紧小渠道分配（全量扫描，常驻定时器调用） */
  applySafetyTightening(): string[] {
    const db = this.db;
    const tightened: string[] = [];
    for (const sku of all<{ id: string; safety_stock: number; is_bundle: number }>(
      db, 'SELECT id, safety_stock, is_bundle FROM skus WHERE safety_stock > 0 AND is_bundle = 0',
    )) {
      if (this.tightenSkuIfBelowSafety(sku.id)) tightened.push(sku.id);
    }
    return tightened;
  }

  /**
   * 安全线收紧小渠道分配。
   *
   * 规则：库存跌破安全线时，小渠道的可分配上限收缩为
   *   max(0, 实物 − 安全库存 − 主渠道分配)
   * 也就是把“安全库存 + 主渠道”之外的多余货量才分给小渠道；越接近安全线，
   * 小渠道分到的越少，直至归零。已预占（未付款订单）不动，保证真实订单能发货。
   * 收紧只改小渠道的 allocated，让出的量自动回到共享池，守恒不变。
   *
   * 幂等：重复调用不会反复削减（目标量由 desiredAllocation 决定，收敛后不再变化）。
   */
  tightenSkuIfBelowSafety(skuId: string): boolean {
    const db = this.db;
    const sku = getSku(db, skuId);
    if (!sku || sku.isBundle || sku.safetyStock <= 0) return false;

    const shops = listShops(db).filter((s) => s.active);
    const smalls = shops.filter((s) => s.isSmall);
    if (smalls.length === 0) return false;

    const allocatedOf = (shopId: string) =>
      Number(
        get<{ allocated: number }>(db, 'SELECT allocated FROM allocations WHERE shop_id = ? AND sku_id = ?', shopId, skuId)?.allocated ?? 0,
      );
    const reservedOf = (shopId: string) =>
      Number(
        get<{ reserved: number }>(db, 'SELECT reserved FROM allocations WHERE shop_id = ? AND sku_id = ?', shopId, skuId)?.reserved ?? 0,
      );

    const freeOf = (shopId: string) => Math.max(0, allocatedOf(shopId) - reservedOf(shopId));
    const smallFree = smalls.reduce((sum, s) => sum + freeOf(s.id), 0);
    if (smallFree <= 0) return false;

    // 目标值由统一分配规则计算（desiredAllocation 内含安全线护栏）
    const target = desiredAllocation(db, skuId);
    const sellable = skuSellable(db, skuId);
    const belowSafety = sellable < sku.safetyStock;

    let changed = false;
    for (const shop of smalls) {
      const want = target.get(shop.id) ?? 0;
      const cur = allocatedOf(shop.id);
      if (want >= cur) continue;
      const reserved = reservedOf(shop.id);
      const next = Math.max(reserved, want);
      if (next >= cur) continue;
      const cut = cur - next;
      run(db, 'UPDATE allocations SET allocated = ? WHERE shop_id = ? AND sku_id = ?', next, shop.id, skuId);
      writeLedger(db, {
        skuId, channel: shop.channel, shopId: shop.id, reason: 'REALLOCATE', deltaAllocated: -cut,
        refType: 'ALLOCATION', refId: `SAFETY:${skuId}`,
        note: `安全线收紧：实物 ${getOnHand(db, skuId)} 安全库存 ${sku.safetyStock}，小渠道上限 ${want}`,
      });
      changed = true;
    }
    if (!changed) return false;

    markSkuChanged(db, skuId, 'safety_tighten');
    raiseAlert(db, {
      type: 'SAFETY_TIGHTEN', skuId, severity: 'WARN',
      message: `SKU ${skuId} 可售 ${sellable}${belowSafety ? ' 已低于' : ' 逼近'}安全线 ${sku.safetyStock}，已收紧小渠道分配`,
      dedupeKey: `safety:${skuId}`,
    });
    return true;
  }

  ledgerFor(skuId: string, limit = 200) {
    return all(this.db, 'SELECT * FROM ledger WHERE sku_id = ? ORDER BY id DESC LIMIT ?', skuId, limit);
  }

  traceByRef(refType: string, refId: string) {
    return all(this.db, 'SELECT * FROM ledger WHERE ref_type = ? AND ref_id = ? ORDER BY id', refType, refId);
  }
}
