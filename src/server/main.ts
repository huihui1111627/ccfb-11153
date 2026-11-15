import { mkdirSync } from 'node:fs';
import { resolve } from 'node:path';
import { openDb } from '../core/db.js';
import { seed } from '../core/seed.js';
import { InventoryHubServer } from './hub.js';
import { MockPlatform } from '../platforms/mock.js';
import { makeHttpInventoryPusher, registerShopBaseUrl, setHubBaseUrl } from '../platforms/client.js';
import { listShops } from '../core/catalog.js';
import { SEED_SHOPS } from '../core/seed.js';
import type { ChannelCode } from '../core/types.js';

/**
 * 常驻演示进程：启动中台 + 4 个 mock 平台 + 后台页面。
 * 打开终端里打印的后台地址即可操作；mock 平台会按比例注入限流/超时/重复推送。
 */
async function main(): Promise<void> {
  const argv = process.argv.slice(2);
  const arg = (name: string, fallback: string): string => {
    const i = argv.indexOf(`--${name}`);
    return i >= 0 && argv[i + 1] ? argv[i + 1]! : fallback;
  };
  const hubPort = Number(arg('port', '8080'));
  const dbPath = resolve(arg('db', 'data/hub.db'));
  const stock = Number(arg('stock', '600'));
  mkdirSync(resolve(dbPath, '..'), { recursive: true });

  const db = openDb(dbPath);
  const fresh = Number((db.prepare('SELECT COUNT(*) AS n FROM skus').get() as { n: number }).n) === 0;
  if (fresh) seed(db, { stockPerSku: stock });

  const hub = new InventoryHubServer({ db, pushInventory: makeHttpInventoryPusher(1200) });
  const port = await hub.listen(hubPort);
  const hubUrl = `http://127.0.0.1:${port}`;
  setHubBaseUrl(hubUrl);

  const platforms: MockPlatform[] = [];
  for (const shop of SEED_SHOPS) {
    const platform = new MockPlatform({
      channel: shop.channel as ChannelCode,
      shopId: shop.id,
      hubBaseUrl: hubUrl,
      chaos: {
        timeoutRate: 0.02,
        errorRate: 0.02,
        busyRate: 0.01,
        duplicateRate: 0.12,
        reorderRate: 0.25,
        pushFailRate: 0.05,
        lostAckRate: 0.02,
      },
    });
    const p = await platform.listen(0);
    registerShopBaseUrl(shop.id, platform.baseUrl);
    db.prepare('UPDATE shops SET base_url = ? WHERE id = ?').run(platform.baseUrl, shop.id);
    platforms.push(platform);
    console.log(`  mock ${shop.name.padEnd(8)} ${platform.baseUrl}`);
  }

  // 常驻同步器：每 15ms 处理一批同步任务
  const syncTimer = setInterval(() => {
    void hub.syncWorker.tick();
  }, 15);
  // 每分钟核对并修复滞后
  const repairTimer = setInterval(() => {
    void hub.syncWorker.tick();
  }, 60_000);
  // 超时未付款释放：每 5 秒扫一次，默认 15 分钟窗口（可通过 --pay-timeout 覆盖）
  const payTimeout = Number(arg('pay-timeout', String(15 * 60_000)));
  const expireTimer = setInterval(() => {
    const n = hub.engine.releaseExpired(payTimeout);
    if (n > 0) console.log(`[expire] 释放超时未付订单 ${n} 笔`);
  }, 5000);
  // 安全线检查
  const safetyTimer = setInterval(() => {
    const list = hub.engine.applySafetyTightening();
    if (list.length) console.log(`[safety] 收紧小渠道分配: ${list.join(', ')}`);
  }, 3000);

  // 初始把库存推给平台
  await hub.syncWorker.drain(50);

  console.log(`\n中台已启动`);
  console.log(`  运营后台: ${hubUrl}/admin/`);
  console.log(`  渠道 API: ${hubUrl}/api/orders  (POST)`);
  console.log(`  Webhook : ${hubUrl}/webhooks/orders  (POST)`);
  console.log(`  数据库  : ${dbPath}${fresh ? '（已初始化演示数据）' : ''}`);
  console.log(`\n门店: ${listShops(db).map((s) => `${s.name}(${s.id})`).join(' | ')}`);
  console.log(`支付超时窗口: ${payTimeout}ms，下单未付款超过此时间会自动释放预占\n`);

  const shutdown = async () => {
    console.log('\n正在关闭…');
    clearInterval(syncTimer);
    clearInterval(repairTimer);
    clearInterval(expireTimer);
    clearInterval(safetyTimer);
    for (const p of platforms) await p.close();
    await hub.close();
    db.close();
    process.exit(0);
  };
  process.on('SIGINT', () => void shutdown());
  process.on('SIGTERM', () => void shutdown());
}

main().catch((err) => {
  console.error(err);
  process.exit(1);
});
