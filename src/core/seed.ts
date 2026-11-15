import type { DB } from './db.js';
import { run, tx } from './db.js';
import { markSkuChanged, rebalanceAllocation, upsertShop, upsertSku, setOnHand } from './catalog.js';

export const SEED_SKUS = [
  { id: 'TEE-BLK-M', title: '基础纯棉T恤 黑色 M', safetyStock: 30 },
  { id: 'TEE-BLK-L', title: '基础纯棉T恤 黑色 L', safetyStock: 30 },
  { id: 'TEE-WHT-M', title: '基础纯棉T恤 白色 M', safetyStock: 25 },
  { id: 'JKT-DNM-M', title: '宽松牛仔外套 M', safetyStock: 12 },
  { id: 'JKT-DNM-L', title: '宽松牛仔外套 L', safetyStock: 12 },
  { id: 'SOCK-3PK', title: '纯色短袜三双装', safetyStock: 40 },
];

export const SEED_BUNDLE = {
  id: 'SET-TEE-SOCK',
  title: 'T恤+短袜超值套装',
  safetyStock: 0,
  components: [
    { skuId: 'TEE-BLK-M', qty: 1 },
    { skuId: 'SOCK-3PK', qty: 1 },
  ],
};

export const SEED_SHOPS = [
  { id: 'tmall-flagship', channel: 'tmall' as const, name: '天猫旗舰店', allocMode: 'ratio' as const, allocValue: 0.3, isSmall: false, priority: 1, baseUrl: '' },
  { id: 'jd-self', channel: 'jd' as const, name: '京东自营店', allocMode: 'ratio' as const, allocValue: 0.3, isSmall: false, priority: 2, baseUrl: '' },
  { id: 'douyin-flagship', channel: 'douyin' as const, name: '抖音旗舰店', allocMode: 'fixed' as const, allocValue: 20, isSmall: false, priority: 3, baseUrl: '' },
  { id: 'miniapp-mall', channel: 'miniapp' as const, name: '微信小程序商城', allocMode: 'fixed' as const, allocValue: 10, isSmall: true, priority: 4, baseUrl: '' },
];

export interface SeedOptions {
  stockPerSku?: number;
  shops?: Array<{ id: string; baseUrl?: string }>;
}

/** 初始化基础数据：SKU、套装、4 个渠道店铺、初始库存与渠道分配 */
export function seed(db: DB, opts: SeedOptions = {}): void {
  const stock = opts.stockPerSku ?? 1000;
  tx(db, () => {
    for (const sku of SEED_SKUS) {
      upsertSku(db, { id: sku.id, title: sku.title, safetyStock: sku.safetyStock });
    }
    upsertSku(db, SEED_BUNDLE);

    const urlMap = new Map((opts.shops ?? []).map((s) => [s.id, s.baseUrl ?? '']));
    for (const shop of SEED_SHOPS) {
      upsertShop(db, { ...shop, active: true, baseUrl: urlMap.get(shop.id) ?? '' });
    }

    const exists = db.prepare('SELECT COUNT(*) AS n FROM stock WHERE on_hand > 0').get() as { n: number };
    if (Number(exists.n) === 0) {
      for (const sku of SEED_SKUS) {
        setOnHand(db, sku.id, stock);
        run(
          db,
          `INSERT INTO ledger(sku_id, channel, shop_id, reason, delta_on_hand, ref_type, ref_id, note, created_at)
           VALUES(?, 'WAREHOUSE', NULL, 'STOCKTAKE', ?, 'STOCKTAKE', 'INIT', '期初入库', ?)`,
          sku.id, stock, Date.now(),
        );
        rebalanceAllocation(db, sku.id, '期初按比例分配渠道', 'INIT');
        markSkuChanged(db, sku.id, 'seed');
      }
    } else {
      for (const sku of SEED_SKUS) markSkuChanged(db, sku.id, 'seed');
    }
  });
}
