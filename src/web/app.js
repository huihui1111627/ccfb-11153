const $ = (sel, root = document) => root.querySelector(sel);
const $$ = (sel, root = document) => [...root.querySelectorAll(sel)];

const state = { tab: 'overview', timer: null, skus: [], shops: [] };

async function api(path, opts = {}) {
  const res = await fetch(`/admin/api${path}`, {
    headers: { 'content-type': 'application/json' },
    ...opts,
    body: opts.body ? JSON.stringify(opts.body) : undefined,
  });
  const data = await res.json();
  if (!res.ok || data.ok === false) throw new Error(data.error || `HTTP ${res.status}`);
  return data.data ?? data;
}

function toast(msg, isErr = false) {
  const el = document.createElement('div');
  el.className = `toast${isErr ? ' err' : ''}`;
  el.textContent = msg;
  document.body.appendChild(el);
  setTimeout(() => el.remove(), 2600);
}

function fmtTime(ts) {
  if (!ts) return '-';
  const d = new Date(Number(ts));
  return d.toLocaleTimeString('zh-CN', { hour12: false });
}

function esc(s) {
  return String(s ?? '').replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
}

function badge(text, kind = '') {
  return `<span class="badge ${kind}">${esc(text)}</span>`;
}

function delta(n) {
  const v = Number(n);
  if (v === 0) return '<span style="color:var(--muted)">0</span>';
  return `<span style="color:${v > 0 ? 'var(--accent)' : 'var(--danger)'}">${v > 0 ? '+' : ''}${v}</span>`;
}

const REASON_LABEL = {
  RESERVE: '预占', RELEASE: '释放', PAID: '支付扣减', REFUND: '退款回补', ALLOCATE: '渠道分配',
  REALLOCATE: '重分配', STOCKTAKE: '盘点', SHARED_MOVE: '共享池',
};

const REASON_KIND = {
  RESERVE: 'info', RELEASE: 'warn', PAID: 'ok', REFUND: 'warn', ALLOCATE: '', REALLOCATE: '', STOCKTAKE: 'info',
};

// ---------------------------------------------------------------- 概览
async function renderOverview() {
  const [data, sync] = await Promise.all([api('/overview'), api('/sync')]);
  const o = data.orders;
  const cards = [
    { label: '实物库存总量', value: data.onHandTotal, sub: `${data.skus} 个 SKU` },
    { label: '可售总量', value: data.sellableTotal, sub: '已扣除全部预占' },
    { label: '订单总数', value: o.total, sub: `已付款 ${o.paid} · 待付款 ${o.reserved}` },
    { label: '超卖件数', value: data.oversold, good: data.oversold === 0, bad: data.oversold > 0, sub: data.oversold === 0 ? '零超卖' : '需立即处理' },
    { label: '待处理预警', value: data.openAlerts, good: data.openAlerts === 0, bad: data.openAlerts > 0, sub: '超卖/安全线/同步死信' },
    { label: '待同步任务', value: data.syncPending, sub: data.syncPending === 0 ? '各渠道库存已对齐' : '同步器处理中' },
    { label: '已取消/释放', value: o.released + o.cancelled, sub: `释放 ${o.released} · 退款 ${o.cancelled}` },
    { label: '预占失败', value: o.failed, sub: '库存不足被拒单' },
  ];
  $('#overviewCards').innerHTML = cards
    .map(
      (c) => `<div class="metric"><div class="label">${c.label}</div>
      <div class="value ${c.good ? 'good' : c.bad ? 'bad' : ''}">${c.value}</div>
      <div class="sub">${c.sub}</div></div>`,
    )
    .join('');

  $('#syncSummary').innerHTML = sync
    .map(
      (s) => `<div class="sync-item">
        <h3>${esc(s.name)} ${badge(s.ok ? '正常' : '异常', s.ok ? 'ok' : 'bad')}</h3>
        <div class="kv"><span>已完成</span><b>${s.done}</b></div>
        <div class="kv"><span>待推送</span><b>${s.pending}</b></div>
        <div class="kv"><span>重试中</span><b>${s.retrying}</b></div>
        <div class="kv"><span>死信</span><b style="color:${s.dead ? 'var(--danger)' : 'inherit'}">${s.dead}</b></div>
        <div class="kv"><span>滞后 SKU</span><b style="color:${s.laggingSkus ? 'var(--warn)' : 'inherit'}">${s.laggingSkus}</b></div>
      </div>`,
    )
    .join('');
}

// ---------------------------------------------------------------- 库存分配
async function renderInventory() {
  const rows = await api('/inventory');
  const shops = state.shops.length ? state.shops : await api('/shops');
  const header = `<tr><th>SKU</th><th>名称</th><th class="num">实物</th><th class="num">渠道分配</th><th class="num">共享池</th><th class="num">可售</th>${shops
    .map((s) => `<th class="num">${esc(s.name)}</th>`)
    .join('')}<th>操作</th></tr>`;
  const body = rows
    .map((r) => {
      const cells = shops
        .map((s) => {
          const c = r.channels.find((x) => x.shopId === s.id);
          const allocated = c?.allocated ?? 0;
          const pct = r.onHand ? Math.min(100, Math.round((allocated / r.onHand) * 100)) : 0;
          return `<td class="num">${allocated}${c?.reserved ? `<span style="color:var(--warn)">-${c.reserved}</span>` : ''}
            <div class="bar"><i style="width:${pct}%"></i></div></td>`;
        })
        .join('');
      return `<tr>
        <td>${esc(r.skuId)} ${r.isBundle ? badge('套装', 'info') : ''}</td>
        <td>${esc(r.title)}</td>
        <td class="num">${r.onHand}</td>
        <td class="num">${r.channelAllocated}</td>
        <td class="num">${r.sharedQty}${r.sharedReserved ? `<span style="color:var(--warn)"> (预占${r.sharedReserved})</span>` : ''}</td>
        <td class="num"><b>${r.sellable}</b></td>
        ${cells}
        <td><button class="btn ghost small" onclick="reallocate('${r.skuId}')">重分配</button></td>
      </tr>`;
    })
    .join('');
  $('#inventoryTable').innerHTML = `<table>${header}${body}</table>`;
  $('#skuOptions').innerHTML = state.skus.map((s) => `<option value="${esc(s.id)}">${esc(s.title)}</option>`).join('');
}

window.reallocate = async (skuId) => {
  try {
    await api('/allocate', { method: 'POST', body: { skuId } });
    toast(`已按规则重分配 ${skuId}`);
    renderInventory();
  } catch (e) {
    toast(e.message, true);
  }
};

$('#stocktakeForm').addEventListener('submit', async (ev) => {
  ev.preventDefault();
  const fd = new FormData(ev.target);
  try {
    const r = await api('/stocktake', {
      method: 'POST',
      body: {
        skuId: fd.get('skuId'),
        counted: Number(fd.get('counted')),
        rule: fd.get('rule'),
        note: fd.get('note') || '后台盘点',
      },
    });
    $('#stocktakeResult').innerHTML =
      `盘点完成：${esc(r.skuId)} ${r.before} → ${r.counted}（差异 ${r.delta}）；` +
      `渠道调整 ${r.channelAdjustments.map((c) => `${esc(c.channel)} ${c.delta > 0 ? '+' : ''}${c.delta}`).join('，') || '无'}；` +
      `释放未付款订单 ${r.releasedOrderIds.length} 笔`;
    renderInventory();
  } catch (e) {
    $('#stocktakeResult').textContent = `失败：${e.message}`;
  }
});

// ---------------------------------------------------------------- SKU
async function renderSkus() {
  const rows = await api('/skus');
  state.skus = rows;
  $('#skuTable').innerHTML = `<table>
    <tr><th>SKU</th><th>名称</th><th>类型</th><th class="num">实物</th><th class="num">安全库存</th><th class="num">可售</th><th>子件</th></tr>
    ${rows
      .map(
        (r) => `<tr>
      <td>${esc(r.id)}</td><td>${esc(r.title)}</td>
      <td>${r.isBundle ? badge('套装', 'info') : badge('单品')}</td>
      <td class="num">${r.onHand}</td>
      <td class="num">${r.safetyStock}</td>
      <td class="num ${r.belowSafety ? '' : ''}">${r.sellable} ${r.belowSafety ? badge('低于安全线', 'warn') : ''}</td>
      <td>${r.components.map((c) => `${esc(c.skuId)}×${c.qty}`).join(' + ') || '-'}</td>
    </tr>`,
      )
      .join('')}
  </table>`;
}

$('#skuForm').addEventListener('submit', async (ev) => {
  ev.preventDefault();
  const fd = new FormData(ev.target);
  const raw = fd.get('components')?.toString().trim();
  try {
    const body = {
      id: fd.get('id'),
      title: fd.get('title'),
      safetyStock: Number(fd.get('safetyStock') || 0),
      components: raw ? JSON.parse(raw) : undefined,
    };
    await api('/skus', { method: 'POST', body });
    $('#skuFormMsg').textContent = '已保存';
    ev.target.reset();
    renderSkus();
  } catch (e) {
    $('#skuFormMsg').textContent = `失败：${e.message}`;
  }
});

// ---------------------------------------------------------------- 店铺
async function renderShops() {
  const rows = await api('/shops');
  state.shops = rows;
  $('#shopTable').innerHTML = `<table>
    <tr><th>店铺</th><th>渠道</th><th>分配模式</th><th class="num">分配值</th><th>小渠道</th><th class="num">优先级</th><th>状态</th><th>平台地址</th></tr>
    ${rows
      .map(
        (r) => `<tr>
      <td>${esc(r.name)}<div style="color:var(--muted);font-size:11px">${esc(r.id)}</div></td>
      <td>${esc(r.channel)}</td>
      <td>${r.allocMode === 'ratio' ? '按比例' : '固定值'}</td>
      <td class="num">${r.allocMode === 'ratio' ? Math.round(Number(r.allocValue) * 100) + '%' : r.allocValue}</td>
      <td>${r.isSmall ? badge('是', 'warn') : badge('否')}</td>
      <td class="num">${r.priority}</td>
      <td>${r.active ? badge('启用', 'ok') : badge('停用')}</td>
      <td style="color:var(--muted);font-size:11px">${esc(r.baseUrl || '-')}</td>
    </tr>`,
      )
      .join('')}
  </table>`;
}

$('#shopForm').addEventListener('submit', async (ev) => {
  ev.preventDefault();
  const fd = new FormData(ev.target);
  try {
    await api('/shops', {
      method: 'POST',
      body: {
        id: fd.get('id'),
        channel: fd.get('channel'),
        name: fd.get('name'),
        allocMode: fd.get('allocMode'),
        allocValue: Number(fd.get('allocValue')),
        isSmall: fd.get('isSmall') === 'on',
        priority: Number(fd.get('priority') || 0),
        active: true,
      },
    });
    $('#shopFormMsg').textContent = '已保存';
    renderShops();
  } catch (e) {
    $('#shopFormMsg').textContent = `失败：${e.message}`;
  }
});

// ---------------------------------------------------------------- 流水
async function renderLedger() {
  const sku = $('#ledgerSku').value.trim();
  const rows = await api(`/ledger?limit=300${sku ? `&skuId=${encodeURIComponent(sku)}` : ''}`);
  $('#ledgerTable').innerHTML = rows.length
    ? `<table>
      <tr><th>#</th><th>时间</th><th>SKU</th><th>渠道</th><th>类型</th><th class="num">实物</th><th class="num">预占</th><th class="num">分配</th><th class="num">售出</th><th>来源</th><th>备注</th></tr>
      ${rows
        .map(
          (r) => `<tr>
        <td>${r.id}</td><td>${fmtTime(r.created_at)}</td><td>${esc(r.sku_id)}</td>
        <td>${esc(r.channel)}${r.shop_id ? `<div style="color:var(--muted);font-size:11px">${esc(r.shop_id)}</div>` : ''}</td>
        <td>${badge(REASON_LABEL[r.reason] || r.reason, REASON_KIND[r.reason] || '')}</td>
        <td class="num">${delta(r.delta_on_hand)}</td>
        <td class="num">${delta(r.delta_reserved)}</td>
        <td class="num">${delta(r.delta_allocated)}</td>
        <td class="num">${delta(r.delta_sold)}</td>
        <td><code>${esc(r.ref_type)}:${esc(r.ref_id)}</code></td>
        <td style="color:var(--muted)">${esc(r.note)}</td>
      </tr>`,
        )
        .join('')}
    </table>`
    : '<div class="hint">暂无流水</div>';
}

// ---------------------------------------------------------------- 预警
const ALERT_KIND = { OVERSOLD: 'bad', SAFETY_TIGHTEN: 'warn', SYNC_DEAD: 'bad', SYNC_BREAKER: 'bad', SYNC_REPAIRED: 'warn', RESERVE_REJECT: 'warn', STOCKTAKE_NEGATIVE_POOL: 'bad' };

async function renderAlerts() {
  const status = $('#alertStatus').value;
  const rows = await api(`/alerts${status ? `?status=${status}` : ''}`);
  $('#alertTable').innerHTML = rows.length
    ? `<table>
      <tr><th>#</th><th>时间</th><th>类型</th><th>严重度</th><th>SKU / 店铺</th><th>说明</th><th>状态</th><th>操作</th></tr>
      ${rows
        .map(
          (r) => `<tr>
        <td>${r.id}</td><td>${fmtTime(r.created_at)}</td>
        <td>${badge(r.type, ALERT_KIND[r.type] || '')}</td>
        <td>${r.severity === 'CRITICAL' ? badge('严重', 'bad') : badge('提醒', 'warn')}</td>
        <td>${esc(r.sku_id || '-')} / ${esc(r.shop_id || '-')}</td>
        <td style="white-space:normal">${esc(r.message)}</td>
        <td>${r.status === 'OPEN' ? badge('待处理', 'warn') : badge('已处理', 'ok')}</td>
        <td>${r.status === 'OPEN' ? `<button class="btn ghost small" onclick="resolveAlert(${r.id})">处理</button>` : '-'}</td>
      </tr>`,
        )
        .join('')}
    </table>`
    : '<div class="hint">无预警</div>';
}

window.resolveAlert = async (id) => {
  try {
    await api('/alerts/resolve', { method: 'POST', body: { id } });
    renderAlerts();
  } catch (e) {
    toast(e.message, true);
  }
};

// ---------------------------------------------------------------- 同步
async function renderSync() {
  const rows = await api('/sync');
  $('#syncTable').innerHTML = `<table>
    <tr><th>店铺</th><th>渠道</th><th class="num">待推送</th><th class="num">重试中</th><th class="num">死信</th><th class="num">已完成</th><th class="num">滞后 SKU</th><th>最近成功/失败</th><th>最近错误</th><th>状态</th></tr>
    ${rows
      .map(
        (r) => `<tr>
      <td>${esc(r.name)}</td><td>${esc(r.channel)}</td>
      <td class="num">${r.pending}</td><td class="num">${r.retrying}</td>
      <td class="num" style="color:${r.dead ? 'var(--danger)' : 'inherit'}">${r.dead}</td>
      <td class="num">${r.done}</td>
      <td class="num" style="color:${r.laggingSkus ? 'var(--warn)' : 'inherit'}">${r.laggingSkus}</td>
      <td>${r.recent.ok} / ${r.recent.retry}</td>
      <td style="color:var(--muted)">${esc(r.lastError || '-')}</td>
      <td>${r.ok ? badge('正常', 'ok') : badge('异常', 'bad')}</td>
    </tr>`,
      )
      .join('')}
  </table>`;

  const platforms = await api('/platforms');
  $('#platformTable').innerHTML = `<table>
    <tr><th>店铺</th><th>SKU</th><th class="num">中台目标</th><th class="num">已确认版本值</th><th class="num">平台缓存</th><th class="num">版本</th><th>一致</th></tr>
    ${platforms
      .flatMap((p) =>
        Object.entries(p.inventory).map(([skuId, v]) => {
          const match = Number(v.available) === Number(v.desired) || (Number(v.available) < 0 && Number(v.desired) === 0);
          return `<tr><td>${esc(p.shop.name)}</td><td>${esc(skuId)}</td>
            <td class="num">${v.desired}</td><td class="num">${v.available}</td>
            <td class="num">${v.available}</td><td class="num">${v.version}</td>
            <td>${match ? badge('一致', 'ok') : badge('滞后', 'warn')}</td></tr>`;
        }),
      )
      .join('')}
  </table>`;
}

$('#syncRun').addEventListener('click', async () => {
  try {
    const r = await api('/sync/run', { method: 'POST' });
    $('#syncMsg').textContent = `补偿完成：处理 ${r.processed}，成功 ${r.succeeded}，失败 ${r.failed}，死信 ${r.dead}`;
    renderSync();
  } catch (e) {
    toast(e.message, true);
  }
});

$('#syncRepair').addEventListener('click', async () => {
  try {
    const r = await api('/sync/repair', { method: 'POST' });
    $('#syncMsg').textContent = `对账修复：重新入队 ${r.repaired} 个滞后 SKU`;
    renderSync();
  } catch (e) {
    toast(e.message, true);
  }
});

// ---------------------------------------------------------------- 追踪
async function renderTrace() {
  const sku = $('#traceSku').value.trim();
  if (!sku) return;
  const data = await api(`/trace?skuId=${encodeURIComponent(sku)}`);
  if (!data.sku) {
    $('#traceSummary').innerHTML = '<span style="color:var(--danger)">SKU 不存在</span>';
    $('#traceTable').innerHTML = '';
    return;
  }
  $('#traceSummary').innerHTML = `
    <div>实物 <b>${data.current.onHand}</b></div>
    <div>渠道余量 <b>${data.current.channelUnreserved}</b></div>
    <div>共享池 <b>${data.current.shared}</b></div>
    <div>可售 <b>${data.current.sellable}</b></div>
    <div>流水条数 <b>${data.entryCount}</b></div>
    ${data.usedByBundles.length ? `<div>被套装引用 <b>${data.usedByBundles.map(esc).join('、')}</b></div>` : ''}`;
  $('#traceTable').innerHTML = `<table>
    <tr><th>#</th><th>时间</th><th>渠道</th><th>类型</th><th class="num">实物</th><th class="num">预占</th><th class="num">分配</th><th class="num">售出</th><th>追责来源</th><th>备注</th></tr>
    ${data.entries
      .map(
        (e) => `<tr>
      <td>${e.id}</td><td>${fmtTime(e.createdAt)}</td>
      <td>${esc(e.channel)}${e.shopId ? `<div style="color:var(--muted);font-size:11px">${esc(e.shopId)}</div>` : ''}</td>
      <td>${badge(REASON_LABEL[e.reason] || e.reason, REASON_KIND[e.reason] || '')}</td>
      <td class="num">${delta(e.deltaOnHand)}</td>
      <td class="num">${delta(e.deltaReserved)}</td>
      <td class="num">${delta(e.deltaAllocated)}</td>
      <td class="num">${delta(e.deltaSold)}</td>
      <td><code>${esc(e.refType)}:${esc(e.refId)}</code></td>
      <td style="color:var(--muted)">${esc(e.note)}</td>
    </tr>`,
      )
      .join('')}
  </table>`;
}

$('#reconcileBtn').addEventListener('click', async () => {
  const r = await api('/reconcile');
  $('#reconcileResult').innerHTML = r.ok
    ? '<span style="color:var(--accent)">对账通过：所有 SKU 守恒，无超卖事件</span>'
    : `<span style="color:var(--danger)">对账失败：超卖 ${r.oversoldQty} 件，差异 SKU ${r.discrepancies.length} 个</span>
       <pre style="white-space:pre-wrap;color:var(--muted)">${esc(JSON.stringify({ discrepancies: r.discrepancies, oversold: r.oversoldRows }, null, 2))}</pre>`;
});

// ---------------------------------------------------------------- 路由
const renderers = {
  overview: async () => {
    await renderOverview();
  },
  inventory: async () => {
    await renderInventory();
  },
  sku: async () => {
    await renderSkus();
  },
  shop: async () => {
    await renderShops();
  },
  ledger: async () => {
    await renderLedger();
  },
  alert: async () => {
    await renderAlerts();
  },
  sync: async () => {
    await renderSync();
  },
  trace: async () => {
    await renderTrace();
  },
};

async function render() {
  try {
    await renderers[state.tab]();
    $('#liveDot').classList.remove('off');
    $('#liveText').textContent = `最后刷新 ${new Date().toLocaleTimeString('zh-CN', { hour12: false })}`;
  } catch (e) {
    $('#liveDot').classList.add('off');
    $('#liveText').textContent = `刷新失败：${e.message}`;
  }
}

function switchTab(tab) {
  state.tab = tab;
  $$('.tab').forEach((t) => t.classList.toggle('active', t.dataset.tab === tab));
  $$('.panel').forEach((p) => p.classList.toggle('hidden', p.id !== `panel-${tab}`));
  render();
}

$$('.tab').forEach((t) => t.addEventListener('click', () => switchTab(t.dataset.tab)));
$('#refresh').addEventListener('click', render);
$('#ledgerLoad').addEventListener('click', renderLedger);
$('#alertLoad').addEventListener('click', renderAlerts);
$('#alertStatus').addEventListener('change', renderAlerts);
$('#traceLoad').addEventListener('click', renderTrace);

$('#ledgerSku').value = 'TEE-BLK-M';
$('#traceSku').value = 'TEE-BLK-M';

(async () => {
  try {
    state.skus = await api('/skus');
    state.shops = await api('/shops');
  } catch {
    /* ignore */
  }
  render();
  state.timer = setInterval(() => {
    if (document.visibilityState === 'visible') render();
  }, 5000);
})();
