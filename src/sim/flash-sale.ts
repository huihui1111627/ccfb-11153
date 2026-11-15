import { setTimeout as sleep } from 'node:timers/promises';
import { writeFileSync, mkdirSync } from 'node:fs';
import { dirname, resolve } from 'node:path';
import { acceptance, drainSyncFully, platformStats, runFlashSale, settle, startHarness } from './harness.js';
import { listSkus, allocationMatrix, getOnHand } from '../core/catalog.js';
import { syncHealth } from '../core/sync.js';
import { invariantViolations, reconcile } from '../server/hub.js';
import { SEED_BUNDLE, SEED_SKUS } from '../core/seed.js';

export interface FlashSaleCliOptions {
  durationMs: number;
  stock: number;
  rps: number;
  concurrency: number;
  chaosScale: number;
  output?: string;
  quiet?: boolean;
  /** 按固定节奏注入盘点，默认在中途抽盘一次 */
  stocktakeMidway?: boolean;
}

export function parseArgs(argv: string[]): FlashSaleCliOptions {
  const get = (name: string, fallback: string): string => {
    const i = argv.indexOf(`--${name}`);
    return i >= 0 && argv[i + 1] ? argv[i + 1]! : fallback;
  };
  return {
    durationMs: Number(get('duration', '20000')),
    stock: Number(get('stock', '1200')),
    rps: Number(get('rps', '90')),
    concurrency: Number(get('concurrency', '6')),
    chaosScale: Number(get('chaos', '1')),
    output: get('output', ''),
    quiet: argv.includes('--quiet'),
    stocktakeMidway: !argv.includes('--no-stocktake'),
  };
}

export async function main(argv = process.argv.slice(2)): Promise<number> {
  const opts = parseArgs(argv);
  const log = (msg: string) => {
    if (!opts.quiet) console.log(msg);
  };
  const scale = opts.chaosScale;

  console.log(`\n=== 模拟大促 ===`);
  console.log(
    `时长 ${opts.durationMs}ms | 初始库存 ${opts.stock}/SKU | 目标 ${opts.rps} 单/秒 | 并发 ${opts.concurrency} | 故障倍率 ${scale}x\n`,
  );

  const h = await startHarness({
    stockPerSku: opts.stock,
    chaos: {
      rps: 400,
      burst: 500,
      timeoutRate: 0.015 * scale,
      errorRate: 0.015 * scale,
      busyRate: 0.01 * scale,
      latencyMinMs: 3,
      latencyMaxMs: 28,
      timeoutHangMs: 900,
      duplicateRate: 0.1 * scale,
      reorderRate: 0.22 * scale,
      reorderMinDelayMs: 40,
      reorderMaxDelayMs: 450,
      pushFailRate: 0.05 * scale,
      lostAckRate: 0.015 * scale,
    },
  });

  // 后台同步循环：模拟中台常驻同步 worker 持续把库存推到平台
  let syncing = true;
  let syncRounds = 0;
  const syncLoop = (async () => {
    while (syncing) {
      try {
        await h.syncWorker.tick();
        syncRounds++;
      } catch (err) {
        console.error('sync error', err);
      }
      await sleep(12);
    }
  })();

  const skus = SEED_SKUS.map((s) => s.id);
  // 超时释放后把库存重分配并同步给渠道
  const onExpired = async (harness: typeof h, count: number) => {
    if (count > 0) {
      const { rebalanceAllocation, markSkuChanged } = await import('../core/catalog.js');
      for (const sku of skus) {
        rebalanceAllocation(harness.db, sku, '超时释放后重分配', `EXPIRE:${Date.now()}`);
        markSkuChanged(harness.db, sku, 'expire_rebalance');
      }
    }
  };
  const sale = await runFlashSale(h, {
    skus,
    bundleSku: SEED_BUNDLE.id,
    durationMs: opts.durationMs,
    concurrency: opts.concurrency,
    orderIntervalMs: 10,
    targetRps: opts.rps,
    // 小程序流量很小：主渠道把库存卖下去时，它还攥着分配没动——安全线收紧才有意义
    weights: {
      'tmall-flagship': 6,
      'jd-self': 5,
      'douyin-flagship': 4,
      'miniapp-mall': 1,
    },
    stocktake: true,
    safetyChecks: true,
  });
  syncing = false;
  await syncLoop;

  log(`下单尝试 ${sale.attempts}，平台受理 ${sale.accepted}，拒单 ${sale.rejected}`);
  log(`已付款 ${sale.paid}，取消/释放 ${sale.cancelled}`);
  for (const [shop, s] of Object.entries(sale.perShop) as Array<[string, { attempts: number; accepted: number; rejected: number; retried: number }]>) {
    log(`  ${shop}: 尝试 ${s.attempts} 受理 ${s.accepted} 拒单 ${s.rejected} 重试 ${s.retried}`);
  }

  // 结束收尾：等 webhook 全部落地，超时未付订单释放，再全量对账
  log('\n收尾：等待事件落地，等满支付超时窗口，自动释放未付款预占…');
  await settle(h, { maxMs: 15000 });
  const payWindowMs = 1200;
  await sleep(payWindowMs + 50);
  const expired = h.engine.releaseExpired(payWindowMs);
  log(`支付超时窗口 ${payWindowMs}ms，自动释放未付款预占 ${expired} 笔`);
  await onExpired?.(h, expired);
  await settle(h, { maxMs: 8000 });

  const report = await acceptance(h);

  log('\n=== 验收结果 ===');
  for (const c of report.checks) {
    log(`${c.ok ? '✅' : '❌'} ${c.name} — ${c.detail}`);
  }

  const chaos = platformStats(h).reduce(
    (acc, s) => ({
      httpRequests: acc.httpRequests + s.httpRequests,
      httpRateLimited: acc.httpRateLimited + s.httpRateLimited,
      httpTimeouts: acc.httpTimeouts + s.httpTimeouts,
      httpErrors: acc.httpErrors + s.httpErrors,
      inventoryApplied: acc.inventoryApplied + s.inventoryApplied,
      inventoryStaleIgnored: acc.inventoryStaleIgnored + s.inventoryStaleIgnored,
      inventoryDuplicateKey: acc.inventoryDuplicateKey + s.inventoryDuplicateKey,
      webhookDeliveries: acc.webhookDeliveries + s.webhookDeliveries,
      webhookDuplicates: acc.webhookDuplicates + s.webhookDuplicates,
      webhookRetries: acc.webhookRetries + s.webhookRetries,
      oversellBlockedByCache: acc.oversellBlockedByCache + s.oversellBlockedByCache,
    }),
    {
      httpRequests: 0, httpRateLimited: 0, httpTimeouts: 0, httpErrors: 0,
      inventoryApplied: 0, inventoryStaleIgnored: 0, inventoryDuplicateKey: 0,
      webhookDeliveries: 0, webhookDuplicates: 0, webhookRetries: 0, oversellBlockedByCache: 0,
    },
  );

  const dupEvents = Number(
    (h.db.prepare(`SELECT COUNT(*) AS n FROM order_events WHERE result = 'DUPLICATE'`).get() as { n: number }).n,
  );
  const conflictEvents = Number(
    (h.db.prepare(`SELECT COUNT(*) AS n FROM order_events WHERE conflict = 1`).get() as { n: number }).n,
  );
  const orders = h.db.prepare(`SELECT status, COUNT(*) AS n FROM orders GROUP BY status`).all() as Array<{ status: string; n: number }>;
  const orderStats = Object.fromEntries(orders.map((o) => [o.status, Number(o.n)]));

  log('\n=== 链路与故障注入统计 ===');
  log(`订单状态分布: ${JSON.stringify(orderStats)}`);
  log(`平台 HTTP 请求 ${chaos.httpRequests}（限流 ${chaos.httpRateLimited} / 超时 ${chaos.httpTimeouts} / 5xx ${chaos.httpErrors}）`);
  log(
    `库存推送应用 ${chaos.inventoryApplied}，平台按版本忽略旧值 ${chaos.inventoryStaleIgnored}，幂等键重复 ${chaos.inventoryDuplicateKey}`,
  );
  log(
    `Webhook 投递 ${chaos.webhookDeliveries}（重复 ${chaos.webhookDuplicates} / 重试 ${chaos.webhookRetries}），中台判定重复事件 ${dupEvents}，冲突 ${conflictEvents}`,
  );
  log(`平台缓存拦截 ${chaos.oversellBlockedByCache} 次（缓存滞后但未占用中台库存）`);

  const alloc = allocationMatrix(h.db);
  log('\n=== 各 SKU 库存 ===');
  for (const row of alloc) {
    const share = row.channels.map((c) => `${c.channel}:${c.allocated}`).join(' ');
    log(`${row.skuId.padEnd(12)} 实物 ${String(row.onHand).padStart(5)} 可售 ${String(row.sellable).padStart(5)} 共享池 ${String(row.sharedQty).padStart(4)} | ${share}`);
  }

  log('\n=== 同步健康 ===');
  for (const s of syncHealth(h.db)) {
    log(`${s.shopId.padEnd(18)} pending=${s.pending} retrying=${s.retrying} dead=${s.dead} 滞后=${s.laggingSkus} ok=${s.ok} lastError=${s.lastError ?? '-'}`);
  }

  const midRunBreaches = invariantViolations(h.db);
  const belowSafetySkus = (listSkus(h.db) as Array<{ id: string; isBundle: boolean; safetyStock: number; onHand: number }>).filter(
    (s) => !s.isBundle && s.onHand < s.safetyStock,
  );
  const alertRows = h.db
    .prepare(`SELECT type, COUNT(*) AS n FROM alerts GROUP BY type ORDER BY n DESC`)
    .all() as Array<{ type: string; n: number }>;
  const tightenCount = alertRows.find((a) => a.type === 'SAFETY_TIGHTEN')?.n ?? 0;
  if (alertRows.length > 0) {
    log('\n=== 预警统计 ===');
    for (const row of alertRows) log(`  ${row.type}: ${row.n}`);
  }

  const extraChecks = [
    { name: '重复推送被幂等丢弃', ok: dupEvents > 0 || chaos.webhookDuplicates === 0, detail: `中台判定重复 ${dupEvents} 条` },
    { name: '乱序消息最终一致', ok: conflictEvents === 0, detail: `事件冲突 ${conflictEvents} 条` },
    { name: '运行期无守恒违规', ok: (sale.invariantBreaches?.length ?? 0) === 0 && midRunBreaches.length === 0, detail: `采样违规 ${sale.invariantBreaches?.length ?? 0} 次，收尾 ${midRunBreaches.length} 处` },
    {
      name: '安全线收紧',
      // 只有真的卖到安全线以下才要求触发；库存充足时未触发是正常的
      ok: belowSafetySkus.length === 0 || tightenCount > 0,
      detail:
        belowSafetySkus.length === 0
          ? '本场库存始终高于安全线，无需收紧'
          : `${belowSafetySkus.length} 个 SKU 跌破安全线，产生收紧告警 ${tightenCount} 条`,
    },
  ];
  for (const c of extraChecks) log(`${c.ok ? '✅' : '❌'} ${c.name} — ${c.detail}`);

  const payload = {
    config: opts,
    sale,
    chaos,
    orderStats,
    duplicateEvents: dupEvents,
    conflictEvents,
    acceptance: report,
    allocation: alloc,
    sync: syncHealth(h.db),
    reconciliation: reconcile(h.db),
    bundles: listSkus(h.db).filter((s) => s.isBundle),
    balances: Object.fromEntries(listSkus(h.db).map((s) => [s.id, getOnHand(h.db, s.id)])),
  };

  if (opts.output) {
    mkdirSync(dirname(resolve(opts.output)), { recursive: true });
    writeFileSync(resolve(opts.output), JSON.stringify(payload, null, 2));
    console.log(`\n报告已写入 ${resolve(opts.output)}`);
  }

  await h.close();

  const allOk = report.passed && extraChecks.every((c) => c.ok);
  console.log(
    `\n${allOk ? '✅ 通过' : '❌ 未通过'}：超卖 ${report.oversoldQty} 件，账实差 ${report.ledgerDiscrepancies + report.inventoryDiscrepancies} 处\n`,
  );
  return allOk ? 0 : 1;
}

if (import.meta.url === `file://${process.argv[1]}`) {
  main().then((code) => process.exit(code));
}
