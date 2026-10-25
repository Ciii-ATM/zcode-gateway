'use strict';

const $ = (id) => document.getElementById(id);
const state = {
  view: 'overview',
  overview: null,
  accounts: [],
  logs: [],
  settings: null,
  addFlow: null,
  addTimer: null,
  logFilter: { status: '', account: '' },
  activityAccountId: null,
  activityCaptcha: null,
};

const numberFmt = new Intl.NumberFormat('zh-CN');
const compactFmt = new Intl.NumberFormat('zh-CN', { notation: 'compact', maximumFractionDigits: 1 });

function n(value) { return numberFmt.format(Number(value) || 0); }
function compact(value) { return compactFmt.format(Number(value) || 0); }
function msText(value) {
  const ms = Number(value) || 0;
  return ms > 0 ? `${ms}ms` : '—';
}
function speedText(value) {
  const speed = Number(value) || 0;
  return speed > 0 ? `${speed.toFixed(1)} tok/s` : '—';
}
function cooldownText(account) {
  if (account.status !== '冷却中' || !account.exhaustedUntil) return '';
  const seconds = Math.max(0, Number(account.exhaustedUntil) - Math.floor(Date.now() / 1000));
  if (seconds <= 0) return '';
  return seconds < 60 ? `剩 ${seconds} 秒` : `剩 ${Math.ceil(seconds / 60)} 分钟`;
}
function units(value) {
  const n = Number(value) || 0;
  if (n < 1000) return numberFmt.format(n);
  if (n < 1e6) return (n / 1e3).toFixed(n < 1e4 ? 2 : 1).replace(/\.0+$/, '') + 'K';
  if (n < 1e9) return (n / 1e6).toFixed(n < 1e7 ? 2 : 1).replace(/\.0+$/, '') + 'M';
  return (n / 1e9).toFixed(2).replace(/\.0+$/, '') + 'B';
}
function esc(value) {
  return String(value == null ? '' : value)
    .replaceAll('&', '&amp;').replaceAll('<', '&lt;').replaceAll('>', '&gt;')
    .replaceAll('"', '&quot;').replaceAll("'", '&#39;');
}
function timeText(ts) {
  if (!ts) return '-';
  return new Date(ts * 1000).toLocaleString('zh-CN', { hour12: false });
}
function relative(ts) {
  if (!ts) return '-';
  const diff = Math.max(0, Math.floor(Date.now() / 1000 - ts));
  if (diff < 60) return `${diff} 秒前`;
  if (diff < 3600) return `${Math.floor(diff / 60)} 分钟前`;
  if (diff < 86400) return `${Math.floor(diff / 3600)} 小时前`;
  return new Date(ts * 1000).toLocaleDateString('zh-CN');
}
function shortId(value) {
  const text = String(value || '');
  return text.length > 12 ? text.slice(0, 8) + '…' : text;
}
function rateClass(rate) {
  if (rate >= 98) return 'rate-good';
  if (rate >= 80) return 'rate-warn';
  return 'rate-bad';
}
function sumUsage(u) {
  if (!u) return 0;
  return (u.input_tokens || 0) + (u.output_tokens || 0) + (u.cache_read_input_tokens || 0)
    + (u.cache_creation_input_tokens || 0) + (u.reasoning_tokens || 0);
}

async function api(path, options = {}) {
  const response = await fetch(path, {
    headers: { 'Content-Type': 'application/json', ...(options.headers || {}) },
    ...options,
  });
  const text = await response.text();
  let body = null;
  try { body = text ? JSON.parse(text) : null; } catch { body = text; }
  if (response.status === 401) {
    window.location.href = '/';
    throw new Error('登录已过期');
  }
  if (!response.ok) {
    const message = body && body.error && (body.error.message || body.error.type)
      ? (body.error.message || body.error.type)
      : (body && body.msg) || `HTTP ${response.status}`;
    const error = new Error(message);
    error.status = response.status;
    error.body = body;
    throw error;
  }
  return body;
}

function toast(message, isError = false) {
  const el = $('toast');
  el.textContent = message;
  el.className = `toast show${isError ? ' error' : ''}`;
  clearTimeout(toast.timer);
  toast.timer = setTimeout(() => { el.className = 'toast'; }, 2800);
}

function setView(view) {
  state.view = view;
  document.querySelectorAll('.nav-item').forEach((item) => {
    item.classList.toggle('active', item.dataset.view === view);
  });
  document.querySelectorAll('.view').forEach((el) => el.classList.add('hidden'));
  const target = $(`view-${view}`);
  if (target) target.classList.remove('hidden');
  const titles = {
    overview: ['概览', '账号、额度与请求状态'],
    accounts: ['账号', '登录、排序与调度状态'],
    logs: ['日志', '每次请求的 token 用量与上游结果'],
    settings: ['设置', '本地网关行为与安全选项'],
  };
  $('page-title').textContent = titles[view][0];
  $('page-subtitle').textContent = titles[view][1];
  if (view === 'logs') loadLogs();
  if (view === 'settings') loadSettings();
}

async function loadOverview() {
  try {
    const data = await api('/api/overview');
    state.overview = data;
    renderOverview(data);
    setServerStatus(true);
  } catch (error) {
    setServerStatus(false);
    toast(error.message, true);
  }
}

function setServerStatus(ok) {
  $('server-dot').className = `dot ${ok ? 'ok' : 'bad'}`;
  $('server-status').textContent = ok ? '运行中' : '连接失败';
}

function renderOverview(data) {
  const accounts = data.accounts || {};
  $('stat-accounts').textContent = n(accounts.total);
  $('stat-available').textContent = `${n(accounts.available)} 个可用`;
  $('stat-requests').textContent = n(data.today?.requests);
  $('stat-tokens').textContent = units(data.today?.totalTokens);
  $('stat-token-breakdown').textContent = `输入 ${units(data.today?.inputTokens)} · 输出 ${units(data.today?.outputTokens)}`;
  $('stat-cache').textContent = units((data.today?.cacheReadTokens || 0) + (data.today?.cacheWriteTokens || 0));
  $('stat-cache-breakdown').textContent = `读取 ${units(data.today?.cacheReadTokens)} · 写入 ${units(data.today?.cacheWriteTokens)}`;
  $('sidebar-version').textContent = `v${data.server?.version || '-'}`;
  $('api-base').textContent = data.server?.baseUrl || 'http://127.0.0.1:8787';
  $('nav-account-count').textContent = n(accounts.total);

  const recent = data.recent || [];
  const tbody = $('recent-table').querySelector('tbody');
  tbody.innerHTML = recent.map(recentRow).join('');
  $('recent-empty').classList.toggle('hidden', recent.length > 0);
  $('recent-table').classList.toggle('hidden', recent.length === 0);
}

function recentRow(log) {
  return `<tr class="clickable" data-log="${esc(log.id)}">
    <td class="muted-cell">${timeText(log.time)}</td>
    <td><div>${esc(log.accountLabel || '-')}</div><div class="muted-cell account-id">${esc(shortId(log.accountId))}</div></td>
    <td class="mono ellipsis">${esc(log.model || '-')}</td>
    <td>${statusTag(log)}</td>
    <td class="num">${n(log.usage?.input_tokens)}</td>
    <td class="num">${n((log.usage?.cache_read_input_tokens || 0) + (log.usage?.cache_creation_input_tokens || 0))}</td>
    <td class="num">${n(log.usage?.output_tokens)}</td>
    <td class="num">${n(log.usage?.reasoning_tokens)}</td>
    <td class="num muted-cell">${log.durationMs || 0}ms</td>
    <td class="num muted-cell">${msText(log.firstTokenMs)}</td>
    <td class="num muted-cell">${msText(log.lastTokenMs)}</td>
    <td class="num">${speedText(log.tokensPerSec)}</td>
  </tr>`;
}

function statusTag(log) {
  if (log.ok) return '<span class="tag ok"><span class="dot"></span>成功</span>';
  return `<span class="tag bad"><span class="dot"></span>${esc(log.status ? `失败 ${log.status}` : '失败')}</span>`;
}

function statusClass(status) {
  if (status === '可用') return 'ok';
  if (status === '额度耗尽' || status === '异常') return 'bad';
  if (status === '已禁用') return 'warn';
  return 'info';
}

function modeLabel(mode) {
  if (mode === 'start-plan') return 'Start Plan';
  if (mode === 'coding-plan') return 'Coding Plan';
  return mode || '-';
}

function barClass(percent) {
  if (percent <= 10) return 'danger';
  if (percent <= 30) return 'warn';
  return '';
}

function quotaHtml(quota, detailed) {
  if (!quota || !quota.models || quota.models.length === 0) {
    return `<div class="empty small">${quota && quota.error ? esc(quota.error) : '等待额度刷新…'}</div>`;
  }
  return quota.models.map((model) => {
    const percent = Math.max(0, Math.min(100, model.percent || 0));
    return `<div class="quota-item">
      <div class="quota-top">
        <span class="quota-name">${esc(model.name)}</span>
        <span class="quota-num">${compact(model.used)} / ${compact(model.granted)} · 剩 ${compact(model.remaining)}</span>
      </div>
      <div class="bar ${barClass(percent)}"><span style="width:${percent.toFixed(1)}%"></span></div>
      ${detailed ? `<div class="quota-num">${esc(model.source === 'api' ? '上游实时额度' : '本地统计额度')} · ${percent.toFixed(1)}%</div>` : ''}
    </div>`;
  }).join('');
}

async function loadAccounts() {
  try {
    const data = await api('/api/accounts');
    state.accounts = data.items || [];
    renderAccounts();
    renderLogAccountOptions();
  } catch (error) {
    toast(error.message, true);
  }
}

function renderAccounts() {
  const tbody = $('accounts-table').querySelector('tbody');
  tbody.innerHTML = state.accounts.map(accountRow).join('');
  $('accounts-empty').classList.toggle('hidden', state.accounts.length > 0);
  $('accounts-table').classList.toggle('hidden', state.accounts.length === 0);
}

function accountRow(account, index) {
  const today = account.today || {};
  const totalUsage = account.usage || {};
  const totalTokens = sumUsage(totalUsage);
  const rate = Number(today.successRate || 0);
  const user = account.user || {};
  const quotaModels = account.quota && Array.isArray(account.quota.models) ? account.quota.models : [];
  const quotaPlans = account.quota && Array.isArray(account.quota.plans) ? account.quota.plans : [];
  const quotaLine = (model) => {
    const percent = Math.max(0, Math.min(100, Number(model.percent) || 0));
    return `<div class="quota-model">
      <div class="quota-model-top"><span>${esc(model.name)}</span><span>${units(model.used)} / ${units(model.granted)} · ${percent.toFixed(1)}%</span></div>
      <div class="bar ${barClass(percent)}"><span style="width:${percent.toFixed(1)}%"></span></div>
    </div>`;
  };
  const quotaHtmlLines = quotaPlans.length
    ? quotaPlans.map((plan) => `<div class="quota-plan">
        <div class="quota-plan-head"><span>${esc(plan.name || plan.planId)}</span><span class="muted-cell">P${n(plan.priority)}</span></div>
        ${(plan.models || []).map(quotaLine).join('')}
      </div>`).join('')
    : quotaModels.length
      ? quotaModels.map(quotaLine).join('')
      : '<span class="muted-cell">等待额度刷新</span>';
  const nowSec = Math.floor(Date.now() / 1000);
  const modelStateLines = (account.modelStates || [])
    .filter((state) => state.disabled || Number(state.cooldownUntil) > nowSec)
    .map((state) => {
      const remain = Math.max(0, Number(state.cooldownUntil) - nowSec);
      const label = state.disabled ? '已禁用' : `冷却 ${remain < 60 ? `${remain}秒` : `${Math.ceil(remain / 60)}分`}`;
      return `<div class="model-state ${state.disabled ? 'bad' : 'warn'}" title="${esc(state.lastError || '')}">${esc(state.model)} · ${label}</div>`;
    }).join('');
  const tokenCells = [
    ['输入', today.input],
    ['输出', today.output],
    ['缓存读', today.cacheRead],
    ['缓存写', today.cacheWrite],
    ['思考', today.reasoning],
  ].map(([label, value]) => `<div class="token-cell"><span>${label}</span><strong>${units(value)}</strong></div>`).join('');
  return `<tr data-account="${esc(account.id)}">
    <td><span class="tag info">#${account.priority}</span></td>
    <td>
      <div style="font-weight:600">${esc(account.label)}</div>
      <div class="muted-cell ellipsis">${esc(user.email || user.id || '')}</div>
      <div class="muted-cell account-id">${esc(shortId(account.id))}</div>
      <div class="muted-cell fingerprint" title="device_mid ${esc(account.deviceMid || '')}">${esc(fingerprintText(account))}</div>
    </td>
    <td><span class="tag">${esc(modeLabel(account.mode))}</span></td>
    <td>
      <span class="tag ${statusClass(account.status)}" title="${esc(account.lastError || '')}">${esc(account.status)}</span>
      ${cooldownText(account) ? `<div class="muted-cell">${esc(cooldownText(account))}</div>` : ''}
    </td>
    <td style="min-width:250px">
      <div class="quota-models">${quotaHtmlLines}</div>
      ${modelStateLines}
    </td>
    <td style="min-width:260px">
      <div class="today-total">${units(today.tokens)} <span class="muted-cell">tokens</span></div>
      <div class="token-grid">${tokenCells}</div>
    </td>
    <td>
      <div class="today-total" title="输入 ${units(totalUsage.input_tokens)} · 缓存读 ${units(totalUsage.cache_read_input_tokens)} · 缓存写 ${units(totalUsage.cache_creation_input_tokens)} · 输出 ${units(totalUsage.output_tokens)} · 思考 ${units(totalUsage.reasoning_tokens)}">${units(totalTokens)} <span class="muted-cell">tokens</span></div>
      <div class="muted-cell">输入 ${units(totalUsage.input_tokens)} · 输出 ${units(totalUsage.output_tokens)}</div>
    </td>
    <td>
      <div>${n(today.requests)} 次</div>
      <div class="${rateClass(rate)}">${rate.toFixed(1)}%</div>
    </td>
    <td>
      <select class="input compact" data-concurrency="${esc(account.id)}">
        ${[1,2,3,4,5].map((v) => `<option value="${v}" ${Number(account.concurrency || 1) === v ? 'selected' : ''}>${v}</option>`).join('')}
      </select>
    </td>
    <td class="muted-cell">${relative(today.lastUsed || account.lastUsedAt)}</td>
    <td class="actions">
      <button class="btn" data-action="up" data-id="${esc(account.id)}" ${index === 0 ? 'disabled' : ''}>↑</button>
      <button class="btn" data-action="down" data-id="${esc(account.id)}" ${index === state.accounts.length - 1 ? 'disabled' : ''}>↓</button>
      <button class="btn" data-action="fingerprint" data-id="${esc(account.id)}" title="重新生成设备指纹（device_mid 会变）">新指纹</button>
      <button class="btn" data-action="activities" data-id="${esc(account.id)}">活动</button>
      <button class="btn" data-action="refresh" data-id="${esc(account.id)}">刷新</button>
      ${account.status === '冷却中' || (account.modelStates || []).some((state) => state.disabled || Number(state.cooldownUntil) > nowSec) ? `<button class="btn" data-action="clear-cooldown" data-id="${esc(account.id)}">清除冷却</button>` : ''}
      <button class="btn ${account.enabled ? 'danger' : ''}" data-action="toggle" data-id="${esc(account.id)}">${account.enabled ? '禁用' : '启用'}</button>
      <button class="btn danger" data-action="delete" data-id="${esc(account.id)}">删除</button>
    </td>
  </tr>`;
}

async function moveAccount(id, direction) {
  const list = [...state.accounts];
  const index = list.findIndex((account) => account.id === id);
  const swapIndex = direction === 'up' ? index - 1 : index + 1;
  if (index < 0 || swapIndex < 0 || swapIndex >= list.length) return;
  const a = list[index];
  const b = list[swapIndex];
  try {
    await api(`/api/accounts/${encodeURIComponent(a.id)}`, { method: 'PATCH', body: JSON.stringify({ priority: b.priority }) });
    await api(`/api/accounts/${encodeURIComponent(b.id)}`, { method: 'PATCH', body: JSON.stringify({ priority: a.priority }) });
    await loadAccounts();
    await loadOverview();
  } catch (error) {
    toast(error.message, true);
  }
}

async function toggleAccount(id) {
  const account = state.accounts.find((item) => item.id === id);
  if (!account) return;
  try {
    await api(`/api/accounts/${encodeURIComponent(id)}`, {
      method: 'PATCH',
      body: JSON.stringify({ enabled: !account.enabled }),
    });
    await loadAccounts();
    await loadOverview();
    toast(account.enabled ? '账号已禁用' : '账号已启用');
  } catch (error) {
    toast(error.message, true);
  }
}

async function refreshAccount(id) {
  try {
    await api(`/api/accounts/${encodeURIComponent(id)}/refresh`, { method: 'POST' });
    await loadAccounts();
    await loadOverview();
    toast('额度已刷新');
  } catch (error) {
    toast(error.message, true);
  }
}

function activityPeriod(period) {
  if (period === 'one_time') return '一次性';
  if (period === 'daily') return '每日';
  return period || '';
}

function activityPlanCard(plan) {
  const entitlements = (plan.entitlements || []).map((item) => {
    const amount = units(item.amount);
    const unit = item.unit || 'Tokens';
    return `<span class="activity-benefit">${esc(item.model || item.entitlementId || '权益')} · ${amount} ${esc(unit)} ${esc(activityPeriod(item.period))}</span>`;
  }).join('');
  const windowText = [
    plan.startsAt ? `开始 ${timeText(plan.startsAt)}` : '',
    plan.endsAt ? `结束 ${timeText(plan.endsAt)}` : '',
    plan.effectiveAt ? `生效 ${timeText(plan.effectiveAt)}` : '',
  ].filter(Boolean).join(' · ');
  const claimed = !!plan.claimed;
  const claimable = !!plan.claimable && !claimed;
  const statusText = claimed ? '已领取' : claimable ? '可领取' : '不可领取';
  const statusClass = claimed ? 'info' : claimable ? 'ok' : 'warn';
  const buttonText = claimed ? '已领取' : claimable ? '领取' : '暂不可领取';
  return `<div class="activity-card">
    <div class="activity-card-head">
      <div>
        <div class="activity-card-title">${esc(plan.name || plan.planId)}</div>
        <div class="activity-card-meta mono">${esc(plan.planId)}</div>
        ${windowText ? `<div class="activity-card-meta">${esc(windowText)}</div>` : ''}
        ${claimed && plan.currentStatus ? `<div class="activity-card-meta">当前套餐状态：${esc(plan.currentStatus)}</div>` : ''}
      </div>
      <span class="tag ${statusClass}">${statusText}</span>
    </div>
    ${entitlements ? `<div class="activity-benefits">${entitlements}</div>` : ''}
    <button class="btn ${claimable ? 'primary' : ''}" data-claim-plan="${esc(plan.planId)}" data-plan-name="${esc(plan.name || plan.planId)}" ${claimable ? '' : 'disabled'}>${buttonText}</button>
  </div>`;
}

async function detectActivities(id) {
  const account = state.accounts.find((item) => item.id === id);
  state.activityAccountId = id;
  $('activities-title').textContent = account ? `活动 · ${account.label}` : '当前活动';
  const body = $('activities-body');
  body.innerHTML = '<div class="waiting"><span class="spinner"></span><span>正在检测活动…</span></div>';
  $('modal-activities').classList.remove('hidden');
  try {
    const data = await api(`/api/accounts/${encodeURIComponent(id)}/activities`);
    state.activityCaptcha = data.captcha || null;
    const plans = data.plans || [];
    const campaigns = data.campaigns || [];
    const warnings = data.warnings || [];
    if (!plans.length) {
      body.innerHTML = '<div class="empty small">当前没有检测到可领取活动。</div>';
      return;
    }
    const campaignText = campaigns.map((campaign) =>
      `campaign ${esc(campaign.campaignId)} · ${esc(campaign.position || '-')} · ${(campaign.planIds || []).map(esc).join(', ')}`).join('<br>');
    body.innerHTML = `
      <div class="activity-list">${plans.map(activityPlanCard).join('')}</div>
      ${campaignText ? `<div class="activity-campaigns">活动来源：<br>${campaignText}</div>` : ''}
      ${warnings.length ? `<div class="alert error">${warnings.map(esc).join('<br>')}</div>` : ''}
    `;
  } catch (error) {
    body.innerHTML = `<div class="alert error">${esc(error.message)}</div>`;
  }
}

let aliyunCaptchaScript = null;

function loadAliyunCaptchaScript() {
  if (aliyunCaptchaScript) return aliyunCaptchaScript;
  aliyunCaptchaScript = new Promise((resolve, reject) => {
    if (typeof window.initAliyunCaptcha === 'function') return resolve();
    const src = 'https://o.alicdn.com/captcha-frontend/aliyunCaptcha/AliyunCaptcha.js';
    const existing = document.querySelector(`script[src="${src}"]`);
    if (existing) {
      existing.addEventListener('load', resolve, { once: true });
      existing.addEventListener('error', () => reject(new Error('Aliyun 验证脚本加载失败')), { once: true });
      return;
    }
    const script = document.createElement('script');
    script.src = src;
    script.async = true;
    script.onload = resolve;
    script.onerror = () => reject(new Error('Aliyun 验证脚本加载失败'));
    document.head.appendChild(script);
  }).catch((error) => {
    aliyunCaptchaScript = null;
    throw error;
  });
  return aliyunCaptchaScript;
}

async function fetchCaptchaParam() {
  const config = state.activityCaptcha;
  if (!config || !config.enabled || !config.sceneId) {
    toast('后端没有返回 Aliyun 验证配置，请手动填写验证参数', true);
    return;
  }
  const button = $('activity-captcha-fetch');
  button.disabled = true;
  button.textContent = '获取中…';
  try {
    await loadAliyunCaptchaScript();
    const param = await new Promise((resolve, reject) => {
      window.AliyunCaptchaConfig = { region: config.region, prefix: config.prefix };
      let settled = false;
      const timer = setTimeout(() => {
        if (!settled) { settled = true; reject(new Error('Aliyun 验证超时，请重试')); }
      }, 30000);
      window.initAliyunCaptcha({
        SceneId: config.sceneId,
        mode: 'popup',
        element: '#aliyun-captcha-element',
        button: '#aliyun-captcha-button',
        language: 'zh',
        captchaVerifyCallback: (value) => {
          if (!settled) {
            settled = true;
            clearTimeout(timer);
            resolve(value);
          }
          return { captchaResult: true, bizResult: true };
        },
        onBizResultCallback: () => {},
        getInstance: (instance) => {
          setTimeout(() => {
            try { instance.startTracelessVerification?.(); } catch (_) {}
          }, 0);
        },
      });
    });
    $('activity-captcha').value = param || '';
    toast('验证参数已获取');
  } catch (error) {
    toast(error.message || '获取验证参数失败，请手动填写或使用官方客户端', true);
  } finally {
    button.disabled = false;
    button.textContent = '获取验证参数';
  }
}

async function claimActivity(planId, planName) {
  if (!state.activityAccountId) return;
  if (!window.confirm(`确认领取「${planName || planId}」？每个账号只能领取一次。`)) return;
  try {
    const captchaVerifyParam = ($('activity-captcha')?.value || '').trim();
    const data = await api(`/api/accounts/${encodeURIComponent(state.activityAccountId)}/claim`, {
      method: 'POST',
      body: JSON.stringify({ planId, captchaVerifyParam, captchaRegion: captchaVerifyParam ? 'cn' : '' }),
    });
    if (data.ok) {
      $('activity-captcha-field')?.classList.add('hidden');
      toast('领取成功');
      await loadAccounts();
      await loadOverview();
      await detectActivities(state.activityAccountId);
    } else {
      const blocked = data.code === 3012 || /unusual activity|异常/i.test(data.message || '');
      if (blocked) {
        $('activity-captcha-field')?.classList.remove('hidden');
        toast('上游风控拦截：填写 Aliyun captchaVerifyParam 后重试，或改用官方客户端领取', true);
      } else {
        toast(data.message || `领取失败（code ${data.code}）`, true);
      }
    }
  } catch (error) {
    toast(error.message, true);
  }
}

async function setConcurrency(id, value) {
  try {
    await api(`/api/accounts/${encodeURIComponent(id)}`, {
      method: 'PATCH',
      body: JSON.stringify({ concurrency: value }),
    });
    await loadAccounts();
    toast(`并发已设为 ${value}`);
  } catch (error) {
    toast(error.message, true);
  }
}

function fingerprintText(account) {
  if (!account || !account.deviceMid) return '指纹未生成';
  return `${account.platform || '?'} · ${account.osVersion || '?'} · ${account.screen || '?'} · ${shortId(account.deviceMid)}`;
}

async function rotateFingerprint(id) {
  const account = state.accounts.find((item) => item.id === id);
  const label = account ? account.label : id;
  if (!window.confirm(`为“${label}”生成一套新的设备指纹？\ndevice_mid、平台、系统版本、分辨率、语言/时区都会换。`)) return;
  try {
    await api(`/api/accounts/${encodeURIComponent(id)}/fingerprint`, { method: 'POST' });
    await loadAccounts();
    toast('已生成新指纹');
  } catch (error) {
    toast(error.message, true);
  }
}

async function clearCooldown(id) {
  try {
    await api(`/api/accounts/${encodeURIComponent(id)}/clear-cooldown`, { method: 'POST' });
    await loadAccounts();
    await loadOverview();
    toast('冷却已清除');
  } catch (error) {
    toast(error.message, true);
  }
}

async function deleteAccount(id) {
  const account = state.accounts.find((item) => item.id === id);
  if (!account) return;
  if (!window.confirm(`确定删除账号“${account.label}”？相关日志会保留。`)) return;
  try {
    await api(`/api/accounts/${encodeURIComponent(id)}`, { method: 'DELETE' });
    await loadAccounts();
    await loadOverview();
    toast('账号已删除');
  } catch (error) {
    toast(error.message, true);
  }
}

async function loadLogs() {
  const params = new URLSearchParams({ limit: '200' });
  if (state.logFilter.status) params.set('status', state.logFilter.status);
  if (state.logFilter.account) params.set('accountId', state.logFilter.account);
  try {
    const data = await api(`/api/logs?${params}`);
    state.logs = data.items || [];
    renderLogs();
  } catch (error) {
    toast(error.message, true);
  }
}

function renderLogs() {
  const tbody = $('logs-table').querySelector('tbody');
  tbody.innerHTML = state.logs.map(logRow).join('');
  $('logs-empty').classList.toggle('hidden', state.logs.length > 0);
  $('logs-table').classList.toggle('hidden', state.logs.length === 0);
}

function logRow(log) {
  return `<tr class="clickable" data-log="${esc(log.id)}">
    <td class="muted-cell">${timeText(log.time)}</td>
    <td><div>${esc(log.accountLabel || '-')}</div><div class="muted-cell account-id">${esc(shortId(log.accountId))}</div></td>
    <td class="mono ellipsis">${esc(log.model || '-')}</td>
    <td>${statusTag(log)}</td>
    <td class="num">${n(log.usage?.input_tokens)}</td>
    <td class="num">${n(log.usage?.cache_read_input_tokens)}</td>
    <td class="num">${n(log.usage?.cache_creation_input_tokens)}</td>
    <td class="num">${n(log.usage?.output_tokens)}</td>
    <td class="num">${n(log.usage?.reasoning_tokens)}</td>
    <td class="num muted-cell">${log.durationMs || 0}ms</td>
    <td class="num muted-cell">${msText(log.firstTokenMs)}</td>
    <td class="num muted-cell">${msText(log.lastTokenMs)}</td>
    <td class="num">${speedText(log.tokensPerSec)}</td>
    <td class="num muted-cell">›</td>
  </tr>`;
}

function renderLogAccountOptions() {
  const select = $('log-account');
  const current = select.value;
  select.innerHTML = '<option value="">全部账号</option>' + state.accounts.map((account) =>
    `<option value="${esc(account.id)}">${esc(account.label)}</option>`).join('');
  select.value = current;
}

async function openLog(id) {
  try {
    const log = await api(`/api/logs/${encodeURIComponent(id)}`);
    const usage = log.usage || {};
    const total = sumUsage(usage);
    $('log-title').textContent = `${log.model || '-'} · ${timeText(log.time)}`;
    $('log-detail').innerHTML = `
      <div class="log-meta">
        <div><span>状态</span><strong>${log.ok ? '成功' : `失败（HTTP ${log.status}）`}</strong></div>
        <div><span>账号</span><strong>${esc(log.accountLabel || '-')} · ${esc(shortId(log.accountId))}</strong></div>
        <div><span>会话</span><strong class="mono">${esc(log.sessionId || '-')}</strong></div>
        <div><span>模式</span><strong>${esc(modeLabel(log.mode))}</strong></div>
        <div><span>耗时</span><strong>${log.durationMs || 0} ms</strong></div>
        <div><span>首 Token</span><strong>${msText(log.firstTokenMs)}</strong></div>
        <div><span>最后流</span><strong>${msText(log.lastTokenMs)}</strong></div>
        <div><span>流时长</span><strong>${msText(log.streamMs)}</strong></div>
        <div><span>速度</span><strong>${speedText(log.tokensPerSec)}</strong></div>
        <div><span>请求 ID</span><strong class="mono">${esc(log.requestId || '-')}</strong></div>
        <div><span>客户端</span><strong class="mono">${esc(log.clientIp || '-')}</strong></div>
        <div><span>请求大小</span><strong>${n(log.requestBytes)} B</strong></div>
        <div><span>响应大小</span><strong>${n(log.responseBytes)} B</strong></div>
      </div>
      ${log.error ? `<div class="alert error">${esc(log.error)}</div>` : ''}
      <div class="usage-grid">
        <div class="usage-cell"><span>输入</span><strong>${n(usage.input_tokens)}</strong></div>
        <div class="usage-cell"><span>缓存读</span><strong>${n(usage.cache_read_input_tokens)}</strong></div>
        <div class="usage-cell"><span>缓存写</span><strong>${n(usage.cache_creation_input_tokens)}</strong></div>
        <div class="usage-cell"><span>输出</span><strong>${n(usage.output_tokens)}</strong></div>
        <div class="usage-cell"><span>思考</span><strong>${n(usage.reasoning_tokens)}</strong></div>
      </div>
      <div class="detail-label"><span>合计 Tokens</span><strong>${n(total)}</strong></div>
      <div class="detail-label"><span>请求内容</span><span class="muted">${log.requestBody ? '截断预览' : '未保存'}</span></div>
      <pre class="pre-block">${esc(log.requestBody || '（未保存请求内容）')}</pre>
      <div class="detail-label"><span>上游响应</span><span class="muted">${log.responseBody ? '截断预览' : '未保存'}</span></div>
      <pre class="pre-block">${esc(log.responseBody || '（未保存响应内容）')}</pre>
    `;
    $('drawer-log').classList.remove('hidden');
  } catch (error) {
    toast(error.message, true);
  }
}

async function loadSettings() {
  try {
    const data = await api('/api/settings');
    state.settings = data.settings;
    $('setting-token').value = data.settings.proxyToken || '';
    $('setting-bodies').checked = !!data.settings.storeBodies;
    $('setting-fingerprint').checked = !!data.settings.fingerprint;
    $('setting-prompt-mode').value = data.settings.promptMode || 'replace';
    $('setting-ua').value = data.settings.uaProfile || 'zcode';
    $('setting-quota').value = data.settings.quotaIntervalSec || 30;
    $('setting-base').textContent = data.server.baseUrl;
    $('setting-version').textContent = `v${data.server.version}`;
    $('setting-datadir').textContent = data.server.dataDir;
  } catch (error) {
    toast(error.message, true);
  }
}

async function saveSettings() {
  try {
    const body = {
      proxyToken: $('setting-token').value.trim(),
      storeBodies: $('setting-bodies').checked,
      fingerprint: $('setting-fingerprint').checked,
      promptMode: $('setting-prompt-mode').value,
      uaProfile: $('setting-ua').value,
      quotaIntervalSec: Number($('setting-quota').value) || 30,
    };
    await api('/api/settings', { method: 'PATCH', body: JSON.stringify(body) });
    toast('设置已保存');
    await loadSettings();
  } catch (error) {
    toast(error.message, true);
  }
}

function openAddModal() {
  $('modal-add').classList.remove('hidden');
  $('add-step-1').classList.remove('hidden');
  $('add-step-2').classList.add('hidden');
  $('add-error').classList.add('hidden');
  $('callback-url').value = '';
  stopAddPolling();
}

function closeModal(id) {
  $(id).classList.add('hidden');
  if (id === 'modal-add') stopAddPolling();
}

async function generateAuthUrl() {
  const provider = $('add-provider').value;
  const button = $('generate-url');
  button.disabled = true;
  button.textContent = '生成中…';
  try {
    const data = await api('/api/accounts/oauth/start', {
      method: 'POST',
      body: JSON.stringify({ provider }),
    });
    state.addFlow = data;
    $('auth-url').value = data.authorizeUrl;
    $('add-step-1').classList.add('hidden');
    $('add-step-2').classList.remove('hidden');
    $('auth-waiting').classList.remove('hidden');
    startAddPolling(data.flowId);
  } catch (error) {
    $('add-error').textContent = error.message;
    $('add-error').classList.remove('hidden');
  } finally {
    button.disabled = false;
    button.textContent = '生成授权链接';
  }
}

function startAddPolling(flowId) {
  stopAddPolling();
  state.addTimer = setInterval(async () => {
    try {
      const data = await api(`/api/accounts/oauth/status?flowId=${encodeURIComponent(flowId)}`);
      if (data.status === 'ready') {
        stopAddPolling();
        $('auth-waiting').classList.add('hidden');
        closeModal('modal-add');
        toast(`账号已添加：${data.account?.label || ''}`);
        await loadAccounts();
        await loadOverview();
      } else if (data.status === 'error') {
        stopAddPolling();
        $('auth-waiting').classList.add('hidden');
        $('add-error').textContent = data.error || '授权失败';
        $('add-error').classList.remove('hidden');
      }
    } catch (error) {
      // Polling errors are transient; keep waiting.
    }
  }, 2000);
}

function stopAddPolling() {
  clearInterval(state.addTimer);
  state.addTimer = null;
}

async function submitCallback() {
  const url = $('callback-url').value.trim();
  if (!url) {
    $('add-error').textContent = '请粘贴完整的回调 URL';
    $('add-error').classList.remove('hidden');
    return;
  }
  try {
    const data = await api('/api/accounts/oauth/callback', {
      method: 'POST',
      body: JSON.stringify({ url }),
    });
    closeModal('modal-add');
    toast(`账号已添加：${data.account?.label || ''}`);
    await loadAccounts();
    await loadOverview();
  } catch (error) {
    $('add-error').textContent = error.message;
    $('add-error').classList.remove('hidden');
  }
}

async function importLocalAccount() {
  try {
    const data = await api('/api/accounts/import', { method: 'POST' });
    await loadAccounts();
    await loadOverview();
    toast(`已导入 ${data.items?.length || 0} 个账号`);
  } catch (error) {
    toast(error.message, true);
  }
}

function generateProxyToken() {
  const length = 24;
  const bytes = new Uint8Array(length);
  if (window.crypto && window.crypto.getRandomValues) {
    window.crypto.getRandomValues(bytes);
  } else {
    for (let i = 0; i < length; i += 1) bytes[i] = Math.floor(Math.random() * 256);
  }
  const token = 'sk-' + Array.from(bytes, (byte) => byte.toString(16).padStart(2, '0')).join('');
  $('setting-token').value = token;
  copyText(token);
  toast('已生成并复制 sk- Token，记得点“保存设置”');
}

function copyText(text) {
  if (!text) return;
  const copied = () => toast('已复制');
  const fallback = () => {
    const input = document.createElement('textarea');
    input.value = text;
    input.setAttribute('readonly', '');
    input.style.position = 'fixed';
    input.style.top = '-1000px';
    input.style.opacity = '0';
    document.body.appendChild(input);
    input.select();
    input.setSelectionRange(0, input.value.length);
    let ok = false;
    try { ok = document.execCommand('copy'); } catch (_) { ok = false; }
    input.remove();
    if (ok) copied();
    else toast('复制失败，请手动选中链接复制', true);
  };
  // Clipboard API is only available in secure contexts (HTTPS or localhost).
  // The gateway is commonly opened over plain http://<LAN-IP>, where
  // navigator.clipboard is undefined; calling .then() on it throws and skips
  // the fallback entirely.
  if (window.isSecureContext && navigator.clipboard?.writeText) {
    navigator.clipboard.writeText(text).then(copied).catch(fallback);
  } else {
    fallback();
  }
}

// Events
$('nav').addEventListener('click', (event) => {
  const button = event.target.closest('.nav-item');
  if (button) setView(button.dataset.view);
});
document.addEventListener('click', (event) => {
  const go = event.target.closest('[data-go]');
  if (go) setView(go.dataset.go);
  const close = event.target.closest('[data-close]');
  if (close) closeModal(close.dataset.close);
  const logRow = event.target.closest('[data-log]');
  if (logRow) openLog(logRow.dataset.log);
  const action = event.target.closest('[data-action]');
  if (action) {
    const id = action.dataset.id;
    if (action.dataset.action === 'up') moveAccount(id, 'up');
    if (action.dataset.action === 'down') moveAccount(id, 'down');
    if (action.dataset.action === 'toggle') toggleAccount(id);
    if (action.dataset.action === 'refresh') refreshAccount(id);
    if (action.dataset.action === 'activities') detectActivities(id);
    if (action.dataset.action === 'fingerprint') rotateFingerprint(id);
    if (action.dataset.action === 'clear-cooldown') clearCooldown(id);
    if (action.dataset.action === 'delete') deleteAccount(id);
  }
  const claim = event.target.closest('[data-claim-plan]');
  if (claim) claimActivity(claim.dataset.claimPlan, claim.dataset.planName);
});

$('refresh-all').addEventListener('click', async () => {
  await Promise.all([loadOverview(), loadAccounts(), loadLogs()]);
  toast('已刷新');
});
$('copy-api').addEventListener('click', () => copyText($('api-base').textContent));
$('logout').addEventListener('click', async () => {
  try { await fetch('/api/auth/logout', { method: 'POST' }); } catch (_) {}
  window.location.href = '/';
});
$('add-account').addEventListener('click', openAddModal);
$('add-account-empty').addEventListener('click', openAddModal);
$('import-local').addEventListener('click', importLocalAccount);
$('generate-url').addEventListener('click', generateAuthUrl);
$('copy-auth-url').addEventListener('click', () => copyText($('auth-url').value));
$('open-auth-url').addEventListener('click', () => {
  const url = $('auth-url').value;
  if (url) window.open(url, '_blank', 'noopener');
});
$('submit-callback').addEventListener('click', submitCallback);
$('save-settings').addEventListener('click', saveSettings);
$('activity-captcha-fetch').addEventListener('click', fetchCaptchaParam);
$('generate-token').addEventListener('click', generateProxyToken);
$('copy-token').addEventListener('click', () => copyText($('setting-token').value));
$('refresh-logs').addEventListener('click', loadLogs);
document.addEventListener('change', (event) => {
  const concurrency = event.target.closest('[data-concurrency]');
  if (concurrency) setConcurrency(concurrency.dataset.concurrency, Number(concurrency.value));
});
$('log-status').addEventListener('change', (event) => {
  state.logFilter.status = event.target.value;
  loadLogs();
});
$('log-account').addEventListener('change', (event) => {
  state.logFilter.account = event.target.value;
  loadLogs();
});

// Initial load + 30s polling.
loadOverview();
loadAccounts();
loadSettings();
setTimeout(loadLogs, 200);
setInterval(() => {
  loadOverview();
  if (state.view === 'accounts') loadAccounts();
}, 30000);