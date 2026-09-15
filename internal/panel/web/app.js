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

/* ── 全局状态 ───────────────────────────────────────── */
const S = { route: 'overview', overview: null, accounts: [], config: null, timer: null };

/* ── 路由 ───────────────────────────────────────────── */
const ROUTES = {
  overview: { title: '概览', sub: '网关健康状态与账号池总览', render: viewOverview },
  accounts: { title: '账号管理', sub: '账号池详情、添加与移除', render: viewAccounts },
  config: { title: '网关配置', sub: '在线编辑 config.json（保存后需重启生效）', render: viewConfig },
  models: { title: '模型列表', sub: '网关对外暴露的全部模型', render: viewModels },
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

  const card = (label, value, foot, cls = '') =>
    `<div class="card stat ${cls}"><div class="stat-label">${label}</div>
     <div class="stat-value">${value}</div><div class="stat-foot">${foot}</div></div>`;

  $('#view').innerHTML = `
    ${ov.counts.auth_files !== ov.counts.total ? `
      <div class="alert warn">
        <b>凭证文件数（${ov.counts.auth_files}）与池内账号数（${ov.counts.total}）不一致。</b>
        可能有凭证文件解析失败或未被加载。可点击右侧「重载账号」重新对齐。
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
        <div class="card-head"><h3>按域可用性</h3><span class="sub">模型名前缀决定路由</span></div>
        <div class="card-body">
          <table class="tbl">
            <thead><tr><th>域</th><th>前缀</th><th class="num">总数</th><th class="num">健康</th><th>状态</th></tr></thead>
            <tbody>
              <tr><td><span class="tag purple">国内版</span></td><td class="mono">cn:</td>
                  <td class="num">${n.total || 0}</td><td class="num">${n.healthy || 0}</td>
                  <td>${n.healthy > 0 ? '<span class="tag ok"><i class="tdot"></i>可服务</span>'
                                    : '<span class="tag gray">无可用账号</span>'}</td></tr>
              <tr><td><span class="tag info">国际版</span></td><td class="mono">global:</td>
                  <td class="num">${g.total || 0}</td><td class="num">${g.healthy || 0}</td>
                  <td>${g.healthy > 0 ? '<span class="tag ok"><i class="tdot"></i>可服务</span>'
                                    : '<span class="tag gray">无可用账号</span>'}</td></tr>
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
  bindAccountActions();
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
  if (!list.length) {
    return `<div class="card"><div class="card-head"><h3>账号池</h3></div>
      <div class="empty">还没有任何账号，点击右上角「添加账号」开始。</div></div>`;
  }
  const maxCredit = Math.max(1, ...list.map(a => a.credits || 0));
  const rows = list.map(a => {
    let st = '<span class="tag ok"><i class="tdot"></i>健康</span>';
    if (a.disabled) st = '<span class="tag err"><i class="tdot"></i>已禁用</span>';
    else if (a.cooling) st = `<span class="tag warn"><i class="tdot"></i>冷却 ${esc(fmtRemain(a.cool_remaining_sec))}</span>`;
    else if (a.in_flight > 0) st = `<span class="tag info"><i class="tdot"></i>在途 ${a.in_flight}</span>`;

    const realm = a.realm === 'global'
      ? '<span class="tag info">国际版</span>' : '<span class="tag purple">国内版</span>';
    const pct = Math.min(100, Math.round(((a.credits || 0) / maxCredit) * 100));

    return `<tr>
      <td>${st}</td>
      <td><div style="font-weight:500">${esc(a.nickname || '（无昵称）')}</div>
          <div class="mono" style="font-size:11px;color:var(--text-mute)">${esc((a.uid || '').slice(0, 18))}…</div></td>
      <td>${realm}</td>
      <td class="num">${a.credits ?? 0}<div class="bar ${a.credits > 0 ? 'ok' : ''}" style="margin-top:5px">
          <i style="width:${pct}%"></i></div></td>
      <td class="num">${a.success_count || 0} / ${a.err_total || 0}</td>
      <td class="num" style="font-size:11.5px">${esc(a.expires_at ? fmtTime(a.expires_at * 1000) : '—')}</td>
      <td class="num" style="font-size:11.5px">${esc(a.last_success || '—')}</td>
      <td style="white-space:nowrap">
        <button class="btn xs danger" data-del="${esc(a.uid)}" data-nick="${esc(a.nickname || a.uid)}">移除</button>
      </td>
    </tr>`;
  }).join('');

  return `<div class="card">
    <div class="card-head">
      <h3>账号池 <span class="sub" style="font-weight:400">（${list.length} 个）</span></h3>
      <div class="row-flex">
        <button class="btn sm" data-checkin
          title="立即执行签到 + 余额查询。定时任务默认只在 9/21 点跑，新账号在此之前积分会显示 0">查询余额 / 签到</button>
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

function bindAccountActions() {
  $$('[data-del]').forEach(b => b.onclick = () => confirmDelete(b.dataset.del, b.dataset.nick));
  $$('[data-add]').forEach(b => b.onclick = addAccountFlow);
  $$('[data-checkin]').forEach(b => b.onclick = runCheckin);
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

function confirmDelete(uid, nick) {
  modal('移除账号', `
    <div class="alert warn">将从 <code>auths/</code> 删除该账号的凭证文件，并立即从账号池移除。
      删除前会自动备份到 <code>backups/</code>。</div>
    <dl class="kv"><dt>昵称</dt><dd>${esc(nick)}</dd><dt>UID</dt><dd>${esc(uid)}</dd></dl>`,
    `<button class="btn" data-close>取消</button>
     <button class="btn danger" id="doDel">确认移除</button>`);
  $('#doDel').onclick = async () => {
    try {
      const r = await api('/accounts/delete', { method: 'POST', body: { uid } });
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
  bindAccountActions();
}

/* ── 添加账号（OAuth 三步）───────────────────────────── */
function addAccountFlow() {
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
  }
}

/* ── 配置页 ─────────────────────────────────────────── */
const HOURS = Array.from({ length: 24 }, (_, i) => i);

const SCHEMA = [
  { sec: '基础设置', desc: '网关监听与访问控制', fields: [
    { p: 'listen', l: '监听地址', t: 'text', h: '如 :7863；修改后需重启' },
    { p: 'api_key', l: 'API 密钥', t: 'secret', h: '客户端 Bearer Token；留空 = 不鉴权（公网务必设置）' },
    { p: 'server.max_body_mb', l: '请求体上限 (MB)', t: 'int', h: '超过直接返回 413，不再静默截断' },
  ]},
  { sec: '账号池调度', desc: '选号权重、熔断与并发', fields: [
    { p: 'pool.max_in_flight', l: '单号最大在途', t: 'int', h: '0 = 不限；占满的号不参与选号' },
    { p: 'pool.breaker_threshold', l: '熔断阈值（连续失败次数）', t: 'int', h: '' },
    { p: 'pool.breaker_cooldown', l: '熔断基础时长', t: 'text', h: '如 30m' },
    { p: 'pool.breaker_cooldown_max', l: '熔断退避封顶', t: 'text', h: '如 6h' },
    { p: 'pool.idle_weight_per_hour', l: '闲置补偿 / 小时', t: 'float', h: '每小时未用增加的权重' },
    { p: 'pool.idle_weight_max', l: '闲置补偿封顶', t: 'float', h: '' },
    { p: 'pool.expiring_soon', l: '快过期积分窗口', t: 'text', h: '如 168h = 7 天；此窗口内到期的积分优先消耗' },
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
  const rows = list.map(m => `<tr>
      <td class="mono">${esc(m.id)}</td>
      <td class="num">${m.context_length ? (m.context_length / 1024).toFixed(0) + 'K' : '—'}</td>
      <td>${(m.reasoning_supported_efforts || []).map(e => `<span class="tag gray">${esc(e)}</span>`).join(' ') || '—'}</td>
      <td>${m.reasoning_default_effort ? `<span class="tag info">${esc(m.reasoning_default_effort)}</span>` : '—'}</td>
    </tr>`).join('');
  $('#view').innerHTML = `<div class="card">
    <div class="card-head"><h3>可用模型 <span class="sub" style="font-weight:400">（${list.length} 个）</span></h3>
      <span class="sub">无前缀 = 国内版；global: = 国际版</span></div>
    <div class="table-wrap"><table class="tbl">
      <thead><tr><th>模型 ID</th><th>上下文</th><th>推理档位</th><th>默认档位</th></tr></thead>
      <tbody>${rows}</tbody></table></div></div>`;
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
