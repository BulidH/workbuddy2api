/* app.js — WorkBuddy2API 控制台前端（无构建步骤，原生 ES2020） */

const API = '/panel/api';

/* ── 基础工具 ───────────────────────────────────────── */
const $ = (s, r = document) => r.querySelector(s);
const $$ = (s, r = document) => [...r.querySelectorAll(s)];

function esc(s) {
  return String(s ?? '').replace(/[&<>"']/g, c =>
    ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
}

async function api(path, { method = 'GET', body } = {}) {
  const res = await fetch(API + path, {
    method,
    headers: body ? { 'Content-Type': 'application/json' } : undefined,
    body: body ? JSON.stringify(body) : undefined,
  });
  let data = {};
  try { data = await res.json(); } catch { /* 非 JSON 响应 */ }
  if (!res.ok || data.ok === false) {
    // 服务端的失败载荷有两种形态：{error}（真错误）与 {msg}（如登录轮询的
    // {ok:false,pending:true,msg:"尚未完成登录"}）。两者都要取，否则会把
    // 有用的提示吞掉、兜底显示成无意义的 "HTTP 200"。
    // 整个载荷挂到 error.payload 上，调用方据此区分「尚未完成」与「真失败」。
    const err = new Error(data.error || data.msg || `HTTP ${res.status}`);
    err.payload = data;
    err.status = res.status;
    throw err;
  }
  return data;
}

function toast(msg, type = 'ok', ms = 4200) {
  const t = document.createElement('div');
  t.className = `toast ${type}`;
  t.innerHTML = esc(msg);
  $('#toasts').appendChild(t);
  setTimeout(() => { t.style.opacity = '0'; setTimeout(() => t.remove(), 260); }, ms);
}

function modal(title, bodyHTML, footHTML = '') {
  $('#modal').innerHTML = `
    <div class="modal-head"><h3>${esc(title)}</h3>
      <button class="x-btn" type="button" data-close>&times;</button></div>
    <div class="modal-body">${bodyHTML}</div>
    ${footHTML ? `<div class="modal-foot">${footHTML}</div>` : ''}`;
  $('#modalBackdrop').hidden = false;
  $$('[data-close]').forEach(b => b.onclick = closeModal);
}
function closeModal() { $('#modalBackdrop').hidden = true; $('#modal').innerHTML = ''; }
$('#modalBackdrop').addEventListener('click', e => { if (e.target.id === 'modalBackdrop') closeModal(); });
document.addEventListener('keydown', e => { if (e.key === 'Escape') closeModal(); });

/* 配置路径读写 */
const getPath = (o, p) => p.split('.').reduce((a, k) => (a == null ? a : a[k]), o);
function setPath(o, p, v) {
  const ks = p.split('.'); const last = ks.pop();
  let cur = o;
  for (const k of ks) { if (typeof cur[k] !== 'object' || cur[k] === null) cur[k] = {}; cur = cur[k]; }
  cur[last] = v;
}

const fmtTime = ts => {
  if (!ts) return '—';
  const d = new Date(ts);
  return isNaN(d) ? '—' : d.toLocaleString('zh-CN', { hour12: false });
};
const fmtRemain = s => {
  if (!s || s <= 0) return '';
  if (s < 60) return `${s}s`;
  if (s < 3600) return `${Math.floor(s / 60)}m${s % 60 ? (s % 60) + 's' : ''}`;
  return `${Math.floor(s / 3600)}h${Math.floor((s % 3600) / 60)}m`;
};
// fmtSnap 把余额快照时刻转成「新鲜度」提示。
// 积分只在权威快照（每日签到 / 点「查询余额」）时与上游对齐，之后由每个成功请求的
// usage.credit 实时扣减——所以面板数字 = 快照余额 − 已发生的消耗。标明快照时刻，
// 用户才知道这个数字有多可信；空值 = 从未取到过快照（旧账号/重启前的旧 state.json）。
const fmtSnap = ts => {
  if (!ts) return '未校准';
  const d = new Date(ts);
  if (isNaN(d)) return '未校准';
  const mins = Math.floor((Date.now() - d.getTime()) / 60000);
  if (mins < 1) return '刚校准';
  if (mins < 60) return `快照 ${mins}m 前`;
  if (mins < 1440) return `快照 ${Math.floor(mins / 60)}h 前`;
  return `快照 ${Math.floor(mins / 1440)}d 前`;
};

/* ── 全局状态 ───────────────────────────────────────── */
// hideDisabled 默认 true：账号页只列可用账号，被禁用的号默认收起（历史上池里
// 累积了十几个老禁用号，会把真正要盯的号挤到下面）。开关状态存 localStorage，
// 刷新/切页保持；'0' 表示用户主动展开过，其余（含无记录）都按默认隐藏。
const HIDE_DISABLED_KEY = 'wb2a-hide-disabled';
const S = {
  route: 'overview', overview: null, accounts: [], config: null, timer: null,
  hideDisabled: localStorage.getItem(HIDE_DISABLED_KEY) !== '0',
};

// accountsDisplayList 账号表的「展示序」：
//   - 隐藏开关打开时过滤掉 disabled 账号；
//   - 展开时 disabled 账号整体沉到**所有状态账号的下边**，组内再按 disabled_at
//     倒序（刚被禁用的在已禁用组内部排上边）；非禁用组保持既有顺序
//     （Array.prototype.sort 自 ES2019 起稳定）。
// disabled_at 缺失（旧 state.json）记 0，落在同组末尾，不参与「最新」比较。
function accountsDisplayList(list) {
  const shown = S.hideDisabled ? list.filter(a => !a.disabled) : list;
  if (!shown.some(a => a.disabled)) return shown;
  return [...shown].sort((a, b) => {
    if (a.disabled !== b.disabled) return a.disabled ? 1 : -1;
    if (!a.disabled) return 0;
    const ta = Date.parse(a.disabled_at || '') || 0;
    const tb = Date.parse(b.disabled_at || '') || 0;
    if (ta !== tb) return tb - ta;
    return 0;
  });
}

/* ── 路由 ───────────────────────────────────────────── */
const ROUTES = {
  overview: { title: '概览', sub: '网关健康状态与账号池总览', render: viewOverview },
  accounts: { title: '账号管理', sub: '账号池详情、添加与移除', render: viewAccounts },
  qoder: { title: 'Qoder 活动', sub: '每日签到 / 活动领取 / 额度与活动流水', render: viewQoder },
  config: { title: '网关配置', sub: '在线编辑 config.json（保存后需重启生效）', render: viewConfig },
  models: { title: '模型列表', sub: '网关对外暴露的全部模型', render: viewModels },
  agents: { title: '智能体配置', sub: '检测本机 AI 客户端，一键指向本网关；写入前备份，可精确还原', render: viewAgents },
  usage: { title: '用量统计', sub: '按账号 / 小时 / 天 / 模型统计积分消耗与调用量', render: viewUsage },
  logs: { title: '运行日志', sub: '网关进程日志与请求流水', render: viewLogs },
};

// go 由导航点击调用：只负责把 hash 改成目标路由，渲染统一交给 hashchange。
//
// 反例（曾经的 bug）：先赋 S.route 再改 hash，会导致 hashchange 里
// `h !== S.route` 守卫不成立而跳过 render —— 页面永远不刷新。
// 现在 hash 是唯一真相源；hash 没变时（重复点击当前项）手动渲染兜底。
function go(route) {
  const r = ROUTES[route] ? route : 'overview';
  if (location.hash.replace('#', '') === r) { render(); return; }
  location.hash = r;
}

async function render() {
  const r = ROUTES[S.route];
  $('#pageTitle').textContent = r.title;
  $('#pageSub').textContent = r.sub;
  $$('.nav-item').forEach(n => n.classList.toggle('active', n.dataset.route === S.route));
  try {
    await r.render();
  } catch (e) {
    $('#view').innerHTML = `<div class="alert err">加载失败：${esc(e.message)}</div>`;
  }
}

window.addEventListener('hashchange', () => {
  const h = location.hash.replace('#', '');
  S.route = ROUTES[h] ? h : 'overview';
  render(); // 无条件渲染：hash 变了就该换页
});

/* ── 概览 ───────────────────────────────────────────── */
async function viewOverview() {
  const [ov, acc] = await Promise.all([api('/overview'), api('/accounts')]);
  S.overview = ov; S.accounts = acc.accounts || [];
  $('#navAccCount').textContent = S.accounts.length || '';
  setHealth(ov.healthy, ov.counts);

  const c = ov.counts;
  const rt = ov.realm_totals || {};
  const g = (rt.global || {}), n = (rt.cn || {});
  const qd = (ov.providers || {}).qoder;
  const lm = (ov.providers || {}).loomy;

  const card = (label, value, foot, cls = '') =>
    `<div class="card stat ${cls}"><div class="stat-label">${label}</div>
     <div class="stat-value">${value}</div><div class="stat-foot">${foot}</div></div>`;

  $('#view').innerHTML = `
    ${c.credential_mismatch ?? (c.auth_files !== c.total) ? `
      <div class="alert warn">
        <b>凭证文件数与池内账号数不一致（WorkBuddy ${c.auth_files_workbuddy ?? c.auth_files}/${c.pool_workbuddy ?? c.total}，Qoder ${c.auth_files_qoder ?? 0}/${c.pool_qoder ?? 0}，Loomy ${c.auth_files_loomy ?? 0}/${c.pool_loomy ?? 0}）。</b>
        可能有凭证文件解析失败或未被加载。点击下方「重载账号」重新对齐各 Provider 的账号池。
        <button class="btn sm" style="margin-left:8px" id="reloadAcc">重载账号</button>
      </div>` : ''}

    <div class="grid cols-4">
      ${card('健康账号', c.healthy, `共 ${c.total} 个账号`, c.healthy > 0 ? 'ok' : 'err')}
      ${card('冷却中', c.cooling, '触发限流 / 熔断', c.cooling > 0 ? 'warn' : '')}
      ${card('已禁用', c.disabled, '连续失败达阈值', c.disabled > 0 ? 'err' : '')}
      ${card('在途占满', c.in_flight, `粘性会话 ${c.sticky ?? 0}`, '')}
    </div>

    <div class="grid cols-2" style="margin-top:16px">
      <div class="card">
        <div class="card-head"><h3>按 Provider 可用性</h3><span class="sub">模型名前缀决定路由</span></div>
        <div class="card-body">
          <table class="tbl">
            <thead><tr><th>上游</th><th>前缀</th><th class="num">总数</th><th class="num">健康</th><th>状态</th></tr></thead>
            <tbody>
              <tr><td><span class="tag purple">WorkBuddy 国内版</span></td><td class="mono">cn:</td>
                  <td class="num">${n.total || 0}</td><td class="num">${n.healthy || 0}</td>
                  <td>${n.healthy > 0 ? '<span class="tag ok"><i class="tdot"></i>可服务</span>'
                                    : '<span class="tag gray">无可用账号</span>'}</td></tr>
              <tr><td><span class="tag info">WorkBuddy 国际版</span></td><td class="mono">global:</td>
                  <td class="num">${g.total || 0}</td><td class="num">${g.healthy || 0}</td>
                  <td>${g.healthy > 0 ? '<span class="tag ok"><i class="tdot"></i>可服务</span>'
                                    : '<span class="tag gray">无可用账号</span>'}</td></tr>
              ${qd ? `<tr><td><span class="tag ok">Qoder</span></td><td class="mono">qoder:</td>
                  <td class="num">${qd.total || 0}</td><td class="num">${qd.healthy || 0}</td>
                  <td>${qd.healthy > 0 ? '<span class="tag ok"><i class="tdot"></i>可服务</span>'
                                    : '<span class="tag gray">无可用账号</span>'}</td></tr>`
                  : `<tr><td><span class="tag gray">Qoder</span></td><td class="mono">qoder:</td>
                  <td class="num">—</td><td class="num">—</td><td><span class="tag gray">未启用</span></td></tr>`}
              ${lm ? `<tr><td><span class="tag info">Loomy</span></td><td class="mono">loomy:</td>
                  <td class="num">${lm.total || 0}</td><td class="num">${lm.healthy || 0}</td>
                  <td>${lm.healthy > 0 ? '<span class="tag ok"><i class="tdot"></i>可服务</span>'
                                    : '<span class="tag gray">无可用账号</span>'}</td></tr>`
                  : `<tr><td><span class="tag gray">Loomy</span></td><td class="mono">loomy:</td>
                  <td class="num">—</td><td class="num">—</td><td><span class="tag gray">未启用</span></td></tr>`}
            </tbody>
          </table>
        </div>
      </div>

      <div class="card">
        <div class="card-head"><h3>运行环境</h3></div>
        <div class="card-body">
          <dl class="kv">
            <dt>网关版本</dt><dd>${esc(ov.gateway_ver || 'dev')}</dd>
            <dt>面板版本</dt><dd>${esc(ov.panel_ver)}</dd>
            <dt>Redis 模式</dt><dd>${esc(ov.redis_mode || 'noop')}${ov.redis_mode === 'noop' ? '（纯内存）' : ''}</dd>
            <dt>配置文件</dt><dd>${esc(ov.paths.config)}</dd>
            <dt>凭证目录</dt><dd>${esc(ov.paths.auths)}</dd>
            <dt>状态文件</dt><dd>${esc(ov.paths.state)}</dd>
            <dt>服务器时间</dt><dd>${esc(fmtTime(ov.now))}</dd>
          </dl>
        </div>
      </div>
    </div>

    <div style="height:16px"></div>
    ${accountsTable(S.accounts, { compact: true })}
  `;

  const ra = $('#reloadAcc');
  if (ra) ra.onclick = async () => {
    try { const r = await api('/reload', { method: 'POST' });
      toast(`已重载，当前 ${r.accounts} 个账号`); render();
    } catch (e) { toast(e.message, 'err'); }
  };
  bindAccountActions(S.accounts);
}

function setHealth(ok, counts) {
  const p = $('#healthPill');
  p.className = 'health-pill ' + (ok ? 'ok' : 'bad');
  p.querySelector('span').textContent = ok
    ? `网关正常 · ${counts?.healthy ?? 0}/${counts?.total ?? 0} 可用`
    : '网关不可用（无健康账号）';
}

/* ── 账号表 ─────────────────────────────────────────── */
function accountsTable(list, { compact = false } = {}) {
  const shown = accountsDisplayList(list);
  if (!shown.length) {
    const empty = S.hideDisabled && list.some(a => a.disabled)
      ? `<div class="empty">当前没有可显示的账号（已隐藏 ${list.filter(a => a.disabled).length} 个已禁用账号）。关闭「隐藏已禁用」即可查看。</div>`
      : `<div class="empty">还没有任何账号，点击「添加账号」选择 WorkBuddy、Qoder 或 Loomy 接入。</div>`;
    return `<div class="card"><div class="card-head"><h3>账号池</h3>
      <div class="row-flex">${hideDisabledToggleHTML()}<button class="btn primary sm" data-add>+ 添加账号</button></div></div>
      ${empty}</div>`;
  }
  const maxCredit = Math.max(1, ...shown.map(a => a.credits || 0));
  const rows = shown.map(a => {
    let st = '<span class="tag ok"><i class="tdot"></i>健康</span>';
    if (a.disabled) st = `<span class="tag err" title="${esc(a.reason || '已禁用')}"><i class="tdot"></i>已禁用</span>`;
    else if (a.cooling) st = `<span class="tag warn"><i class="tdot"></i>冷却 ${esc(fmtRemain(a.cool_remaining_sec))}</span>`;
    else if (a.in_flight > 0) st = `<span class="tag info"><i class="tdot"></i>在途 ${a.in_flight}</span>`;

    const qoder = a.provider === 'qoder';
    const loomy = a.provider === 'loomy';
    const realm = qoder
      ? `<span class="tag ok">Qoder ${a.realm === 'intl' ? '国际' : '国内'}</span>`
      : loomy
        ? '<span class="tag info">Loomy 国内</span>'
        : (a.realm === 'global'
          ? '<span class="tag info">国际版</span>' : '<span class="tag purple">国内版</span>');
    const pct = Math.min(100, Math.round(((a.credits || 0) / maxCredit) * 100));

    return `<tr${a.disabled ? ' class="row-disabled"' : ''}>
      <td>${st}
          ${a.disabled && a.disabled_at ? `<div style="font-size:10.5px;color:var(--text-mute);margin-top:3px"
            title="禁用时刻；已禁用账号沉底，组内按此时间由近到远排上边">${esc(fmtTime(a.disabled_at))}</div>` : ''}</td>
      <td><div style="font-weight:500">${esc(a.nickname || '（无昵称）')}</div>
          <div class="mono" style="font-size:11px;color:var(--text-mute)">${esc((a.uid || '').slice(0, 18))}…</div>
          ${qoder && (a.plan || a.last_checkin) ? `<div style="font-size:11px;color:var(--text-mute)">
            ${a.plan ? '套餐 ' + esc(a.plan) : ''}${a.plan && a.last_checkin ? ' · ' : ''}${a.last_checkin ? '签到 ' + esc(a.last_checkin) : ''}</div>` : ''}
      </td>
      <td>${realm}</td>
      <td class="num">${a.credits ?? 0}<div class="bar ${a.credits > 0 ? 'ok' : ''}" style="margin-top:5px">
          <i style="width:${pct}%"></i></div>
          <div style="font-size:10.5px;color:var(--text-mute);margin-top:2px"
            title="积分在签到时与上游对齐，之后按每个成功请求的实测扣费实时递减；点「查询余额」可立即重新校准">${esc(fmtSnap(a.credits_at))}</div></td>
      <td class="num">${a.success_count || 0} / ${a.err_total || 0}</td>
      <td class="num" style="font-size:11.5px">${esc(a.expires_at ? fmtTime(a.expires_at * 1000) : '—')}</td>
      <td class="num" style="font-size:11.5px">${esc(a.last_success || '—')}</td>
      <td style="white-space:nowrap">
        <button class="btn xs ${a.disabled ? 'primary' : 'warn'}" data-toggle-enable="${esc(a.uid)}"
          data-provider="${esc(a.provider || 'workbuddy')}"
          data-enabled="${a.disabled ? '0' : '1'}" data-nick="${esc(a.nickname || a.uid)}"
          title="${a.disabled ? '解禁后重新参与选号（若上游仍在拦截会被再次禁用）' : '禁用后不再参与选号，重启后仍保持'}">
          ${a.disabled ? '解禁' : '禁用'}</button>
        <button class="btn xs danger" data-del="${esc(a.uid)}" data-provider="${esc(a.provider || 'workbuddy')}"
          data-nick="${esc(a.nickname || a.uid)}">移除</button>
      </td>
    </tr>`;
  }).join('');

  const disabledCount = list.filter(a => a.disabled).length;
  return `<div class="card">
    <div class="card-head">
      <h3>账号池 <span class="sub" style="font-weight:400">（${shown.length} 个${S.hideDisabled && disabledCount ? ` · 已隐藏 ${disabledCount} 个禁用` : ''}）</span></h3>
      <div class="row-flex">
        ${hideDisabledToggleHTML()}
        ${disabledCount ? `<button class="btn sm warn" data-enable-all
          title="解禁所有被禁用的账号（包括当前被隐藏的；重新参与选号）">全部解禁（${disabledCount}）</button>` : ''}
        <button class="btn sm" data-balance
          title="只读查询全部已启用上游（WorkBuddy / Qoder / Loomy）的账号余额；不发签到请求，不触发任何自动调用">查询余额</button>
        <button class="btn sm" data-checkin
          title="执行签到流程（上游只对国内版生效，国际版会被 D4 门控跳过）">签到</button>
        ${list.some(a => a.provider === 'qoder') ? `<button class="btn sm" data-qcheckin-all
          title="对全部 Qoder 账号执行活动签到（活动领取 + Credits）">Qoder 签到</button>` : ''}
        ${list.some(a => a.provider === 'loomy') ? `<button class="btn sm" data-lcheckin-all
          title="对全部 Loomy 账号执行签到（每日积分 + 新手任务领取）">Loomy 签到</button>` : ''}
        <button class="btn primary sm" data-add>+ 添加账号</button>
      </div>
    </div>
    <div class="table-wrap"><table class="tbl">
      <thead><tr>
        <th>状态</th><th>账号</th><th>域</th><th>积分</th>
        <th>成功/失败</th>${compact ? '' : '<th>凭证到期</th>'}<th>最近成功</th><th>操作</th>
      </tr></thead>
      <tbody>${rows}</tbody>
    </table></div>
  </div>`;
}

// hideDisabledToggleHTML 账号表头的「隐藏已禁用」开关；默认开，状态存 localStorage。
function hideDisabledToggleHTML() {
  return `<label class="field row" style="gap:7px;white-space:nowrap;cursor:pointer"
      title="默认隐藏已禁用账号；关闭后它们沉到列表底部，组内按禁用时刻由近到远排上边">
    <label class="switch sm">
      <input type="checkbox" id="hideDisabled" data-hide-disabled ${S.hideDisabled ? 'checked' : ''}>
      <span></span></label>
    <label for="hideDisabled" style="font-size:12px;cursor:pointer">隐藏已禁用</label>
  </label>`;
}

function bindAccountActions(list) {
  $$('[data-hide-disabled]').forEach(cb => cb.onchange = () => {
    S.hideDisabled = cb.checked;
    localStorage.setItem(HIDE_DISABLED_KEY, cb.checked ? '1' : '0');
    render();
  });
  $$('[data-del]').forEach(b => b.onclick = () =>
    confirmDelete(b.dataset.del, b.dataset.nick, b.dataset.provider));
  $$('[data-add]').forEach(b => b.onclick = addAccountFlow);
  $$('[data-checkin]').forEach(b => b.onclick = runCheckin);
  $$('[data-balance]').forEach(b => b.onclick = runBalance);
  $$('[data-toggle-enable]').forEach(b => b.onclick = () =>
    toggleAccountEnabled(b.dataset.toggleEnable, b.dataset.enabled === '0', b.dataset.nick, b));
  $$('[data-enable-all]').forEach(b => b.onclick = () => enableAllDisabled(list || S.accounts, b));
  $$('[data-qcheckin-all]').forEach(b => b.onclick = async () => {
    b.disabled = true;
    try {
      const res = await api('/qoder/checkin', { method: 'POST', body: { only_daily: false } });
      const s = res.summary || {};
      toast(`Qoder 签到完成：${s.total || 0} 个账号，成功 ${(s.ok || 0) + (s.already || 0)}，失败 ${s.fail || 0}${s.earned_credit ? '，+' + s.earned_credit + ' 积分' : ''}`,
        s.fail ? 'warn' : 'ok', 8000);
      await render();
    } catch (e) { toast(e.message, 'err', 7000); b.disabled = false; }
  });
  $$('[data-lcheckin-all]').forEach(b => b.onclick = async () => {
    b.disabled = true;
    try {
      const res = await api('/loomy/checkin', { method: 'POST', body: {} });
      const s = res.summary || {};
      toast(`Loomy 签到完成：${s.total || 0} 个账号，成功 ${(s.ok || 0) + (s.already || 0)}，失败 ${s.fail || 0}${s.earned_points ? '，+' + s.earned_points + ' 分' : ''}`,
        s.fail ? 'warn' : 'ok', 8000);
      await render();
    } catch (e) { toast(e.message, 'err', 7000); b.disabled = false; }
  });
}

// toggleAccountEnabled 手动禁用 / 解禁单个账号。
// enable=true 表示当前是禁用态、点击后要解禁。
async function toggleAccountEnabled(uid, enable, nick, btn) {
  const label = enable ? '解禁' : '禁用';
  const provider = btn ? btn.dataset.provider : '';
  const old = btn ? btn.textContent : '';
  if (btn) { btn.disabled = true; btn.textContent = label + '中…'; }
  try {
    const r = await api('/accounts/enable', { method: 'POST', body: { uid, enabled: enable, provider } });
    toast(`${nick} ${r.msg || '已' + label}`);
    await render();
  } catch (e) {
    toast(e.message, 'err', 7000);
    if (btn) { btn.disabled = false; btn.textContent = old; }
  }
}

// enableAllDisabled 一键解禁当前所有被禁用的账号（逐个调用，串行避免并发写池）。
async function enableAllDisabled(list, btn) {
  const targets = (list || []).filter(a => a.disabled);
  if (!targets.length) { toast('没有被禁用的账号'); return; }
  if (btn) { btn.disabled = true; btn.textContent = '解禁中…'; }
  let okN = 0, failN = 0;
  for (const a of targets) {
    try {
      await api('/accounts/enable', { method: 'POST', body: { uid: a.uid, enabled: true, provider: a.provider } });
      okN++;
    } catch { failN++; }
  }
  toast(`已解禁 ${okN} 个` + (failN ? `，失败 ${failN} 个` : ''), failN ? 'warn' : 'ok', 7000);
  await render();
}

// PROVIDER_LABEL / providerTag 统一三种上游在提示与表格里的显示口径。
const PROVIDER_LABEL = { workbuddy: 'WorkBuddy', qoder: 'Qoder', loomy: 'Loomy' };
function providerTag(p) {
  if (p === 'qoder') return '<span class="tag ok">Qoder</span>';
  if (p === 'loomy') return '<span class="tag info">Loomy</span>';
  return '<span class="tag purple">WorkBuddy</span>';
}

// runBalance 只读查询所有账号余额（含国际版）。
// 与「签到」的分工：上游 D4 门控让 global 账号不参与签到任务、不对该域发起任何
// 自动调用以免触发风控；这里保持门控不动，只做用户显式点击触发的一次只读查询。
async function runBalance() {
  const btns = $$('[data-balance]');
  btns.forEach(b => { b.disabled = true; b.textContent = '查询中…'; });
  try {
    const r = await api('/balance', { method: 'POST' });
    const outs = r.outcomes || [];
    const got = outs.filter(o => o.credits !== null && o.credits !== undefined);
    const fail = outs.filter(o => o.error);
    const byProv = r.summary?.by_provider || {};
    const provText = Object.keys(byProv).length
      ? '（' + Object.entries(byProv).map(([k, v]) => `${PROVIDER_LABEL[k] || k} ${v}`).join(' · ') + '）' : '';
    const detail = got.map(o => `${o.nickname || o.uid.slice(0, 8)} = ${o.credits}`).join('，');
    toast(`余额查询完成：成功 ${got.length} · 失败 ${fail.length}${provText}` + (detail ? `；${detail}` : ''),
      fail.length ? 'warn' : 'ok', 9000);
    if (fail.length) {
      modal('余额查询明细', `
        <div class="alert warn">以下账号未能取到余额：</div>
        <div class="table-wrap"><table class="tbl">
          <thead><tr><th>账号</th><th>上游</th><th>域</th><th>原因</th></tr></thead>
          <tbody>${fail.map(o => `<tr>
            <td>${esc(o.nickname || o.uid.slice(0, 12))}</td>
            <td>${providerTag(o.provider)}</td>
            <td><span class="tag ${o.realm === 'global' ? 'info' : 'purple'}">${esc(o.realm || '—')}</span></td>
            <td style="font-size:12px;color:var(--text-dim)">${esc(o.error)}</td>
          </tr>`).join('')}</tbody>
        </table></div>`, `<button class="btn primary" data-close>知道了</button>`);
      $$('[data-close]').forEach(b => b.onclick = () => { closeModal(); render(); });
      return;
    }
    await render();
  } catch (e) {
    toast(e.message, 'err', 8000);
    btns.forEach(b => { b.disabled = false; b.textContent = '查询余额'; });
  }
}

// runCheckin 手动触发一轮签到 + 余额查询。
// 必要性：定时任务只在 schedule.checkin_hours（默认 9/21 点）触发，且调度器启动时不跑，
// 新加的账号在下一个整点前积分一直是 0，看起来像功能坏了。
async function runCheckin() {
  const btns = $$('[data-checkin]');
  btns.forEach(b => { b.disabled = true; b.textContent = '查询中…'; });
  try {
    const r = await api('/checkin', { method: 'POST' });
    const s = r.summary || {};
    const outs = r.outcomes || [];
    const balance = outs.filter(o => o.credits !== null && o.credits !== undefined)
      .map(o => `${o.nickname || o.uid.slice(0, 8)} = ${o.credits}`).join('，');
    toast(`签到完成：成功 ${s.ok || 0} · 已签到 ${s.already || 0} · 失败 ${s.fail || 0} · 跳过 ${s.skipped || 0}`
      + (balance ? `；余额：${balance}` : '；未取到余额'), s.fail ? 'warn' : 'ok', 9000);

    // 有失败时把逐账号原因列出来（比 toast 更能说明问题）
    const bad = outs.filter(o => o.status === 'fail' || o.status === 'skipped');
    if (bad.length) {
      modal('签到明细', `
        <div class="alert warn">以下账号未能取到余额，明细如下：</div>
        <div class="table-wrap"><table class="tbl">
          <thead><tr><th>账号</th><th>结果</th><th>原因</th></tr></thead>
          <tbody>${bad.map(o => `<tr>
            <td>${esc(o.nickname || o.uid.slice(0, 12))}</td>
            <td><span class="tag ${o.status === 'fail' ? 'err' : 'gray'}">${esc(o.status)}</span></td>
            <td style="font-size:12px;color:var(--text-dim)">${esc(o.detail || '—')}</td>
          </tr>`).join('')}</tbody>
        </table></div>`, `<button class="btn primary" data-close>知道了</button>`);
      $$('[data-close]').forEach(b => b.onclick = () => { closeModal(); render(); });
      return;
    }
    await render();
  } catch (e) {
    toast(e.message, 'err', 8000);
    btns.forEach(b => { b.disabled = false; b.textContent = '查询余额 / 签到'; });
  }
}

function confirmDelete(uid, nick, provider = 'workbuddy') {
  const dir = provider === 'qoder' ? 'qoder_auths/' : 'auths/';
  modal('移除账号', `
    <div class="alert warn">将从 <code>${dir}</code> 删除该账号的凭证文件，并立即从账号池移除。
      删除前会自动备份到 <code>backups/</code>。</div>
    <dl class="kv"><dt>昵称</dt><dd>${esc(nick)}</dd><dt>UID</dt><dd>${esc(uid)}</dd>
      <dt>上游</dt><dd>${provider === 'qoder' ? 'Qoder' : 'WorkBuddy'}</dd></dl>`,
    `<button class="btn" data-close>取消</button>
     <button class="btn danger" id="doDel">确认移除</button>`);
  $('#doDel').onclick = async () => {
    try {
      const r = await api('/accounts/delete', { method: 'POST', body: { uid, provider } });
      closeModal(); toast(`已移除，剩余 ${r.accounts} 个账号`); render();
    } catch (e) { toast(e.message, 'err'); }
  };
}

/* ── 账号管理页 ─────────────────────────────────────── */
async function viewAccounts() {
  const r = await api('/accounts');
  S.accounts = r.accounts || [];
  $('#navAccCount').textContent = S.accounts.length || '';
  $('#view').innerHTML = `
    <div class="alert info">
      账号池按「积分占比 ×10 + 闲置补偿 + 成功率 ×3 + 快过期积分 ×8」加权选号。
      加号后<b>无需重启</b>，凭证落盘即自动入池。
    </div>
    ${accountsTable(S.accounts)}`;
  bindAccountActions(S.accounts);
}

/* ── Qoder 活动（签到 / 活动领取 / 额度）────────────── */
const qoderSafeURL = u => (/^https?:\/\//i.test(u || '') ? u : '');

async function viewQoder() {
  const r = await api('/accounts');
  S.accounts = r.accounts || [];
  const qs = S.accounts.filter(a => a.provider === 'qoder');
  if (!qs.length) {
    $('#view').innerHTML = `<div class="card"><div class="empty">
      Qoder 未启用或还没有 Qoder 账号。请在「账号管理」添加 Qoder 账号
      （设备授权 / PAT / 账号文件导入）。</div></div>`;
    return;
  }
  const uid = qs.some(a => a.uid === S.qoderUID) ? S.qoderUID : qs[0].uid;
  S.qoderUID = uid;
  const acct = qs.find(a => a.uid === uid) || qs[0];

  let camp = null, campErr = '';
  try {
    camp = (await api('/qoder/campaigns?uid=' + encodeURIComponent(uid))).result;
  } catch (e) { campErr = e.message; }
  let act = { entries: [] };
  try { act = await api('/qoder/activity?limit=100'); } catch { /* 无流水不算错误 */ }

  const statusTag = s => {
    const map = { CLAIMABLE: ['ok', '可领取'], CLAIMED: ['gray', '已领取'],
      BLOCKED: ['err', '被拦截'], PENDING: ['warn', '待发放'], LOCKED: ['warn', '未解锁'] };
    const [cls, label] = map[(s || '').toUpperCase()] || ['gray', s || '—'];
    return `<span class="tag ${cls}">${esc(label)}</span>`;
  };

  const campRows = (camp?.campaigns || []).map(c => {
    const claimable = (c.claim_status || '').toUpperCase() === 'CLAIMABLE';
    const url = qoderSafeURL(c.detail_url);
    return `<tr>
      <td><div style="font-weight:500">${esc(c.title || c.key || c.id)}</div>
        <div class="mono" style="font-size:11px;color:var(--text-mute)">${esc(c.key || c.id)}</div></td>
      <td>${esc(c.benefit_kind || '—')}${c.benefit_amount ? ' · ' + c.benefit_amount : ''}</td>
      <td>${statusTag(c.claim_status)}</td>
      <td style="font-size:11.5px;color:var(--text-mute)">${esc(c.unavailable_reason || c.required_achievement_key || '')}</td>
      <td style="white-space:nowrap">
        ${claimable ? `<button class="btn xs primary" data-claim="${esc(c.id)}"
          data-title="${esc(c.title || c.key || c.id)}">领取</button>` : ''}
        ${url ? `<a class="btn xs" href="${esc(url)}" target="_blank" rel="noopener noreferrer">详情</a>` : ''}
      </td></tr>`;
  }).join('');

  const codes = Object.entries(acct.credits_detail?.campaign_codes || {});
  const pkgs = (acct.credits_detail?.packages || []).map(p =>
    `<tr><td>${esc(p.label || p.kind || '—')}</td>
      <td class="num">${p.unlimited ? '∞' : (p.remain ?? 0)}</td>
      <td class="num">${p.size ?? 0}</td></tr>`).join('');

  const actRows = (act.entries || []).slice().reverse().map(e => `<tr>
    <td class="mono" style="font-size:11.5px">${esc(e.time ? fmtTime(e.time) : '—')}</td>
    <td>${esc(e.nickname || e.uid || '')}</td>
    <td>${esc(e.kind || '')}${e.campaign ? ' · ' + esc(e.campaign) : ''}</td>
    <td class="num">${e.credits ? '+' + e.credits : '—'}</td>
    <td>${statusTag(e.status)}</td>
    <td style="font-size:11.5px;color:var(--text-mute)">${esc(e.message || '')}</td>
  </tr>`).join('');

  const opts = qs.map(a => `<option value="${esc(a.uid)}" ${a.uid === uid ? 'selected' : ''}>
    ${esc(a.nickname || a.uid.slice(0, 12))} · ${a.realm === 'intl' ? '国际' : '国内'}${a.disabled ? '（已禁用）' : ''}
  </option>`).join('');

  $('#view').innerHTML = `
    <div class="alert info">
      签到走官方活动平台：先列出活动，再对可领取项 POST claim（幂等）。每日 10:00（UTC+8）
      刷新名额；上游按「人」去重时本轮会显示 blocked，属正常。
    </div>

    <div class="card">
      <div class="card-head">
        <h3>运营操作</h3>
        <div class="row-flex">
          <select class="input" id="qAccount" style="min-width:260px">${opts}</select>
          <button class="btn sm" data-qcheckin="daily" title="只领 Credits 类活动（每日签到）">每日签到</button>
          <button class="btn sm" data-qcheckin="full" title="领取所有可领活动（含兑换码 / 券）">全额领取</button>
          <button class="btn sm" data-qbalance title="只读刷新额度与套餐">刷新额度</button>
          <button class="btn sm" data-qrefresh title="强制重新拉取活动列表">刷新活动</button>
        </div>
      </div>
      <div class="card-body">
        <div class="row-flex" style="gap:16px;flex-wrap:wrap;font-size:12.5px">
          <span>套餐：<b>${esc(acct.plan || '—')}</b></span>
          <span>最近签到：<b>${esc(acct.last_checkin || '—')}</b></span>
          <span>余额：<b>${acct.credits ?? 0}</b></span>
          <label class="row-flex" style="gap:6px;cursor:pointer">
            <input type="checkbox" id="qPrefCheckin" ${acct.checkin_when_disabled ? 'checked' : ''}>
            账号被禁用时也参与签到
          </label>
        </div>
      </div>
    </div>

    <div style="height:16px"></div>
    <div class="card">
      <div class="card-head"><h3>活动任务中心</h3>
        <span class="sub">${camp ? (camp.claimable ? '有可领取项' : '当前无可领取项') : ''}</span></div>
      ${campErr ? `<div class="card-body"><div class="alert err">活动查询失败：${esc(campErr)}</div></div>`
        : campRows ? `<div class="table-wrap"><table class="tbl">
            <thead><tr><th>活动</th><th>奖励</th><th>状态</th><th>说明</th><th>操作</th></tr></thead>
            <tbody>${campRows}</tbody></table></div>`
        : `<div class="empty">当前没有活动可展示${camp && camp.hint ? '（' + esc(camp.hint) + '）' : ''}</div>`}
    </div>

    <div style="height:16px"></div>
    <div class="grid cols-2">
      <div class="card">
        <div class="card-head"><h3>额度构成</h3></div>
        <div class="card-body">
          <dl class="kv">
            <dt>剩余（有效）</dt><dd>${acct.credits_detail?.remain_effective ?? acct.credits ?? 0}</dd>
            <dt>已用 / 总量</dt><dd>${acct.credits_detail?.used ?? 0} / ${acct.credits_detail?.size ?? 0}</dd>
          </dl>
          ${pkgs ? `<table class="tbl" style="margin-top:8px">
            <thead><tr><th>资源包</th><th class="num">剩余</th><th class="num">总量</th></tr></thead>
            <tbody>${pkgs}</tbody></table>` : ''}
          ${codes.length ? `<div style="margin-top:10px;font-size:12.5px">
            兑换码：${codes.map(([k, v]) => `<span class="mono">${esc(k)}=${esc(v)}</span>`).join('、')}</div>` : ''}
        </div>
      </div>
      <div class="card">
        <div class="card-head"><h3>活动流水</h3><span class="sub">最近 100 条</span></div>
        <div class="table-wrap" style="max-height:320px;overflow:auto">
          ${actRows ? `<table class="tbl">
            <thead><tr><th>时间</th><th>账号</th><th>类型</th><th class="num">积分</th><th>状态</th><th>说明</th></tr></thead>
            <tbody>${actRows}</tbody></table>` : '<div class="empty">还没有活动记录</div>'}
        </div>
      </div>
    </div>`;

  $('#qAccount').onchange = e => { S.qoderUID = e.target.value; render(); };
  $('#qPrefCheckin').onchange = async e => {
    try {
      await api('/qoder/prefs', { method: 'POST', body: { uid, checkin_when_disabled: e.target.checked } });
      toast('偏好已保存');
      render();
    } catch (err) { toast(err.message, 'err'); e.target.checked = !e.target.checked; }
  };
  $$('[data-qcheckin]').forEach(b => b.onclick = async () => {
    const onlyDaily = b.dataset.qcheckin === 'daily';
    b.disabled = true;
    const old = b.textContent;
    b.textContent = '执行中…';
    try {
      const res = await api('/qoder/checkin', { method: 'POST', body: { uids: [uid], only_daily: onlyDaily } });
      const o = (res.outcomes || [])[0] || {};
      const earned = o.earned_credit ? ` +${o.earned_credit}` : '';
      toast(`签到 ${o.status || '完成'}${earned}${o.detail ? '：' + o.detail : ''}`,
        o.status === 'fail' ? 'err' : 'ok', 7000);
      render();
    } catch (err) {
      toast(err.message, 'err', 7000);
      b.disabled = false;
      b.textContent = old;
    }
  });
  $$('[data-qbalance]').forEach(b => b.onclick = async () => {
    b.disabled = true;
    try {
      const res = await api('/qoder/balance', { method: 'POST', body: { uids: [uid] } });
      const o = (res.outcomes || [])[0] || {};
      toast(o.error ? '刷新失败：' + o.error : `余额已刷新：${o.credits ?? 0}`, o.error ? 'err' : 'ok');
      render();
    } catch (err) { toast(err.message, 'err'); b.disabled = false; }
  });
  $$('[data-qrefresh]').forEach(b => b.onclick = async () => {
    b.disabled = true;
    try {
      await api('/qoder/campaigns?uid=' + encodeURIComponent(uid) + '&force=1');
      render();
    } catch (err) { toast(err.message, 'err'); b.disabled = false; }
  });
  $$('[data-claim]').forEach(b => b.onclick = async () => {
    b.disabled = true;
    try {
      const res = await api('/qoder/campaign/claim', { method: 'POST', body: { uid, campaign_id: b.dataset.claim } });
      const rr = res.result || {};
      toast(rr.ok || rr.replayed ? `已领取：${b.dataset.title}` : `领取未成功：${rr.message || rr.error || ''}`,
        rr.ok || rr.replayed ? 'ok' : 'warn', 7000);
      render();
    } catch (err) { toast(err.message, 'err', 7000); b.disabled = false; }
  });
}


/* ── 添加账号（WorkBuddy OAuth / Qoder / Loomy）──────── */
function addAccountFlow() {
  let provider = 'workbuddy';

  const renderProvider = () => {
    modal('添加账号 · 选择类型', `
      <p class="muted" style="margin-top:0">选择要接入的上游。同名模型会在多个上游之间混合调度。</p>
      <div class="radio-row">
        <div class="radio-card ${provider === 'workbuddy' ? 'sel' : ''}" data-pv="workbuddy">
          <strong>WorkBuddy</strong><small>国内版 / 国际版 · OAuth 登录</small></div>
        <div class="radio-card ${provider === 'qoder' ? 'sel' : ''}" data-pv="qoder">
          <strong>Qoder</strong><small>设备授权 / PAT / 账号 JSON 导入</small></div>
        <div class="radio-card ${provider === 'loomy' ? 'sel' : ''}" data-pv="loomy">
          <strong>Loomy</strong><small>讯飞 · 手机号密码 / 账号 JSON 导入</small></div>
      </div>`,
      `<button class="btn" data-close>取消</button><button class="btn primary" id="next0">下一步</button>`);
    $$('[data-pv]').forEach(c => c.onclick = () => { provider = c.dataset.pv; renderProvider(); });
    $('#next0').onclick = () => (provider === 'qoder' ? qoderFlow() : provider === 'loomy' ? loomyFlow() : workbuddyFlow());
  };

  const workbuddyFlow = () => {
  let realm = 'global';
  let state = null;
  let pollTimer = null;

  // 绑定必须写在 modal() **之后**：modal 负责把 HTML 注入 DOM，在那之前
  // $('#next1') 仍是 null，赋值 onclick 会抛 TypeError 并中断整个函数，
  // 结果是弹窗根本打不开（点「添加账号」毫无反应）。
  // 放进 renderStep1 内部还能保证每次重渲染（切换版本）后重新绑定。
  const renderStep1 = () => {
    modal('添加账号 · 第 1 步', `
    <p class="muted" style="margin-top:0">选择要登录的账号版本，随后在浏览器完成登录。</p>
    <div class="radio-row">
      <div class="radio-card ${realm === 'global' ? 'sel' : ''}" data-r="global">
        <strong>国际版 Global</strong><small>www.workbuddy.ai</small></div>
      <div class="radio-card ${realm === 'cn' ? 'sel' : ''}" data-r="cn">
        <strong>国内版 CN</strong><small>copilot.tencent.com</small></div>
    </div>
    <div class="alert warn" style="margin-top:16px">
      浏览器若已登录其他账号，点开链接会直接授权该账号。建议使用<b>无痕窗口</b>登录新账号。
    </div>`,
      `<button class="btn" data-close>取消</button><button class="btn primary" id="next1">生成授权链接</button>`);

    $$('[data-r]').forEach(c => c.onclick = () => { realm = c.dataset.r; renderStep1(); });
    $('#next1').onclick = async () => {
      try {
        const r = await api('/login/start', { method: 'POST', body: { realm } });
        state = r.state;
        renderStep2(r.url);
      } catch (e) { toast(e.message, 'err'); }
    };
  };
  renderStep1();

  function renderStep2(url) {
    modal('添加账号 · 第 2 步', `
      <div class="steps">
        <div class="step done">选择版本</div>
        <div class="step on">浏览器登录</div>
        <div class="step">完成</div>
      </div>
      <p style="margin-top:0">请在浏览器中打开下面的链接并完成登录（没账号就先注册）：</p>
      <div class="url-box">${esc(url)}</div>
      <p class="muted" style="font-size:12px">登录完成后点击下面的按钮，我会自动换取凭证并加入账号池。</p>
      <div id="loginMsg"></div>`,
      `<button class="btn" data-close>取消</button><button class="btn primary" id="pollBtn">我已完成登录</button>`);

    $('#pollBtn').onclick = async () => {
      const btn = $('#pollBtn'); btn.disabled = true; btn.textContent = '检查中…';
      $('#loginMsg').innerHTML = '';
      try {
        const r = await api(`/login/poll?state=${encodeURIComponent(state)}`);
        if (r.pending) {
          $('#loginMsg').innerHTML = `<div class="alert warn">${esc(r.msg)}</div>`;
          btn.disabled = false; btn.textContent = '再试一次';
          return;
        }
        renderStep3(r);
      } catch (e) {
        // pending（用户还没点完登录）不是错误，用黄色提示而非红色报错，
        // 并显示后端给的真实原因（如 "11217:login ing..."）。
        const p = e.payload || {};
        const pending = !!p.pending;
        $('#loginMsg').innerHTML =
          `<div class="alert ${pending ? 'warn' : 'err'}">${esc(p.msg || e.message)}</div>`;
        btn.disabled = false; btn.textContent = '再试一次';
      }
    };
  }

  function renderStep3(r) {
    const reg = r.register ? `<dt>注册激活</dt><dd>${esc(r.register)}</dd>` : '';
    const trial = r.trial ? `<dt>Trial 加油包</dt><dd>${esc(r.trial)}</dd>` : '';

    if (r.needs_region && r.countries) {
      const det = r.detected_region?.ios2;
      modal('添加账号 · 完善注册地区', `
        <div class="steps">
          <div class="step done">选择版本</div><div class="step done">浏览器登录</div>
          <div class="step on">完善地区</div>
        </div>
        <div class="alert warn">该国际版账号需要先完善注册地区，否则聊天会报 <code>14017</code>。
          ${det ? `检测到当前地区为 <b>${esc(det)}</b>，已为你预选。` : ''}</div>
        <div class="radio-row" id="ctry">
          ${r.countries.map(c => `<div class="radio-card ${c.ios2 === det ? 'sel' : ''}" data-ios2="${esc(c.ios2)}"
             data-en="${esc(c.en_name)}" data-code="${esc(c.code)}">
             <strong>${esc(c.ios2)}</strong><small>${esc(c.en_name)}</small></div>`).join('')}
        </div>
        <div id="regionMsg"></div>`,
        `<button class="btn primary" id="doRegion">提交并完成</button>`);
      let sel = r.countries.find(c => c.ios2 === det) || r.countries[0];
      $$('#ctry .radio-card').forEach(el => el.onclick = () => {
        $$('#ctry .radio-card').forEach(x => x.classList.remove('sel'));
        el.classList.add('sel');
        sel = { ios2: el.dataset.ios2, en_name: el.dataset.en, code: el.dataset.code };
      });
      $('#doRegion').onclick = async () => {
        // 后端要串行打 3 个上游请求（提交地区 → 重新激活 → 领 trial），
        // 最坏情况要等几十秒。没有加载反馈用户会以为按钮坏了，故必须禁用 + 改文案。
        const btn = $('#doRegion');
        btn.disabled = true; btn.textContent = '提交中…';
        $('#regionMsg').innerHTML = '';
        try {
          const rr = await api('/login/region', { method: 'POST',
            body: { uid: r.uid, ios2: sel.ios2, en_name: sel.en_name, code: sel.code } });
          closeModal();
          toast(rr.msg || '地区已完善', 'ok');
          render();
        } catch (e) {
          // 错误就地显示在弹窗里（toast 在右下角，被弹窗挡着容易漏看）
          const msg = (e.payload && e.payload.msg) || e.message;
          $('#regionMsg').innerHTML = `<div class="alert err" style="margin-top:14px;margin-bottom:0">${esc(msg)}</div>`;
          btn.disabled = false; btn.textContent = '重新提交';
        }
      };
      return;
    }

    modal('添加账号 · 完成', `
      <div class="steps">
        <div class="step done">选择版本</div><div class="step done">浏览器登录</div>
        <div class="step done">完成</div>
      </div>
      <div class="alert ok">账号已添加并加入账号池。</div>
      <dl class="kv">
        <dt>昵称</dt><dd>${esc(r.nickname || '—')}</dd>
        <dt>UID</dt><dd>${esc(r.uid)}</dd>
        <dt>版本</dt><dd>${esc(r.realm)}</dd>
        ${reg}${trial}
        <dt>账号总数</dt><dd>${r.accounts}</dd>
      </dl>`,
      `<button class="btn primary" data-close>完成</button>`);
    $$('[data-close]').forEach(b => b.onclick = () => { closeModal(); render(); });
  };
  };

  // qoderFlow：设备授权 / PAT / 账号 JSON 三合一弹窗。
  const qoderFlow = () => {
    let realm = 'cn';
    let state = null;

    const renderQoder = () => {
      modal('添加 Qoder 账号', `
        <p class="muted" style="margin-top:0">设备授权最省事；已持有令牌可用 PAT；账号 JSON 支持批量导入。</p>
        <div class="radio-row">
          <div class="radio-card ${realm === 'cn' ? 'sel' : ''}" data-r="cn">
            <strong>国内版</strong><small>qoder.com.cn</small></div>
          <div class="radio-card ${realm === 'intl' ? 'sel' : ''}" data-r="intl">
            <strong>国际版</strong><small>qoder.com</small></div>
        </div>
        <div class="row-flex" style="margin-top:16px">
          <button class="btn primary sm" id="qLogin">设备授权登录</button>
        </div>
        <div id="qLoginBox" style="margin-top:10px"></div>

        <details class="form-section" style="margin-top:16px">
          <summary>个人访问令牌（PAT）</summary>
          <div class="field" style="margin-top:8px">
            <label>PAT（pt- 开头）</label>
            <input type="password" id="qPat" autocomplete="off" placeholder="pt-...">
          </div>
          <div class="row-flex" style="gap:8px">
            <button class="btn sm" id="qPatBtn">导入 PAT</button>
            <span class="muted" id="qPatMsg" style="font-size:12px;align-self:center"></span>
          </div>
        </details>

        <details class="form-section">
          <summary>账号文件导入（JSON）</summary>
          <div class="field" style="margin-top:8px">
            <label>粘贴 qoder-*.json 内容（单个对象 / 数组 / {"accounts":[...]}）</label>
            <textarea id="qJson" rows="6" class="mono"
              placeholder='{"uid":"...","accessToken":"...","refreshToken":"drt-..."}'></textarea>
          </div>
          <div class="row-flex" style="gap:8px">
            <button class="btn sm" id="qImpBtn">导入</button>
            <span class="muted" id="qImpMsg" style="font-size:12px;align-self:center"></span>
          </div>
        </details>`,
        `<button class="btn" data-close>关闭</button>`);

      $$('[data-r]').forEach(c => c.onclick = () => { realm = c.dataset.r; renderQoder(); });

      $('#qLogin').onclick = async () => {
        const box = $('#qLoginBox');
        box.innerHTML = '<div class="muted">正在生成授权链接…</div>';
        try {
          const r = await api('/qoder/login/start', { method: 'POST', body: { realm } });
          state = r.state;
          box.innerHTML = `
            <div class="url-box">${esc(r.authUrl || r.url || '')}</div>
            <p class="muted" style="font-size:12px">在浏览器打开上面的链接完成授权，然后点击右侧按钮。</p>
            <div class="row-flex" style="gap:8px">
              <button class="btn primary sm" id="qPoll">我已完成授权</button>
              <span class="muted" id="qPollMsg" style="font-size:12px;align-self:center"></span>
            </div>`;
          $('#qPoll').onclick = pollQoder;
        } catch (e) {
          box.innerHTML = `<div class="alert err">${esc(e.message)}</div>`;
        }
      };

      $('#qPatBtn').onclick = async () => {
        const pat = $('#qPat').value.trim();
        if (!pat) { toast('请填写 PAT', 'err'); return; }
        const b = $('#qPatBtn');
        b.disabled = true; b.textContent = '导入中…';
        try {
          const r = await api('/qoder/pat', { method: 'POST', body: { pat, realm } });
          closeModal();
          toast(`已添加 Qoder 账号${r.account?.nickname ? '：' + r.account.nickname : ''}`);
          render();
        } catch (e) {
          toast(e.message, 'err', 8000);
          b.disabled = false; b.textContent = '导入 PAT';
        }
      };

      $('#qImpBtn').onclick = async () => {
        const raw = $('#qJson').value.trim();
        if (!raw) { toast('请粘贴账号 JSON', 'err'); return; }
        let payload;
        try { payload = JSON.parse(raw); } catch (e) { toast('JSON 解析失败：' + e.message, 'err'); return; }
        const b = $('#qImpBtn');
        b.disabled = true; b.textContent = '导入中…';
        try {
          const r = await api('/qoder/import', { method: 'POST', body: payload });
          closeModal();
          const n = (r.imported || []).length, bad = (r.errors || []).length;
          toast(`已导入 ${n} 个${bad ? `，失败 ${bad} 个` : ''}`, bad ? 'warn' : 'ok', 8000);
          render();
        } catch (e) {
          toast(e.message, 'err', 8000);
          b.disabled = false; b.textContent = '导入';
        }
      };
    };

    const pollQoder = async () => {
      const btn = $('#qPoll');
      if (btn) { btn.disabled = true; btn.textContent = '检查中…'; }
      const retry = (msg) => {
        $('#qPollMsg').textContent = msg || '';
        if (btn) { btn.disabled = false; btn.textContent = '再试一次'; }
      };
      try {
        const r = await api(`/qoder/login/poll?state=${encodeURIComponent(state)}`);
        if (r.status === 'pending') { retry(r.message || '尚未完成授权'); return; }
        closeModal(); toast('Qoder 账号已添加'); render();
      } catch (e) {
        const p = e.payload || {};
        retry(p.status === 'pending' ? (p.message || '尚未完成授权') : (p.message || e.message));
      }
    };

    renderQoder();
  };

  // loomyFlow：手机号 + 密码登录 / 账号 JSON 导入。
  // Loomy 没有 refresh token，14 天续期靠重新登录，所以必须存密码（文件 0600），
  // 面板这里不收验证码、也不接受"只给 session"的接入方式。
  const loomyFlow = () => {
    const renderLoomy = () => {
      modal('添加 Loomy 账号', `
        <p class="muted" style="margin-top:0">用 Loomy App 的手机号 + 密码登录。
          密码会连同会话一起写入本机账号文件（权限 600），仅用于 14 天后的自动续期。</p>
        <div class="field">
          <label>手机号</label>
          <input id="lPhone" inputmode="numeric" autocomplete="username" placeholder="13800000000">
        </div>
        <div class="field">
          <label>密码</label>
          <input type="password" id="lPwd" autocomplete="current-password" placeholder="登录密码">
        </div>
        <div class="field">
          <label>备注（可选）</label>
          <input id="lName" placeholder="缺省用脱敏手机号">
        </div>
        <div class="row-flex" style="gap:8px">
          <button class="btn primary sm" id="lLoginBtn">登录并添加</button>
          <span class="muted" id="lLoginMsg" style="font-size:12px;align-self:center"></span>
        </div>

        <details class="form-section" style="margin-top:16px">
          <summary>账号文件导入（JSON）</summary>
          <div class="field" style="margin-top:8px">
            <label>粘贴 loomy-*.json 内容（单个对象 / 数组 / {"accounts":[...]}）</label>
            <textarea id="lJson" rows="6" class="mono"
              placeholder='{"uid":"...","loginid":"138...","password":"...","accessToken":"..."}'></textarea>
          </div>
          <div class="row-flex" style="gap:8px">
            <button class="btn sm" id="lImpBtn">导入</button>
            <span class="muted" id="lImpMsg" style="font-size:12px;align-self:center"></span>
          </div>
        </details>`,
        `<button class="btn" data-close>关闭</button>`);

      $('#lLoginBtn').onclick = async () => {
        const loginid = $('#lPhone').value.trim();
        const password = $('#lPwd').value;
        const nickname = $('#lName').value.trim();
        if (!loginid || !password) { toast('请填写手机号与密码', 'err'); return; }
        const b = $('#lLoginBtn');
        b.disabled = true; b.textContent = '登录中…';
        $('#lLoginMsg').textContent = '正在登录上游…';
        try {
          const r = await api('/loomy/login', { method: 'POST', body: { loginid, password, nickname } });
          closeModal();
          const c = r.account || {};
          toast(`已添加 Loomy 账号${c.nickname ? '：' + c.nickname : ''}${c.credits ? '（积分 ' + c.credits + '）' : ''}`, 'ok', 7000);
          render();
        } catch (e) {
          // 错误留在弹窗里：toast 会被弹窗挡住，密码错这类问题需要当场看到
          $('#lLoginMsg').innerHTML = `<span style="color:var(--err)">${esc(e.message)}</span>`;
          b.disabled = false; b.textContent = '重试';
        }
      };

      $('#lImpBtn').onclick = async () => {
        const raw = $('#lJson').value.trim();
        if (!raw) { toast('请粘贴账号 JSON', 'err'); return; }
        let payload;
        try { payload = JSON.parse(raw); } catch (e) { toast('JSON 解析失败：' + e.message, 'err'); return; }
        const b = $('#lImpBtn');
        b.disabled = true; b.textContent = '导入中…';
        try {
          const r = await api('/loomy/import', { method: 'POST', body: payload });
          closeModal();
          const n = (r.imported || []).length, bad = (r.errors || []).length;
          toast(`已导入 ${n} 个${bad ? `，失败 ${bad} 个` : ''}`, bad ? 'warn' : 'ok', 8000);
          render();
        } catch (e) {
          toast(e.message, 'err', 8000);
          b.disabled = false; b.textContent = '导入';
        }
      };
    };

    renderLoomy();
  };

  renderProvider();
}

/* ── 配置页 ─────────────────────────────────────────── */
const HOURS = Array.from({ length: 24 }, (_, i) => i);

const SCHEMA = [
  { sec: '基础设置', desc: '网关监听与访问控制', fields: [
    { p: 'listen', l: '监听地址', t: 'text', h: '如 :7863；修改后需重启' },
    { p: 'api_key', l: 'API 密钥', t: 'secret', h: '客户端 Bearer Token；留空 = 不鉴权（公网务必设置）' },
    { p: 'server.max_body_mb', l: '请求体上限 (MB)', t: 'int', h: '超过直接返回 413，不再静默截断' },
    { p: 'server.max_rotate', l: '单请求最多换号次数', t: 'int', h: '默认 3。号多时可调大，避免「只试了前几个就报全部账号不可用」；代价是最坏延迟随之增长' },
  ]},
  { sec: '账号池调度', desc: '选号权重、熔断与并发', fields: [
    { p: 'pool.max_in_flight', l: '单号最大在途', t: 'int', h: '0 = 不限；占满的号不参与选号' },
    { p: 'pool.breaker_threshold', l: '熔断阈值（连续失败次数）', t: 'int', h: '' },
    { p: 'pool.breaker_cooldown', l: '熔断基础时长', t: 'text', h: '如 30m' },
    { p: 'pool.breaker_cooldown_max', l: '熔断退避封顶', t: 'text', h: '如 6h' },
    { p: 'pool.idle_weight_per_hour', l: '闲置补偿 / 小时', t: 'float', h: '每小时未用增加的权重' },
    { p: 'pool.idle_weight_max', l: '闲置补偿封顶', t: 'float', h: '' },
    { p: 'pool.expiring_soon', l: '快过期积分窗口', t: 'text', h: '如 168h = 7 天；此窗口内到期的积分优先消耗' },
    { p: 'pool.free_tier_bonus', l: '免费账号权重加成', t: 'float', h: '已实测免费的号在选号时的权重加成，默认 3.0。调大更偏向免费号，调小更均摊负载；0 = 完全平等竞争' },
  ]},
  { sec: '冷却策略', desc: '限流与错误的冷却时长', fields: [
    { p: 'cooldown.soft_rate', l: '软冷却基数', t: 'text', h: '429 限流起始冷却，如 600s' },
    { p: 'cooldown.soft_rate_max', l: '软冷却封顶', t: 'text', h: '指数退避上限，如 2h' },
  ]},
  { sec: '定时任务', desc: '六类任务的开关与执行时刻', fields: [
    { p: 'schedule.checkin_enabled', l: '签到 + 余额查询', t: 'bool', h: '余额恢复会自动解冻冷却账号' },
    { p: 'schedule.checkin_hours', l: '签到时刻', t: 'hours', h: '' },
    { p: 'schedule.activity_enabled', l: '活跃上报', t: 'bool', h: '点亮连登天数' },
    { p: 'schedule.activity_hours', l: '活跃上报时刻', t: 'hours', h: '' },
    { p: 'schedule.travel_enabled', l: '猫猫旅行', t: 'bool', h: '领养 / 派出 / 领奖' },
    { p: 'schedule.travel_hours', l: '猫猫旅行时刻', t: 'hours', h: '' },
    { p: 'schedule.keepalive_enabled', l: 'token 保活', t: 'bool', h: '' },
    { p: 'schedule.keepalive_hours', l: '保活时刻', t: 'hours', h: '' },
    { p: 'schedule.school_enabled', l: '开学季任务', t: 'bool', h: '活动下线自动跳过' },
    { p: 'schedule.school_hours', l: '开学季时刻', t: 'hours', h: '' },
    { p: 'schedule.cat_enabled', l: '夜猫子任务', t: 'bool', h: '夜间窗口自动补足' },
    { p: 'schedule.cat_hours', l: '夜猫子时刻', t: 'hours', h: '' },
  ]},
  { sec: '双域路由', desc: '国内版 / 国际版', fields: [
    { p: 'global.enabled', l: '启用国际版（global realm）', t: 'bool', h: '关闭 = 锁死纯国内版部署' },
    { p: 'global.chat_base', l: '国际版 chat 基址', t: 'text', h: '留空 = 默认 https://www.workbuddy.ai' },
    { p: 'global.billing_base', l: '国际版 billing 基址', t: 'text', h: '留空 = 默认' },
  ]},
  { sec: '上游请求', desc: '超时、指纹与出站头', fields: [
    { p: 'upstream.timeout_seconds', l: '短 RPC 超时 (秒)', t: 'int', h: 'refresh / 签到 / 余额 / 模型列表' },
    { p: 'upstream.header_timeout_seconds', l: 'SSE 首字节超时 (秒)', t: 'int', h: '0 = 回落短 RPC 超时' },
    { p: 'upstream.idle_timeout_seconds', l: 'SSE 空闲超时 (秒)', t: 'int', h: '流中持续吐数据会续命' },
    { p: 'upstream.client_name', l: '用量归属客户端名', t: 'text', h: '默认 WorkBuddy（对齐官方桌面端）；SaaS = 还原旧行为' },
    { p: 'upstream.client_version', l: '客户端版本段', t: 'text', h: '留空 = 内置默认' },
    { p: 'upstream.cli_version', l: 'CLI 版本段', t: 'text', h: '留空 = 内置默认' },
    { p: 'upstream.user_agent', l: '自定义 User-Agent', t: 'text', h: '留空 = 默认三段式' },
    { p: 'upstream.device_token', l: '设备风控 Token', t: 'secret', h: 'X-Device-Token 全局兜底；留空 = 不注入' },
    { p: 'upstream.device_token_file', l: '设备 Token 文件路径', t: 'text', h: '' },
    { p: 'upstream.passthrough_ip', l: '透传客户端 IP', t: 'bool', h: '默认关闭（不把内网 IP 暴露给上游）' },
  ]},
  { sec: '请求处理', desc: '提示词与指纹脱敏', fields: [
    { p: 'prompt.mode', l: '系统提示词模式', t: 'select', opts: [['passthrough', 'passthrough（透传客户端原始 system）'], ['custom', 'custom（网关替换为自有提示词）']], h: '' },
    { p: 'prompt.file', l: '自定义提示词文件', t: 'text', h: 'custom 模式下生效；留空 = 内置默认' },
    { p: 'features.sanitize_blacklist_fingerprints', l: '黑名单指纹脱敏', t: 'bool', h: '清洗出站请求体中的客户端指纹字段' },
  ]},
  { sec: '会话粘性', desc: '多轮对话绑定同一账号', fields: [
    { p: 'session_sticky.enabled', l: '启用会话粘性', t: 'bool', h: '避免多轮上下文跳号' },
    { p: 'session_sticky.ttl', l: '绑定 TTL', t: 'text', h: '如 30m' },
    { p: 'session_sticky.gc_interval', l: 'GC 周期', t: 'text', h: '如 5m' },
  ]},
  { sec: '状态镜像 (Upstash Redis)', desc: '可选；留空则纯内存', fields: [
    { p: 'upstash.url', l: 'Upstash URL', t: 'text', h: '留空 = 纯内存模式' },
    { p: 'upstash.token', l: 'Upstash Token', t: 'secret', h: '' },
  ]},
  { sec: 'Qoder 第二上游', desc: '可选；与 WorkBuddy 按模型名混合调度', fields: [
    { p: 'qoder.enabled', l: '启用 Qoder', t: 'bool', h: '关闭后 Qoder 账号与 qoder: 前缀均不可用' },
    { p: 'qoder.auth_dir', l: 'Qoder 账号目录', t: 'text', h: '默认 ./qoder_auths；与 qoder2api-hub 账号文件兼容' },
    { p: 'qoder.state_file', l: 'Qoder 状态文件', t: 'text', h: '冷却/熔断状态落盘路径' },
    { p: 'qoder.weight', l: '混合调度权重', t: 'float', h: '同名模型选中 Qoder 的相对权重（WorkBuddy 固定 1.0）' },
    { p: 'qoder.cosy_version', l: 'COSY 版本', t: 'text', h: '留空 = 内置默认 1.1.75' },
    { p: 'qoder.timeout_seconds', l: '请求超时 (秒)', t: 'int', h: '' },
    { p: 'qoder.guard.enabled', l: '单号并发闸门', t: 'bool', h: '限制同一 Qoder 账号的在途请求数并做节奏控制（防风控）' },
    { p: 'qoder.guard.max_concurrency', l: '每号最大并发', t: 'int', h: '默认 1' },
    { p: 'qoder.guard.min_gap_ms', l: '请求最小间隔 (ms)', t: 'int', h: '默认 400ms' },
    { p: 'qoder.guard.max_wait_ms', l: '排队最长等待 (ms)', t: 'int', h: '超过后放行（fail-open），避免拖死请求' },
    { p: 'qoder.guard.jitter_ms', l: '间隔抖动 (ms)', t: 'int', h: '默认 120ms' },
    { p: 'qoder.web_tools', l: '本地 Web 工具', t: 'bool', h: '为 qoder: 强路由代跑 web_search / web_fetch（SSRF 防护）' },
    { p: 'qoder.image_transcode', l: '内联 PNG 压缩', t: 'bool', h: '仅对能省 ≥20% 的大 PNG 生效，不联网' },
    { p: 'qoder.pricing_file', l: '价格覆盖文件', t: 'text', h: '可选 JSON：{"model":{"in":..,"cached":..,"out":..}}（USD/1M）' },
    { p: 'qoder.anthropic_thinking', l: 'Anthropic thinking 档位', t: 'text', h: 'off = 不启用（默认）；留空也按 off 处理' },
    { p: 'qoder.priority', l: '调度档位', t: 'int', h: '数值大者先试；与 WorkBuddy 同为 1 = 平级加权' },
  ]},
  { sec: 'Loomy 第三上游', desc: '可选；讯飞桌面 AI 助手，签到 + 新手任务 + 模型网关', fields: [
    { p: 'loomy.enabled', l: '启用 Loomy', t: 'bool', h: '关闭后 Loomy 账号与 loomy: 前缀均不可用' },
    { p: 'loomy.auth_dir', l: 'Loomy 账号目录', t: 'text', h: '默认 ./loomy_auths；用 cmd/loomylogin 生成 loomy-*.json' },
    { p: 'loomy.state_file', l: 'Loomy 状态文件', t: 'text', h: '冷却/熔断状态落盘路径' },
    { p: 'loomy.priority', l: '调度档位', t: 'int', h: '数值大者先试；默认 2 = 在裸名合并调度中最优先（用完自动回落其它上游）' },
    { p: 'loomy.weight', l: '同档位权重', t: 'float', h: '与其它上游同档位时的相对权重' },
    { p: 'loomy.version', l: 'loomy-version', t: 'text', h: '上游请求头版本，留空 = 内置默认 0.9.38' },
    { p: 'loomy.timeout_seconds', l: '请求超时 (秒)', t: 'int', h: '' },
    { p: 'loomy.checkin_hours', l: '签到时点', t: 'hours', h: '每天在这些整点各签一次（上游按天幂等）' },
    { p: 'loomy.checkin_enabled', l: '启用签到', t: 'bool', h: '关闭后不再自动签到' },
    { p: 'loomy.onboarding_enabled', l: '领取新手任务', t: 'bool', h: '签到时领取 8 项新手任务（共 10000 分），默认开' },
    { p: 'loomy.keepalive_hours', l: '会话续期时点', t: 'hours', h: 'Loomy 无 refresh token，续期 = 重新登录' },
    { p: 'loomy.keepalive_enabled', l: '启用会话续期', t: 'bool', h: '' },
    { p: 'loomy.session_renew_before_days', l: '提前续期天数', t: 'float', h: '会话剩余不足该天数时重登，默认 3' },
    { p: 'loomy.request_gap_seconds', l: '账号间隔 (秒)', t: 'float', h: '批量签到/查询时账号之间的间隔（防风控），默认 1.0' },
  ]},
  { sec: '管理面板', desc: '本页面自身', fields: [
    { p: 'panel.enabled', l: '启用管理面板', t: 'bool', h: '关闭后 /panel/ 不再可用（需重启）' },
    { p: 'panel.dir', l: '前端资源目录', t: 'text', h: '留空 = 用二进制内嵌资源；填目录则从磁盘读（便于改样式）' },
  ]},
];

async function viewConfig() {
  const r = await api('/config');
  S.config = r.config;
  const cfg = r.config;
  const secs = SCHEMA.map(sec => {
    const fields = sec.fields.map(f => fieldHTML(f, getPath(cfg, f.p))).join('');
    return `<details class="form-section" ${['基础设置', '账号池调度'].includes(sec.sec) ? 'open' : ''}>
      <summary>${esc(sec.sec)}</summary>
      <div class="sec-desc">${esc(sec.desc)}</div>
      <div class="form-grid">${fields}</div>
    </details>`;
  }).join('');

  $('#view').innerHTML = `
    <div class="alert warn">
      保存会直接改写 <code>config.json</code>（深合并，保留未在此列出的字段）。
      <b>网关的配置在启动时一次性装配，因此几乎所有改动都需要重启才能生效</b>——
      保存后点击「保存并重启」即可，容器会在约 2 秒内自动拉起。
    </div>
    <div class="card">
      <div class="card-head"><h3>config.json</h3>
        <span class="sub mono">${esc(r.path)}</span></div>
      ${secs}
      <div class="savebar">
        <span class="grow" id="cfgMsg">修改后点击右侧保存</span>
        <button class="btn" id="cfgReset">放弃修改</button>
        <button class="btn primary" id="cfgSave">保存</button>
        <button class="btn warn" id="cfgSaveRestart">保存并重启</button>
      </div>
    </div>`;

  bindHours();
  $('#cfgReset').onclick = () => render();
  $('#cfgSave').onclick = () => saveConfig(false);
  $('#cfgSaveRestart').onclick = () => saveConfig(true);
}

function fieldHTML(f, val) {
  const id = `f_${f.p.replace(/\./g, '_')}`;
  if (f.t === 'bool') {
    return `<div class="field row"><label class="switch">
        <input type="checkbox" id="${id}" data-p="${f.p}" data-t="bool" ${val ? 'checked' : ''}>
        <span></span></label>
      <label for="${id}">${esc(f.l)}</label>
      ${f.h ? `<div class="hint" style="flex-basis:100%">${esc(f.h)}</div>` : ''}</div>`;
  }
  if (f.t === 'hours') {
    const cur = Array.isArray(val) ? val : [];
    return `<div class="field" style="grid-column:1/-1">
      <label>${esc(f.l)}</label>
      <div class="hours" data-hours="${f.p}">
        ${HOURS.map(h => `<label><input type="checkbox" value="${h}" ${cur.includes(h) ? 'checked' : ''}>${h}</label>`).join('')}
      </div>${f.h ? `<div class="hint">${esc(f.h)}</div>` : ''}</div>`;
  }
  if (f.t === 'select') {
    return `<div class="field"><label>${esc(f.l)}</label>
      <select id="${id}" data-p="${f.p}" data-t="select">
        ${f.opts.map(([v, t]) => `<option value="${esc(v)}" ${val === v ? 'selected' : ''}>${esc(t)}</option>`).join('')}
      </select>${f.h ? `<div class="hint">${esc(f.h)}</div>` : ''}</div>`;
  }
  const itype = f.t === 'int' || f.t === 'float' ? 'number' : 'text';
  const step = f.t === 'float' ? ' step="0.1"' : '';
  return `<div class="field"><label>${esc(f.l)}</label>
    <input type="${itype}"${step} id="${id}" data-p="${f.p}" data-t="${f.t}"
      class="${f.t === 'text' ? '' : 'mono'}" value="${esc(val ?? '')}"
      ${f.t === 'secret' ? 'autocomplete="off"' : ''}>
    ${f.h ? `<div class="hint">${esc(f.h)}</div>` : ''}</div>`;
}

function bindHours() {
  $$('[data-hours] input').forEach(cb => cb.onchange = () => {
    const box = cb.closest('[data-hours]');
    box.dataset.dirty = '1';
  });
}

function collectConfig() {
  const out = {};
  $$('[data-p]').forEach(el => {
    const p = el.dataset.p, t = el.dataset.t;
    let v;
    if (t === 'bool') v = el.checked;
    else if (t === 'int') v = parseInt(el.value, 10);
    else if (t === 'float') v = parseFloat(el.value);
    else v = el.value;
    if ((t === 'int' && Number.isNaN(v)) || (t === 'float' && Number.isNaN(v))) return;
    setPath(out, p, v);
  });
  $$('[data-hours]').forEach(box => {
    const p = box.dataset.hours;
    const hs = $$('input:checked', box).map(i => parseInt(i.value, 10)).sort((a, b) => a - b);
    setPath(out, p, hs);
  });
  return out;
}

async function saveConfig(restart) {
  const btn = restart ? $('#cfgSaveRestart') : $('#cfgSave');
  const old = btn.textContent; btn.disabled = true; btn.textContent = '保存中…';
  try {
    const r = await api('/config', { method: 'PUT', body: { config: collectConfig() } });
    toast(r.msg || '配置已保存');
    if (restart) await doRestart();
    else $('#cfgMsg').textContent = '已保存，需重启网关后生效';
  } catch (e) {
    toast(e.message, 'err', 7000);
  } finally { btn.disabled = false; btn.textContent = old; }
}

/* ── 模型页 ─────────────────────────────────────────── */
async function viewModels() {
  const r = await api('/models');
  const list = r.data || [];
  if (!list.length) { $('#view').innerHTML = `<div class="card"><div class="empty">暂无模型</div></div>`; return; }
  const rows = list.map(m => {
    const src = m.provider || m.owned_by || 'workbuddy';
    const badge = src === 'loomy' ? '<span class="tag info">Loomy</span>'
      : src === 'qoder' ? '<span class="tag ok">Qoder</span>'
      : src === 'merged' ? '<span class="tag warn">混合</span>'
      : '<span class="tag purple">WorkBuddy</span>';
    return `<tr>
      <td class="mono">${esc(m.id)}</td>
      <td>${badge}${m.qoder_key || m.loomy_key ? `<div class="mono" style="font-size:11px;color:var(--text-mute)">${esc(m.qoder_key || m.loomy_key)}</div>` : ''}</td>
      <td class="num">${m.context_length ? (m.context_length / 1024).toFixed(0) + 'K' : '—'}</td>
      <td>${(m.reasoning_supported_efforts || []).map(e => `<span class="tag gray">${esc(e)}</span>`).join(' ') || '—'}</td>
      <td>${m.reasoning_default_effort ? `<span class="tag info">${esc(m.reasoning_default_effort)}</span>` : '—'}</td>
    </tr>`;
  }).join('');
  $('#view').innerHTML = `<div class="card">
    <div class="card-head"><h3>可用模型 <span class="sub" style="font-weight:400">（${list.length} 个）</span></h3>
      <span class="sub">裸名 = 两上游混合调度；cn: / global: / qoder: 强制指定</span></div>
    <div class="table-wrap"><table class="tbl">
      <thead><tr><th>模型 ID</th><th>来源</th><th>上下文</th><th>推理档位</th><th>默认档位</th></tr></thead>
      <tbody>${rows}</tbody></table></div></div>`;
}

/* ── 用量统计页 ─────────────────────────────────────── */
// 数据源：上游「积分消耗明细」（get-user-request-usage，与官网同源）。
// 免费请求（credit=0）也逐条记录，故本页同时反映调用量与扣费。
// 拉取要对每个账号各打一轮上游请求，故后端带 TTL 缓存；「强制刷新」绕过缓存。
let usageData = null;

async function viewUsage(force = false) {
  $('#view').innerHTML = `<div class="loading">${force ? '正在重新拉取上游明细…' : '加载中…'}</div>`;
  let r;
  try {
    // 同时取账号池：上游明细只返回「有调用记录」的账号，窗口内零调用的号不会出现。
    // 需求是「每个账号」，故用账号池补齐零记录行（消耗 0、调用 0）。
    const [ur, ar] = await Promise.all([api('/usage' + (force ? '?refresh=1' : '')), api('/accounts')]);
    r = ur;
    r.__accounts = ar.accounts || [];
  } catch (e) {
    $('#view').innerHTML = `<div class="alert err">加载失败：${esc(e.message)}</div>`;
    return;
  }
  usageData = r.data || {};
  const d = usageData;

  // 用账号池补齐零记录账号，保证「每个账号」都有一行。
  const seen = new Set((d.accounts || []).map(a => a.uid));
  const merged = [...(d.accounts || [])];
  for (const a of (r.__accounts || [])) {
    if (!seen.has(a.uid)) {
      merged.push({ uid: a.uid, nickname: a.nickname, realm: a.realm,
                    calls: 0, credit: 0, calls_today: 0, credit_today: 0, free_calls: 0 });
    }
  }
  d.accounts = merged.sort((a, b) =>
    (b.credit - a.credit) || (b.calls - a.calls));

  const ageS = Math.round((r.cache_age_ms || 0) / 1000);
  const ageTxt = r.cached
    ? `缓存于 ${ageS < 60 ? ageS + ' 秒' : Math.floor(ageS / 60) + ' 分钟'}前`
    : '刚刚拉取';
  const ttlMin = Math.round((r.ttl_ms || 0) / 60000);

  const freeCalls = (d.accounts || []).reduce((s, a) => s + (a.free_calls || 0), 0);
  const freePct = d.total_calls ? Math.round((freeCalls / d.total_calls) * 100) : 0;

  const card = (label, value, foot, cls = '') =>
    `<div class="card stat ${cls}"><div class="stat-label">${label}</div>
     <div class="stat-value">${value}</div><div class="stat-foot">${foot}</div></div>`;

  const errBox = (d.errors || []).length ? `
    <div class="alert warn" style="margin-bottom:16px">
      <b>${d.errors.length} 个账号的明细拉取失败</b>（其余账号数据正常）：
      <div style="margin-top:6px;font-size:12px">
        ${d.errors.map(e => `${esc(e.nickname || e.uid.slice(0, 8))}（${esc(e.realm)}）: ${esc(e.error)}`).join('<br>')}
      </div>
    </div>` : '';

  $('#view').innerHTML = `
    <div class="alert info">
      数据源为上游「积分消耗明细」，含 <b>credit=0 的免费请求</b>，故同时反映调用量与扣费。
      ${esc(ageTxt)}（缓存 ${ttlMin} 分钟，点「强制刷新」立即重拉）。
    </div>
    ${errBox}

    <div class="grid cols-4">
      ${card('总调用', d.total_calls || 0, `近 ${d.windows?.days || 7} 天`, '')}
      ${card('总消耗', (d.total_credit || 0).toFixed(2), '积分（credit）', (d.total_credit > 0) ? 'warn' : 'ok')}
      ${card('免费调用', freeCalls, `占 ${freePct}%（credit=0）`, 'ok')}
      ${card('有消耗账号', (d.accounts || []).filter(a => a.credit > 0).length,
             `共 ${(d.accounts || []).length} 个账号`, '')}
    </div>

    <div class="row-flex" style="margin:16px 0;gap:8px">
      <button class="btn sm" id="usageForce">强制刷新</button>
      <span class="muted" style="font-size:12px;align-self:center">
        时间口径 UTC+8；免费模型（如 deepseek-v4.1-flash）调用量再大，消耗也是 0。
      </span>
    </div>

    <div class="grid cols-2">
      ${chartCard('近 24 小时调用量', d.hourly, 'calls')}
      ${chartCard('近 7 天调用量', d.daily, 'calls')}
    </div>

    <div style="height:16px"></div>
    <div class="grid cols-2">
      ${chartCard('近 24 小时消耗', d.hourly, 'credit')}
      ${chartCard('近 7 天消耗', d.daily, 'credit')}
    </div>

    <div style="height:16px"></div>
    ${modelTable(d.models || [])}

    <div style="height:16px"></div>
    ${usageAccountTable(d.accounts || [], d.detail || {})}
  `;

  $('#usageForce').onclick = () => viewUsage(true);
  bindUsageRows();
}

// chartCard 用纯 CSS 柱状图渲染一组时间桶（无需引入图表库）。
function chartCard(title, buckets, metric) {
  const list = buckets || [];
  const vals = list.map(b => metric === 'credit' ? (b.credit || 0) : (b.calls || 0));
  const max = Math.max(1, ...vals);
  const sum = vals.reduce((a, b) => a + b, 0);
  const unit = metric === 'credit' ? '积分' : '次';

  const bars = list.map((b, i) => {
    const v = vals[i];
    const h = Math.round((v / max) * 100);
    const fmt = metric === 'credit' ? v.toFixed(2) : v;
    const cls = v > 0 ? (metric === 'credit' ? 'warn' : 'ok') : '';
    return `<div class="ubar" title="${esc(b.label)}：${fmt} ${unit}">
        <div class="ubar-fill ${cls}" style="height:${v > 0 ? Math.max(2, h) : 0}%"></div>
        <div class="ubar-tick">${esc((b.label || '').split(' ').pop())}</div>
      </div>`;
  }).join('');

  // 24 根柱逐根标会重叠，加 wide 让 CSS 只显示每 3 小时一档刻度。
  const wide = list.length > 12 ? ' wide' : '';

  return `<div class="card">
    <div class="card-head"><h3>${esc(title)}</h3>
      <span class="sub">合计 ${metric === 'credit' ? sum.toFixed(2) : sum} ${unit}</span></div>
    <div class="card-body"><div class="ubars${wide}">${bars}</div></div>
  </div>`;
}

// modelTable 按模型汇总（跨全部账号）。
function modelTable(rows) {
  if (!rows.length) {
    return `<div class="card"><div class="card-head"><h3>按模型</h3></div>
      <div class="empty">窗口内没有调用记录。</div></div>`;
  }
  const max = Math.max(1, ...rows.map(r => r.credit || 0));
  return `<div class="card">
    <div class="card-head"><h3>按模型 <span class="sub" style="font-weight:400">（${rows.length} 个）</span></h3>
      <span class="sub">消耗降序</span></div>
    <div class="table-wrap"><table class="tbl">
      <thead><tr><th>模型</th><th class="num">调用</th><th class="num">消耗</th>
        <th class="num">单次均价</th><th>占比</th></tr></thead>
      <tbody>${rows.map(r => {
        const avg = r.calls ? (r.credit / r.calls) : 0;
        const pct = Math.round(((r.credit || 0) / max) * 100);
        return `<tr>
          <td class="mono">${esc(r.model)}</td>
          <td class="num">${r.calls}</td>
          <td class="num">${(r.credit || 0).toFixed(2)}</td>
          <td class="num">${avg ? avg.toFixed(3) : '0'}${r.credit === 0 ? ' <span class="tag ok">免费</span>' : ''}</td>
          <td class="num"><div class="bar ${r.credit > 0 ? 'warn' : ''}"><i style="width:${pct}%"></i></div></td>
        </tr>`;
      }).join('')}</tbody>
    </table></div></div>`;
}

// usageAccountTable 按账号汇总，行可点击展开该账号的小时/天/模型细分。
function usageAccountTable(rows, detail) {
  if (!rows.length) {
    return `<div class="card"><div class="card-head"><h3>按账号</h3></div>
      <div class="empty">窗口内没有调用记录。</div></div>`;
  }
  const max = Math.max(1, ...rows.map(r => r.credit || 0));
  const body = rows.map(a => {
    const realm = a.realm === 'global'
      ? '<span class="tag info">国际版</span>' : '<span class="tag purple">国内版</span>';
    const pct = Math.round(((a.credit || 0) / max) * 100);
    return `<tr class="usage-row" data-uid="${esc(a.uid)}" style="cursor:pointer">
        <td>${realm}</td>
        <td><div style="font-weight:500">${esc(a.nickname || '（无昵称）')}</div>
            <div class="mono" style="font-size:11px;color:var(--text-mute)">${esc((a.uid || '').slice(0, 18))}…</div></td>
        <td class="num">${a.calls}</td>
        <td class="num">${(a.credit || 0).toFixed(2)}
          <div class="bar ${a.credit > 0 ? 'warn' : 'ok'}" style="margin-top:5px"><i style="width:${pct}%"></i></div></td>
        <td class="num">${(a.credit_today || 0).toFixed(2)} <span class="muted" style="font-size:11px">/ ${a.calls_today || 0} 次</span></td>
        <td class="num">${a.free_calls || 0}</td>
        <td class="num"><button class="btn xs ghost" data-toggle="${esc(a.uid)}">展开 ▾</button></td>
      </tr>
      <tr class="usage-detail" id="ud_${esc(a.uid)}" hidden><td colspan="7">
        ${accountDetailHTML(detail[a.uid])}
      </td></tr>`;
  }).join('');

  return `<div class="card">
    <div class="card-head"><h3>按账号 <span class="sub" style="font-weight:400">（${rows.length} 个）</span></h3>
      <span class="sub">点击行展开该账号细分</span></div>
    <div class="table-wrap"><table class="tbl">
      <thead><tr><th>域</th><th>账号</th><th class="num">调用</th><th class="num">消耗</th>
        <th class="num">今日消耗/次数</th><th class="num">免费调用</th><th></th></tr></thead>
      <tbody>${body}</tbody>
    </table></div></div>`;
}

// accountDetailHTML 单账号下钻：模型分布 + 小时/天折线（用迷你柱表示）。
function accountDetailHTML(det) {
  if (!det) return '<div class="muted" style="padding:8px">无细分数据</div>';
  const models = (det.models || []).filter(m => m.calls > 0);
  const hourly = det.hourly || [];
  const daily = det.daily || [];

  const mini = (list, metric) => {
    const vals = list.map(b => metric === 'credit' ? (b.credit || 0) : (b.calls || 0));
    const max = Math.max(1, ...vals);
    return list.map((b, i) => {
      const v = vals[i];
      const h = Math.round((v / max) * 100);
      const fmt = metric === 'credit' ? v.toFixed(2) : v;
      return `<div class="ubar sm" title="${esc(b.label)}：${fmt}">
        <div class="ubar-fill" style="height:${v > 0 ? Math.max(2, h) : 0}%"></div>
        <div class="ubar-tick">${esc((b.label || '').split(' ').pop())}</div></div>`;
    }).join('');
  };

  return `<div class="usage-detail-body">
    <div class="usage-detail-col">
      <h4>模型分布</h4>
      ${models.length ? `<table class="tbl mini-tbl">
        <thead><tr><th>模型</th><th class="num">调用</th><th class="num">消耗</th></tr></thead>
        <tbody>${models.map(m => `<tr><td class="mono">${esc(m.model)}</td>
          <td class="num">${m.calls}</td><td class="num">${(m.credit || 0).toFixed(2)}</td></tr>`).join('')}</tbody>
      </table>` : '<div class="muted">无记录</div>'}
    </div>
    <div class="usage-detail-col">
      <h4>近 24 小时调用</h4><div class="ubars sm">${mini(hourly, 'calls')}</div>
      <h4 style="margin-top:12px">近 7 天调用</h4><div class="ubars sm">${mini(daily, 'calls')}</div>
    </div>
  </div>`;
}

function bindUsageRows() {
  $$('[data-toggle]').forEach(b => b.onclick = e => {
    e.stopPropagation();
    const uid = b.dataset.toggle;
    const row = $(`#ud_${CSS.escape(uid)}`);
    if (!row) return;
    row.hidden = !row.hidden;
    b.textContent = row.hidden ? '展开 ▾' : '收起 ▴';
  });
  $$('.usage-row').forEach(tr => tr.onclick = () => {
    const btn = $(`[data-toggle="${CSS.escape(tr.dataset.uid)}"]`);
    if (btn) btn.click();
  });
}

/* ── 日志页 ─────────────────────────────────────────── */
let logPaused = false;
async function viewLogs() {
  $('#view').innerHTML = `
    <div class="card">
      <div class="card-head">
        <h3>运行日志 <span class="sub" style="font-weight:400" id="logCount"></span></h3>
        <div class="row-flex">
          <label class="auto-refresh"><input type="checkbox" id="logPause"><span>暂停</span></label>
          <button class="btn sm" id="logClear">仅看开启后</button>
        </div>
      </div>
      <div class="card-body"><div class="logbox" id="logbox"><div class="muted">加载中…</div></div></div>
    </div>`;
  $('#logPause').onchange = e => { logPaused = e.target.checked; };
  let since = 0;
  $('#logClear').onclick = () => { since = Date.now(); toast('已过滤更早的日志'); };
  await refreshLogs(() => since);
}

async function refreshLogs(getSince) {
  if (logPaused || S.route !== 'logs') return;
  try {
    const r = await api('/logs?limit=400');
    const since = getSince();
    const lines = (r.lines || []).filter(l => new Date(l.t).getTime() >= since);
    const box = $('#logbox');
    if (!box) return;
    $('#logCount').textContent = `（${lines.length} 行）`;
    box.innerHTML = lines.map(l => {
      const t = new Date(l.t).toLocaleTimeString('zh-CN', { hour12: false });
      return `<div class="logline ${esc(l.level)}"><span class="lt">${t}</span>
        <span class="lv">${esc(l.level)}</span><span class="lm">${esc(l.text)}</span></div>`;
    }).join('') || '<div class="muted">暂无日志</div>';
    box.scrollTop = box.scrollHeight;
  } catch { /* 静默：日志刷新失败不打扰 */ }
}

/* ── 重启 ───────────────────────────────────────────── */
async function doRestart() {
  modal('重启网关', `<div class="alert warn">网关进程将退出并由容器自动拉起，约 2 秒后恢复。
    在途的流式请求会被中断。</div><div id="rsMsg" class="muted">重启中…</div>`);
  try { await api('/restart', { method: 'POST' }); } catch { /* 进程退出会导致连接中断，属正常 */ }
  const t0 = Date.now();
  const tick = async () => {
    try {
      const ov = await api('/overview');
      $('#rsMsg').innerHTML = `<span style="color:var(--ok)">已恢复（耗时 ${((Date.now() - t0) / 1000).toFixed(1)}s）</span>`;
      setTimeout(() => { closeModal(); render(); }, 900);
      return;
    } catch { /* 还没起来 */ }
    if (Date.now() - t0 > 45000) { $('#rsMsg').innerHTML = '<span style="color:var(--err)">等待超时，请手动检查容器状态</span>'; return; }
    setTimeout(tick, 700);
  };
  setTimeout(tick, 800);
}

/* ── 主题 ───────────────────────────────────────────── */
function initTheme() {
  const saved = localStorage.getItem('wb2a-theme') || 'dark';
  document.documentElement.dataset.theme = saved;
  $('#themeBtn').onclick = () => {
    const cur = document.documentElement.dataset.theme === 'light' ? 'dark' : 'light';
    document.documentElement.dataset.theme = cur;
    localStorage.setItem('wb2a-theme', cur);
  };
}

/* ── 自动刷新 ───────────────────────────────────────── */
function initAutoRefresh() {
  const cb = $('#autoRefresh');
  const info = $('#tickInfo');
  const start = () => {
    clearInterval(S.timer);
    S.timer = setInterval(async () => {
      if (S.route === 'logs') { await refreshLogs(() => 0); return; }
      if (S.route === 'config') return; // 配置页不自动刷，避免覆盖用户输入
      try {
        const ov = await api('/overview');
        setHealth(ov.healthy, ov.counts);
        info.textContent = new Date().toLocaleTimeString('zh-CN', { hour12: false });
        if (S.route === 'overview' || S.route === 'accounts') await render();
      } catch {
        setHealth(false, {});
        info.textContent = '连接失败';
      }
    }, 5000);
  };
  cb.onchange = () => { cb.checked ? start() : (clearInterval(S.timer), info.textContent = ''); };
  if (cb.checked) start();
}

/* ── 智能体配置 ─────────────────────────────────────── */
async function viewAgents() {
  const data = await api('/agents');
  const list = data.clients || [];
  S.agentClients = list;
  const auto = list.filter(c => c.mode === 'automatic');
  const manual = list.filter(c => c.mode !== 'automatic');
  const baseURL = S.agentBaseURL || location.origin;

  const row = c => `
    <tr>
      <td><strong>${esc(c.label)}</strong><div class="muted" style="font-size:11px">${esc(c.desc)}</div></td>
      <td>${c.installed ? '<span class="tag ok">已安装</span>' : '<span class="tag gray">未检测到</span>'}
          ${c.configured ? '<span class="tag warn">已接入</span>' : ''}
          <div class="muted" style="font-size:11px">${esc((c.evidence || []).join(' · ') || '—')}</div></td>
      <td class="mono" style="font-size:11px">${esc((c.config_paths || []).join('\n') || '—')}</td>
      <td style="white-space:nowrap">
        ${c.mode === 'automatic'
          ? `<button class="btn sm" data-plan="${esc(c.id)}">预览</button>
             <button class="btn sm" data-apply="${esc(c.id)}">接入</button>
             ${c.configured ? `<button class="btn sm warn" data-restore="${esc(c.id)}">还原</button>` : ''}`
          : `<button class="btn sm" data-guide="${esc(c.id)}">接入指引</button>`}
      </td>
    </tr>`;

  $('#view').innerHTML = `
    <div class="card">
      <div class="row-flex" style="gap:10px;align-items:end">
        <div class="field" style="flex:2;min-width:240px"><label>网关地址</label>
          <input id="agBase" value="${esc(baseURL)}" placeholder="http://127.0.0.1:8787"></div>
        <div class="field" style="flex:2;min-width:200px"><label>API Key</label>
          <input id="agKey" type="password" placeholder="面板/网关的 API Key"></div>
        <div class="field" style="flex:1;min-width:160px"><label>默认模型</label>
          <input id="agModel" value="${esc(S.agentModel || '')}" placeholder="如 qoder:DeepSeek-V4-Pro"></div>
      </div>
      <p class="muted" style="margin:8px 0 0">Key 只写入客户端本地配置；网关 state 仅保存 sha256。写入前会备份原件，还原回到首次接入前。</p>
    </div>
    <div class="card"><h3>可一键接入</h3>
      <table class="tbl"><thead><tr><th>客户端</th><th>状态</th><th>配置文件</th><th>操作</th></tr></thead>
      <tbody>${auto.map(row).join('')}</tbody></table></div>
    <div class="card"><h3>仅检测 / 手动接入</h3>
      <table class="tbl"><thead><tr><th>客户端</th><th>状态</th><th>配置</th><th>操作</th></tr></thead>
      <tbody>${manual.map(row).join('')}</tbody></table></div>`;

  const ctx = () => {
    const body = { base_url: $('#agBase').value.trim(), api_key: $('#agKey').value, model: $('#agModel').value.trim() };
    S.agentBaseURL = body.base_url; S.agentModel = body.model;
    return body;
  };
  const needKey = () => {
    if (!$('#agKey').value.trim()) { toast('请先填 API Key', 'err'); return false; }
    return true;
  };

  $$('[data-plan]').forEach(b => b.onclick = async () => {
    if (!needKey()) return;
    b.disabled = true;
    try {
      const r = await api('/agents/plan', { method: 'POST', body: { client: b.dataset.plan, ...ctx() } });
      showAgentPlan(r.plan, '预览（尚未写入）');
    } catch (e) { toast(e.message, 'err'); } finally { b.disabled = false; }
  });
  $$('[data-apply]').forEach(b => b.onclick = async () => {
    if (!needKey()) return;
    b.disabled = true;
    try {
      const r = await api('/agents/apply', { method: 'POST', body: { client: b.dataset.apply, ...ctx() } });
      const files = (r.result.files || []).map(f => f.path).join('\n');
      toast(`已接入 ${r.result.label}`);
      modal('接入完成', `<p>已写入并备份：</p><pre class="code">${esc(files)}</pre>`);
      render();
    } catch (e) { toast(e.message, 'err'); } finally { b.disabled = false; }
  });
  $$('[data-restore]').forEach(b => b.onclick = async () => {
    if (!confirm('还原到首次接入前的状态？')) return;
    try {
      const r = await api('/agents/restore', { method: 'POST', body: { client: b.dataset.restore } });
      toast(`已还原 ${r.result.label}`);
      render();
    } catch (e) { toast(e.message, 'err'); }
  });
  $$('[data-guide]').forEach(b => b.onclick = () => {
    const c = S.agentClients.find(x => x.id === b.dataset.guide);
    const guide = (c.guide || '').replaceAll('{BASE_URL_V1}', $('#agBase').value.trim());
    modal(c.label, `<p>${esc(c.desc)}</p><pre class="code" style="white-space:pre-wrap">${esc(guide)}</pre>
      ${c.docs_url ? `<p><a href="${esc(c.docs_url)}" target="_blank" rel="noopener">官方文档</a></p>` : ''}`);
  });
}

function showAgentPlan(plan, title) {
  const files = plan.files || [];
  const body = files.length
    ? files.map(f => `<div class="muted mono" style="margin-top:8px">${esc(f.path)}${f.existed ? '（已存在，将备份）' : '（新建）'}</div>
        <pre class="code" style="white-space:pre-wrap">${esc(f.content)}</pre>`).join('')
    : `<p>${esc(plan.guide || '请参考官方配置指引。')}</p>`;
  modal(`${title} · ${plan.label}`, body,
    files.length ? `<button class="btn" data-close>关闭</button><button class="btn primary" id="agGo">确认写入</button>` : '');
  const go = $('#agGo');
  if (go) go.onclick = async () => {
    go.disabled = true;
    try {
      await api('/agents/apply', { method: 'POST', body: {
        client: plan.client, base_url: $('#agBase').value.trim(),
        api_key: $('#agKey').value, model: $('#agModel').value.trim(),
      } });
      closeModal(); toast('已接入'); render();
    } catch (e) { toast(e.message, 'err'); go.disabled = false; }
  };
}

/* ── 启动 ───────────────────────────────────────────── */
$('#nav').addEventListener('click', e => {
  const item = e.target.closest('.nav-item');
  if (item) go(item.dataset.route);
});
$('#refreshBtn').onclick = () => render();
$('#restartBtn').onclick = () => {
  modal('重启网关', `<div class="alert warn">确认重启？在途的流式请求会被中断。</div>`,
    `<button class="btn" data-close>取消</button><button class="btn warn" id="okRs">确认重启</button>`);
  $('#okRs').onclick = () => { closeModal(); doRestart(); };
};

initTheme();
initAutoRefresh();
S.route = ROUTES[location.hash.replace('#', '')] ? location.hash.replace('#', '') : 'overview';
render();
