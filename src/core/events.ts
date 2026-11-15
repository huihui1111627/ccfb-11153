import type { DB } from './db.js';
import { all, get, run, tx } from './db.js';
import { getOrderByExternal, newOrderId, type InventoryEngine } from './engine.js';
import type { OrderEventPayload } from './types.js';
import { raiseAlert } from './alerts.js';

export interface EventProcessResult {
  status: 'APPLIED' | 'DUPLICATE' | 'OUT_OF_ORDER' | 'CONFLICT' | 'DEFERRED';
  orderStatus?: string;
  message: string;
  orderId?: string;
}

/**
 * 平台订单消息处理。
 *
 * 平台推送的特点：可能重复、可能乱序、可能丢失（重试补齐）。
 * 设计原则：处理结果只取决于“该订单收到过的事件集合”，与到达顺序无关。
 *
 * 1. 事件按 (channel, shopId, externalOrderNo, seq) 去重落库；重复 = DUPLICATE，不产生副作用。
 * 2. 每个事件到来时，把该订单已落库的事件按 seq 重放一遍——终态由 seq 顺序决定，
 *    所以先到 PAID 后到 CREATED 与先到 CREATED 后到 PAID 结果相同。
 * 3. 数据不完整时(如先到无行信息的 CREATED)置 DEFERRED，等后续事件补齐后自动重放。
 * 4. 同一 seq 出现不同事件类型 = CONFLICT，保留先到版本并告警，供人工核对。
 */
export class OrderEventProcessor {
  constructor(
    private readonly db: DB,
    private readonly engine: InventoryEngine,
  ) {}

  process(event: OrderEventPayload): EventProcessResult {
    const db = this.db;
    return tx(db, () => {
      const dup = get<{ processed: number; result: string | null }>(
        db,
        'SELECT processed, result FROM order_events WHERE channel = ? AND shop_id = ? AND external_order_no = ? AND seq = ?',
        event.channel, event.shopId, event.externalOrderNo, event.seq,
      );
      if (dup) {
        return {
          status: 'DUPLICATE',
          message: `重复推送 seq=${event.seq}，已幂等忽略`,
          orderStatus: this.orderStatus(event),
          orderId: this.findOrder(event)?.id,
        };
      }

      run(
        db,
        `INSERT INTO order_events(message_id, channel, shop_id, external_order_no, event_type, seq, occurred_at, payload, received_at, processed, conflict)
         VALUES(?,?,?,?,?,?,?,?,?,0,0)`,
        event.messageId, event.channel, event.shopId, event.externalOrderNo,
        event.type, event.seq, event.occurredAt, JSON.stringify(event), Date.now(),
      );

      const result = this.reconcile(event);
      run(
        db,
        'UPDATE order_events SET processed = 1, result = ? WHERE channel = ? AND shop_id = ? AND external_order_no = ? AND seq = ?',
        result.status, event.channel, event.shopId, event.externalOrderNo, event.seq,
      );
      return result;
    });
  }

  /**
   * 按事件集合推导该订单应处于的终态，再与当前状态比对，只做“向前”修正。
   * 这是乱序安全的：重复调用同一事件集合得到同一结果。
   */
  private reconcile(event: OrderEventPayload): EventProcessResult {
    const db = this.db;
    const events = all<{ seq: number; event_type: string; payload: string; conflict: number }>(
      db,
      `SELECT seq, event_type, payload, conflict FROM order_events
       WHERE channel = ? AND shop_id = ? AND external_order_no = ? ORDER BY seq`,
      event.channel, event.shopId, event.externalOrderNo,
    );

    // 目标状态：按 seq 顺序，最后一个“有效”事件决定
    let target: 'CREATED' | 'PAID' | 'CANCELLED' | null = null;
    let lines: OrderEventPayload['lines'] = undefined;
    for (const row of events) {
      const ev = JSON.parse(row.payload) as OrderEventPayload;
      if (ev.lines && ev.lines.length > 0 && !lines) lines = ev.lines;
      if (ev.type === 'ORDER_CREATED') target = 'CREATED';
      else if (ev.type === 'ORDER_PAID') target = 'PAID';
      else if (ev.type === 'ORDER_CANCELLED') target = 'CANCELLED';
    }
    if (target === null) {
      return { status: 'DEFERRED', message: '事件集合无有效状态', orderStatus: this.orderStatus(event) };
    }

    const order = this.findOrder(event);
    const current = order?.status;

    // 需要订单行才能建单
    if (target === 'CREATED' && !order && !lines) {
      return { status: 'DEFERRED', message: '缺少订单行，等待完整消息', orderStatus: current };
    }

    if (!order) {
      // 付款/取消先到：用已收到的行建单（乐观路径：付款先到直接扣）
      if (!lines) return { status: 'DEFERRED', message: `订单不存在且缺少订单行`, orderStatus: current };
      const created = this.engine.reserve({
        orderId: newOrderId(event.externalOrderNo, event.channel, event.shopId),
        shopId: event.shopId,
        channel: event.channel,
        externalOrderNo: event.externalOrderNo,
        lines,
        idempotencyKey: `event:${event.channel}:${event.shopId}:${event.externalOrderNo}`,
        now: event.occurredAt,
      });
      const orderId = created.orderId;
      if (target === 'PAID') {
        const paid = this.engine.pay(orderId);
        return {
          status: 'APPLIED',
          orderId,
          orderStatus: this.orderStatus(event),
          message: `乱序：付款先到，已补齐建单并扣减${created.ok ? '' : '（库存不足，订单以失败态落库）'}${paid.ok ? '' : ` / 支付失败: ${paid.reason}`}`,
        };
      }
      if (target === 'CANCELLED') {
        this.engine.release(orderId, '乱序取消事件');
        const hasPaidEvent = events.some((row) => {
          const ev = JSON.parse(row.payload) as OrderEventPayload;
          return ev.type === 'ORDER_PAID';
        });
        if (hasPaidEvent) {
          // 事件集里同时有付款与取消：净效果为零，终态统一记为已取消
          this.engine.markCancelledAfterRelease(orderId, '付款与取消事件均到达，订单已释放，净库存不变');
        }
        return { status: 'APPLIED', orderId, orderStatus: this.orderStatus(event), message: '乱序：取消先到，已建单并直接释放' };
      }
      return {
        status: 'APPLIED',
        orderId,
        orderStatus: this.orderStatus(event),
        message: created.ok ? '预占成功' : `预占失败: ${created.reason}`,
      };
    }

    const orderId = order.id;
    if (target === 'CREATED') {
      // 已存在订单；若当前状态比 CREATED 更靠后则无需动作
      if (current === 'RESERVED' || current === 'PAID' || current === 'CANCELLED' || current === 'RELEASED') {
        return { status: 'APPLIED', orderId, orderStatus: current, message: '订单已存在，创建事件幂等' };
      }
      return { status: 'APPLIED', orderId, orderStatus: current, message: `订单当前 ${current}，创建事件不触发变更` };
    }

    if (target === 'PAID') {
      if (current === 'PAID') {
        return { status: 'DUPLICATE', orderId, orderStatus: current, message: '订单已支付，支付事件幂等' };
      }
      if (current === 'FAILED') {
        // 平台收了钱但我们没有货：绝不超卖，但必须让人立刻看到并安排退款
        raiseAlert(db, {
          type: 'PAID_WITHOUT_STOCK',
          skuId: null,
          shopId: event.shopId,
          severity: 'CRITICAL',
          message: `订单 ${orderId} 预占失败（无库存）却收到支付事件，需人工退款：${order.reject_reason ?? ''}`,
          dedupeKey: `paidnostock:${orderId}`,
        });
        return { status: 'CONFLICT', orderId, orderStatus: current, message: '预占失败的订单收到支付事件，已告警待人工退款（不超卖）' };
      }
      if (current === 'RESERVED') {
        const r = this.engine.pay(orderId);
        return {
          status: r.ok ? 'APPLIED' : 'CONFLICT',
          orderId,
          orderStatus: this.orderStatus(event),
          message: r.ok ? '支付扣减成功' : `支付失败: ${r.reason}`,
        };
      }
      if (current === 'RELEASED' || current === 'CANCELLED') {
        // 超时释放后平台才确认付款：尝试复活（重新预占 + 扣减）
        const r = this.engine.reviveAndPay(orderId);
        return {
          status: r.revived ? 'APPLIED' : 'CONFLICT',
          orderId,
          orderStatus: this.orderStatus(event),
          message: r.revived ? '订单已超时释放，付款晚到，已重新预占并扣减' : `付款晚到但无法复活: ${r.reason}`,
        };
      }
      return { status: 'APPLIED', orderId, orderStatus: current, message: '无可执行变更' };
    }

    // target === CANCELLED：取消是终态，晚到的取消覆盖不了已支付（退款走售后）
    if (current === 'RELEASED' || current === 'CANCELLED') {
      const hasPaidEvent = events.some((row) => {
        const ev = JSON.parse(row.payload) as OrderEventPayload;
        return ev.type === 'ORDER_PAID';
      });
      if (current === 'RELEASED' && hasPaidEvent) {
        // 曾收到支付事件但订单已释放：净效果为零，终态统一为 CANCELLED
        this.engine.markCancelledAfterRelease(orderId, '支付与取消事件均到达，订单已释放，净库存不变');
        return { status: 'APPLIED', orderId, orderStatus: this.orderStatus(event), message: '订单已释放且收到支付事件，终态修正为已取消（净库存不变）' };
      }
      return { status: 'APPLIED', orderId, orderStatus: current, message: `订单 ${current}，取消事件幂等` };
    }
    if (current === 'PAID') {
      const r = this.engine.refund(orderId);
      return { status: r.ok ? 'APPLIED' : 'CONFLICT', orderId, orderStatus: this.orderStatus(event), message: r.ok ? '已支付订单收到取消，按退款回补' : `退款失败: ${r.reason}` };
    }
    if (current === 'RESERVED') {
      const r = this.engine.release(orderId, '平台取消');
      return { status: 'APPLIED', orderId, orderStatus: this.orderStatus(event), message: r.ok ? '释放预占成功' : `释放失败: ${r.reason}` };
    }
    return { status: 'APPLIED', orderId, orderStatus: current, message: `订单 ${current}，取消事件幂等` };
  }

  private findOrder(event: OrderEventPayload) {
    return getOrderByExternal(this.db, event.channel, event.shopId, event.externalOrderNo);
  }

  private orderStatus(event: OrderEventPayload): string | undefined {
    return this.findOrder(event)?.status;
  }
}
