/** 领域共用类型与常量 */

export const CHANNEL_CODES = ['tmall', 'jd', 'douyin', 'miniapp'] as const;
export type ChannelCode = (typeof CHANNEL_CODES)[number];

export const CHANNEL_LABELS: Record<ChannelCode, string> = {
  tmall: '天猫',
  jd: '京东',
  douyin: '抖音',
  miniapp: '小程序',
};

export type OrderStatus = 'RESERVED' | 'PAID' | 'RELEASED' | 'CANCELLED' | 'FAILED';

export interface Sku {
  id: string;
  title: string;
  isBundle: boolean;
  safetyStock: number;
}

export interface ChannelShop {
  id: string;
  channel: ChannelCode;
  name: string;
  /** 分配规则：fixed 固定值 / ratio 按可用库存比例 */
  allocMode: 'fixed' | 'ratio';
  allocValue: number;
  /** 小渠道：低于安全线时优先收紧 */
  isSmall: boolean;
  priority: number;
  active: boolean;
  baseUrl: string;
}

export interface OrderLine {
  skuId: string;
  qty: number;
}

export const ORDER_EVENT_TYPES = ['ORDER_CREATED', 'ORDER_PAID', 'ORDER_CANCELLED'] as const;
export type OrderEventType = (typeof ORDER_EVENT_TYPES)[number];

/** 平台推送的订单消息 */
export interface OrderEventPayload {
  messageId: string;
  channel: ChannelCode;
  shopId: string;
  externalOrderNo: string;
  type: OrderEventType;
  /** 单笔订单内单调递增的事件序号：1 下单、2 付款、3 取消 */
  seq: number;
  occurredAt: number;
  /** 下单/付款事件都带完整行，乱序时可自愈 */
  lines?: OrderLine[];
  /** 乐观下单：平台未等预占结果就收了钱 */
  optimistic?: boolean;
}

export interface ApiResult<T> {
  ok: boolean;
  data?: T;
  error?: string;
  code?: string;
}

export interface SkuView {
  id: string;
  title: string;
  isBundle: boolean;
  safetyStock: number;
  onHand: number;
  components: Array<{ skuId: string; qty: number }>;
  sellable: number;
  belowSafety: boolean;
}
