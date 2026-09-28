'use strict';

const $ = (s, el = document) => el.querySelector(s);
const $$ = (s, el = document) => [...el.querySelectorAll(s)];

const state = { routes: [], meta: null, tab: 'all' };

/* ── 工具 ─────────────────────────────────────────── */
function esc(s) {
  return String(s ?? '').replace(/[&<>"']/g, c =>
    ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
}
function minutesText(m) {
  if (m == null) return '-';
  const h = Math.floor(m / 60), mm = m % 60;
  return h ? `${h}小时${String(mm).padStart(2, '0')}分` : `${mm}分`;
}
async function api(path, opts) {
  const res = await fetch(path, opts);
  let data = null;
  try { data = await res.json(); } catch (_) { /* ignore */ }
  if (!res.ok) throw new Error((data && data.error) || `请求失败（${res.status}）`);
  return data;
}
function setStatus(kind, text) {
  const el = $('#status');
  el.className = 'status' + (kind === 'error' ? ' error' : '');
  el.innerHTML = kind === 'loading' ? `<span class="spinner"></span>${esc(text)}` : esc(text);
  el.hidden = false;
}

/* ── 初始化 ───────────────────────────────────────── */
async function init() {
  try {
    const seats = await api('/api/seats');
    $('#seat').innerHTML = seats
      .map(o => `<option value="${esc(o.value)}">${esc(o.label)}</option>`).join('');
    $('#fSeat').innerHTML = '<option value="">不限</option>' + seats
      .filter(o => o.value !== 'auto')
      .map(o => `<option value="${esc(o.value)}">${esc(o.label)}</option>`).join('');

    const info = await api('/api/info');
    $('#disclaimer').textContent = info.disclaimer;
    let hint = (`离线数据库：${info.stations} 座车站 ｜ 最多 ${info.limits.maxTransfer} 次中转 ｜ ` +
      `换乘间隔 ≥ ${info.limits.minGap} 分钟 ｜ 结果上限 ${info.limits.maxResult} 条 ｜ 全程零网络请求`);
    if (info.fareDate) {
      const days = Math.max(0, Math.floor((Date.now() - new Date(info.fareDate).getTime()) / 86400000));
      const age = days === 0 ? '今天' : `${days} 天前`;
      hint += ` ｜ 12306 真实票价（${info.fareCount} 条）抓取于 ${age}`;
      if (days > 15) {
        hint += ' ⚠ 已超过 15 天，建议更新：python tools/fetch_12306_fare.py --all';
      }
    } else {
      hint += ' ｜ ⚠ 尚未抓取 12306 真实票价，部分区间为估算。执行：python tools/fetch_12306_fare.py --all';
    }
    $('#techHint').textContent = hint;
  } catch (e) {
    setStatus('error', '初始化失败：' + e.message);
  }
  bindSuggest($('#from'));
  bindSuggest($('#to'));
}

/* ── 车站联想 ─────────────────────────────────────── */
const suggestCache = new Map();
function bindSuggest(input) {
  let timer = null;
  input.addEventListener('input', () => {
    clearTimeout(timer);
    const q = input.value.trim();
    if (!q) return;
    timer = setTimeout(async () => {
      let list = suggestCache.get(q);
      if (!list) {
        try {
          list = await api('/api/suggest?q=' + encodeURIComponent(q));
          suggestCache.set(q, list);
        } catch (_) { return; }
      }
      $('#stationList').innerHTML = list
        .map(s => `<option value="${esc(String(s).replace(/（.*?）/g, ''))}"></option>`)
        .join('');
    }, 160);
  });
}

/* ── 查询 ─────────────────────────────────────────── */
$('#form').addEventListener('submit', async e => {
  e.preventDefault();
  const from = $('#from').value.trim();
  const to = $('#to').value.trim();
  if (!from || !to) return;

  const btn = $('#go');
  btn.disabled = true;
  setStatus('loading', `正在计算 ${from} → ${to} 的最优换乘方案…`);
  $('#results').innerHTML = '';
  $('#toolbar').hidden = true;

  try {
    const params = new URLSearchParams({ from, to, seat: $('#seat').value });
    const t = $('#time').value;
    if (t) params.set('time', t);
    const res = await api('/api/search?' + params);
    state.meta = res;
    state.routes = res.routes || [];
    state.tab = 'all';
    $$('#tabs .tab').forEach(b => b.classList.toggle('active', b.dataset.tab === 'all'));
    renderMeta(res);
    $('#toolbar').hidden = false;
    render();
  } catch (err) {
    setStatus('error', err.message);
  } finally {
    btn.disabled = false;
  }
});

$('#swap').addEventListener('click', () => {
  const a = $('#from').value;
  $('#from').value = $('#to').value;
  $('#to').value = a;
});

/* ── 概览 ─────────────────────────────────────────── */
function renderMeta(res) {
  const chips = [];
  chips.push(`<span class="chip">出发站 <b>${esc(res.startStations.join(' / '))}</b></span>`);
  chips.push(`<span class="chip">到达站 <b>${esc(res.goalStations.join(' / '))}</b></span>`);
  if (res.hasDirect) {
    const kind = res.directFast ? '高铁/动车最便宜直达' : '普速直达基准';
    chips.push(`<span class="chip ok">${kind} ${esc(res.directTrain || '')} <b>¥${res.directPrice.toFixed(1)}</b> / ${minutesText(res.directMinutes)}</span>`);
  } else {
    chips.push('<span class="chip">无直达车次，按综合成本排序</span>');
  }
  chips.push(`<span class="chip">命中 <b>${res.totalRoutes}</b> 条 ／ 耗时 ${res.elapsedMs} ms</span>`);
  chips.push(`<span class="chip">剪枝：时间 ${res.stats.pruneTime} · 支配 ${res.stats.pruneDominated}</span>`);
  $('#metaChips').innerHTML = chips.join('');
  $('#meta').hidden = false;
}

/* ── 筛选与排序 ───────────────────────────────────── */
function applyFilters() {
  let list = state.routes.slice();
  if (state.tab !== 'all') {
    const n = Number(state.tab);
    list = list.filter(r => r.transfers === n);
  }
  const fSeat = $('#fSeat').value;
  if (fSeat) {
    list = list.filter(r => r.legs.every(l => l.fares.some(f => f.seat === fSeat)));
  }
  const fp = parseFloat($('#fPrice').value);
  if (!Number.isNaN(fp)) list = list.filter(r => r.totalPrice <= fp);
  const ft = parseFloat($('#fTime').value);
  if (!Number.isNaN(ft)) list = list.filter(r => r.totalMin <= ft * 60);
  if ($('#fCheap').checked) list = list.filter(r => r.isCheap);

  const cmp = {
    score: (a, b) => b.score - a.score,
    price: (a, b) => a.totalPrice - b.totalPrice,
    time: (a, b) => a.totalMin - b.totalMin,
    depart: (a, b) => a.firstDepart - b.firstDepart,
    transfer: (a, b) => a.transfers - b.transfers || b.score - a.score,
  }[$('#sort').value];
  return list.sort(cmp || ((a, b) => b.score - a.score));
}

function render() {
  const list = applyFilters();
  if (!state.meta) return;
  if (!list.length) {
    $('#results').innerHTML = '';
    setStatus('', '当前筛选条件下没有匹配的方案，可放宽筛选条件试试。');
    return;
  }
  $('#results').innerHTML = list.map(routeCard).join('');
  setStatus('', `显示 ${list.length} 条 / 共 ${state.routes.length} 条方案（数据库命中 ${state.meta.totalRoutes} 条）`);
}

/* ── 方案卡片 ─────────────────────────────────────── */
function routeCard(r, i) {
  const tLabel = r.transfers === 0 ? '直达' : `${r.transfers} 次中转`;
  const tCls = r.transfers === 0 ? 'badge n0' : 'badge';
  const cheap = r.isCheap ? '<span class="badge cheap">★ 高性价比</span>' : '';

  let save = '';
  if (state.meta.hasDirect) {
    save = r.savePercent >= 0
      ? `<span class="save">较直达省 ${r.savePercent.toFixed(1)}%</span>`
      : `<span class="save up">比直达贵 ${Math.abs(r.savePercent).toFixed(1)}%</span>`;
  }

  const first = r.legs[0], last = r.legs[r.legs.length - 1];
  const legs = r.legs.map((l, j) => {
    const isLast = j === r.legs.length - 1;
    const pills = l.fares.map(f =>
      `<span class="seat-pill ${f.seat === l.seat ? 'used' : ''}">${esc(f.label)} ¥${f.price.toFixed(1)}</span>`
    ).join('');
    const transfer = (!isLast)
      ? `<div class="transfer">换乘 ${esc(l.to)} · 等待 <em>${minutesText(r.legs[j + 1].waitMin)}</em></div>`
      : '';
    return `
      <div class="leg">
        <div class="rail">
          <div class="dot ${j === 0 ? '' : 'end'}"></div>
          ${isLast ? '' : '<div class="line"></div>'}
        </div>
        <div class="leg-body">
          <div class="leg-title">
            <span class="train">${esc(l.trainNo)}</span>
            <span class="ttype">${esc(l.trainType)}</span>
            <span class="route-text">
              <b>${esc(l.from)}</b> <span class="time">${esc(l.depart)}</span>
              →
              <b>${esc(l.to)}</b> <span class="time">${esc(l.arrive)}</span>
            </span>
          </div>
          <div class="leg-meta">
            <span class="seat-price">${esc(l.seatName)} ¥${l.price.toFixed(1)}</span>
            <span>用时 ${minutesText(l.duration)}</span>
            ${l.km ? `<span>${l.km} km</span>` : ''}
          </div>
          ${pills ? `<div class="seat-list">${pills}</div>` : ''}
          ${transfer}
        </div>
      </div>`;
  }).join('');

  return `
    <article class="card ${r.isCheap ? 'cheap' : ''}">
      <div class="card-head">
        <span class="badge rank">#${i + 1}</span>
        <span class="${tCls}">${tLabel}</span>
        ${cheap}
        <span class="price">¥${r.totalPrice.toFixed(1)}</span>
      </div>
      <div class="card-sub">
        <span>全程 <b>${minutesText(r.totalMin)}</b></span>
        <span>${esc(first.depart)} 出发 → ${esc(last.arrive)} 到达</span>
        <span>综合得分 ${r.score.toFixed(1)}</span>
        ${save}
      </div>
      <div class="legs">${legs}</div>
    </article>`;
}

/* ── 交互绑定 ─────────────────────────────────────── */
$('#tabs').addEventListener('click', e => {
  const btn = e.target.closest('.tab');
  if (!btn) return;
  state.tab = btn.dataset.tab;
  $$('#tabs .tab').forEach(b => b.classList.toggle('active', b === btn));
  render();
});

['#sort', '#fSeat', '#fPrice', '#fTime', '#fCheap'].forEach(sel => {
  $(sel).addEventListener('change', render);
  $(sel).addEventListener('input', render);
});

$('#reset').addEventListener('click', () => {
  $('#sort').value = 'score';
  $('#fSeat').value = '';
  $('#fPrice').value = '';
  $('#fTime').value = '';
  $('#fCheap').checked = false;
  state.tab = 'all';
  $$('#tabs .tab').forEach(b => b.classList.toggle('active', b.dataset.tab === 'all'));
  render();
});

init();

// ============ 数据更新面板（12306 真实票价抓取 / 导入） ============
const dsTimerEl = () => null;

let dsTimer = null;

function dsPct(s) {
  return s.allTotal ? Math.min(100, Math.round((s.doneTotal || 0) * 100 / s.allTotal)) : 0;
}

function dsRender(s) {
  const days = s.fareDate
    ? Math.max(0, Math.floor((Date.now() - new Date(s.fareDate).getTime()) / 86400000))
    : null;
  document.getElementById('dsSummary').textContent = s.fareDate
    ? ('12306 真实票价 ' + (s.fareCount || 0) + ' 条（抓取于 ' + (days === 0 ? '今天' : days + ' 天前') + '）' + (days > 15 ? ' ⚠ 已超过 15 天，建议重新抓取' : ''))
    : '尚未抓取 12306 真实票价（当前部分区间为估算价）';
  document.getElementById('dsStart').hidden = !!s.running;
  document.getElementById('dsStop').hidden = !s.running;

  const pct = dsPct(s);
  document.getElementById('dsBar').hidden = !(s.running || (s.doneTotal || 0) > 0);
  document.getElementById('dsBarFill').style.width = pct + '%';

  let detail = '已覆盖 ' + (s.doneTotal || 0) + ' / ' + (s.allTotal || 0) + ' 个线路区间（' + pct + '%），票价 ' + (s.csvRows || 0) + ' 行';
  if (s.running) {
    detail += ' ｜ 本轮 ' + (s.done || 0) + '/' + (s.total || 0) + '，成功 ' + (s.ok || 0) + '，失败 ' + (s.fail || 0) + '，新增 ' + (s.rows || 0) + ' 行';
  }
  if (s.lastErr) detail += ' ｜ 最近错误：' + s.lastErr;
  document.getElementById('dsDetail').textContent = detail;
}

async function dsRefresh() {
  try {
    const s = await api('/api/update/status');
    dsRender(s);
    if (s.running && !dsTimer) dsTimer = setInterval(dsRefresh, 3000);
    if (!s.running && dsTimer) { clearInterval(dsTimer); dsTimer = null; dsRefresh(); }
  } catch (_) { /* 服务未就绪时忽略 */ }
}

document.getElementById('dsStart').addEventListener('click', async () => {
  try {
    const r = await api('/api/update/start', { method: 'POST' });
    if (!r.ok) alert(r.msg);
    dsRefresh();
  } catch (e) { alert(e.message); }
});

document.getElementById('dsStop').addEventListener('click', async () => {
  try { await api('/api/update/stop', { method: 'POST' }); } catch (_) {}
  dsRefresh();
});

document.getElementById('dsImport').addEventListener('click', async () => {
  try {
    const r = await api('/api/update/import', { method: 'POST' });
    if (r.ok) {
      alert('已导入 ' + r.count + ' 条 12306 真实票价（抓取日期 ' + r.fareDate + '），立即生效');
      dsRefresh();
    } else {
      alert(r.msg);
    }
  } catch (e) { alert(e.message); }
});

dsRefresh();

// ==================== 旅行模式（与中转比价并存，独立面板） ====================
const fareMain = document.getElementById('fareMain');
const travelPanel = document.getElementById('travelPanel');
const modeFare = document.getElementById('modeFare');
const modeTravel = document.getElementById('modeTravel');
const tStatus = document.getElementById('tStatus');
const tResults = document.getElementById('tResults');

modeFare.addEventListener('click', () => switchMode('fare'));
modeTravel.addEventListener('click', () => switchMode('travel'));

function switchMode(mode) {
  const travel = mode === 'travel';
  modeFare.classList.toggle('active', !travel);
  modeTravel.classList.toggle('active', travel);
  fareMain.hidden = travel;
  travelPanel.hidden = !travel;
}

let tPollTimer = null;
let tPollFrom = '';

document.getElementById('travelForm').addEventListener('submit', async (e) => {
  e.preventDefault();
  const from = document.getElementById('tFrom').value.trim();
  if (!from) { tStatus.textContent = '请输入出发地'; return; }
  const days = Math.max(0, parseInt(document.getElementById('tDays').value, 10) || 0);
  const limit = Math.max(10, parseInt(document.getElementById('tLimit').value, 10) || 50);
  tStatus.textContent = '正在准备深度搜索...';
  tResults.innerHTML = '';
  try {
    const hs = document.getElementById('tHS').checked ? '1' : '';
    await api('/api/travel/start?from=' + encodeURIComponent(from) +
      '&days=' + days + '&limit=' + limit + (hs ? '&hardSleeper=1' : ''), { method: 'POST' });
    tPollFrom = from.trim();
    if (tPollTimer) clearInterval(tPollTimer);
    pollTravel();
    tPollTimer = setInterval(pollTravel, 800);
  } catch (err) {
    tStatus.textContent = '启动失败：' + err.message;
  }
});

async function pollTravel() {
  try {
    const p = await api('/api/travel/progress?from=' + encodeURIComponent(tPollFrom));
    if (p.err) {
      if (tPollTimer) { clearInterval(tPollTimer); tPollTimer = null; }
      tStatus.textContent = '搜索失败：' + p.err;
      document.getElementById('tBar').hidden = true;
      return;
    }
    // 若当前运行中的任务属于别的出发地，等待而非错显示结果
    if (p.running && p.from && p.from !== tPollFrom && p.from !== (tPollFrom + '|hs')) {
      tStatus.textContent = '另一出发地的搜索仍在进行，请稍候…';
      return;
    }
    const pct = p.total ? Math.min(100, Math.round(p.done * 100 / p.total)) : 0;
    if (p.running) {
      document.getElementById('tBar').hidden = false;
      document.getElementById('tBarFill').style.width = pct + '%';
      tStatus.textContent = '正在深度搜索全国往返方案… ' + p.done + ' / ' + p.total +
        ' 个目的地（' + pct + '%，约每目的地 1 秒）';
      return;
    }
    if (tPollTimer) { clearInterval(tPollTimer); tPollTimer = null; }
    document.getElementById('tBar').hidden = true;
    if (p.items && p.items.length) {
      renderTravel({ items: p.items, destCount: p.destCount });
    } else {
      tStatus.textContent = p.err || '没有找到可展示的目的地';
    }
  } catch (_) { /* 瞬时网络错误忽略，下轮重试 */ }
}

// —— 旅行模式结果排序：往返总价可切换最低/最高，性价比表头恢复默认 ——
let tItems = [];
let tSortMode = 'score'; // score | totalAsc | totalDesc

function tSortBy(mode) {
  if (mode === 'total') {
    tSortMode = (tSortMode === 'totalAsc') ? 'totalDesc' : 'totalAsc';
  } else {
    tSortMode = mode;
  }
  renderTravel({ items: tItems, destCount: tItems.length, keepSort: true });
}

function tSorted() {
  const arr = tItems.slice();
  if (tSortMode === 'totalAsc') arr.sort((a, b) => a.total - b.total);
  if (tSortMode === 'totalDesc') arr.sort((a, b) => b.total - a.total);
  return arr;
}

function renderTravel(res) {
  if (!res.keepSort) { tSortMode = 'score'; }
  tItems = res.items || [];
  const items = tSorted();
  if (!items.length) {
    tStatus.textContent = '没有找到可展示的目的地（该出发方向的真实票价尚未抓取）';
    return;
  }
  tStatus.textContent = '共 ' + (res.destCount || items.length) + ' 个目的地完成深度搜索（含中转方案，与直达同场竞争），展示 ' + items.length + ' 个' +
    (tSortMode === 'score' ? '，按性价比排序' : tSortMode === 'totalAsc' ? '，按往返总价从低到高' : '，按往返总价从高到低');

  const totalArrow = tSortMode === 'totalAsc' ? '▲' : (tSortMode === 'totalDesc' ? '▼' : '▲▼');
  const scoreArrow = tSortMode === 'score' ? '★' : '☆';

  const rows = items.map((it) => {
    const isBest = tItems.length && it === tItems[0] && tSortMode === 'score';
    const tag = isBest ? '<span class="best">★ 最优</span>' : '';
    const plan = '<b>' + esc(it.legsText) + '</b>' +
      '<br><span class="sub">' + it.oneWayMinutes + ' 分钟 ｜ 单程 ¥' + it.oneWayPrice.toFixed(1) +
      (it.savePercent > 0.05 ? ' ｜ <span class="cheap">较直达省 ' + it.savePercent.toFixed(1) + '%</span>' : '') + '</span>';
    const direct = it.directPrice > 0
      ? '直达最低 ¥' + it.directPrice.toFixed(1)
      : '<span class="sub">无直达</span>';
    return '<tr>' +
      '<td class="rank">' + (items.indexOf(it) + 1) + tag + '</td>' +
      '<td><b>' + esc(it.city) + '</b><br><span class="sub">' + esc(it.destStation) + '</span></td>' +
      '<td class="plan">' + plan + '</td>' +
      '<td>' + direct + '</td>' +
      '<td class="total">¥' + it.total.toFixed(1) + '</td>' +
      '<td>' + it.km + ' km<br><span class="sub">往返 ' + it.unitPrice.toFixed(2) + ' 元/km<br>均速 ' + it.avgSpeed.toFixed(0) + ' km/h</span></td>' +
      '<td class="score">' + it.score.toFixed(1) + '</td>' +
      '</tr>';
  }).join('');
  tResults.innerHTML =
    '<table class="ttable"><thead><tr>' +
    '<th>#</th><th>目的地</th><th>最优方案（含中转）</th><th>直达对照</th>' +
    '<th class="sortable" onclick="tSortBy(&quot;total&quot;)">往返总价 <span class="arrow">' + totalArrow + '</span></th>' +
    '<th>里程/单价</th>' +
    '<th class="sortable" onclick="tSortBy(&quot;score&quot;)">性价比 <span class="arrow">' + scoreArrow + '</span></th>' +
    '</tr></thead><tbody>' + rows + '</tbody></table>';
}
