// Accounts management JavaScript

let accounts = [];
let currentPlatform = '';
let accountHealth = {};
let pageSize = 20;
let currentPage = 1;
let modelCatalog = [];

// DOM 缓存
const domCache = {
    accountsList: null,
    paginationInfo: null,
    paginationControls: null,
};

function initDOMCache() {
    domCache.accountsList = document.getElementById("accountsList");
    domCache.paginationInfo = document.getElementById("paginationInfo");
    domCache.paginationControls = document.getElementById("paginationControls");
}

const fallbackAgentModes = {
  orchids: [
    "claude-sonnet-4-5",
    "claude-opus-4-6",
    "claude-opus-4-6-thinking",
    "claude-opus-4-5",
    "claude-sonnet-4-5-thinking",
  ],
  warp: [
    "auto",
    "auto-efficient",
    "auto-genius",
    "claude-4-5-sonnet",
    "claude-4-5-sonnet-thinking",
    "claude-4-5-opus",
    "claude-4-5-opus-thinking",
    "claude-4-6-opus-high",
    "claude-4-6-opus-max",
  ],
  bolt: [
    "claude-opus-4-6",
    "claude-sonnet-4-5",
    "claude-3-7-sonnet-20250219",
  ],
  puter: [
    "claude-opus-4-5",
    "claude-sonnet-4-5",
    "claude-3-7-sonnet-20250219",
  ],
  v0: [
    "v0-max",
  ],
  grok: [
    "grok-3",
    "grok-3-mini",
    "grok-3-thinking",
    "grok-4",
    "grok-4-mini",
    "grok-4-thinking",
    "grok-4-heavy",
    "grok-4.1-mini",
    "grok-4.1-fast",
    "grok-4.1-expert",
    "grok-4.1-thinking",
    "grok-imagine-1.0",
    "grok-imagine-1.0-edit",
    "grok-imagine-1.0-video",
  ],
};

function normalizeChannel(channel) {
  return String(channel || "orchids").trim().toLowerCase();
}

function isModelAvailable(model) {
  if (!model) return false;
  if (typeof model.status === "boolean") return model.status;
  const status = String(model.status || "").toLowerCase();
  if (!status) return true;
  return status === "available";
}

function bySortOrder(a, b) {
  const aOrderRaw = a && a.sort_order;
  const bOrderRaw = b && b.sort_order;
  const aOrder = Number.isFinite(Number(aOrderRaw)) ? Number(aOrderRaw) : 0;
  const bOrder = Number.isFinite(Number(bOrderRaw)) ? Number(bOrderRaw) : 0;
  if (aOrder !== bOrder) return aOrder - bOrder;
  const aModelID = a && a.model_id ? a.model_id : "";
  const bModelID = b && b.model_id ? b.model_id : "";
  return String(aModelID).localeCompare(String(bModelID));
}

function getModelsForAccountType(type) {
  const channel = normalizeChannel(type);
  const active = modelCatalog
    .filter((m) => normalizeChannel(m.channel) === channel)
    .filter(isModelAvailable)
    .sort(bySortOrder);

  if (active.length > 0) return active;

  const fallback = fallbackAgentModes[channel] || [];
  return fallback.map((modelID, idx) => ({
    model_id: modelID,
    name: modelID,
    sort_order: idx,
    is_default: idx === 0,
  }));
}

async function loadModelCatalog() {
  try {
    const res = await fetch("/api/models");
    if (!res.ok) {
      throw new Error(`status ${res.status}`);
    }
    const list = await res.json();
    modelCatalog = Array.isArray(list) ? list : [];
  } catch (err) {
    console.error("Failed to load models:", err);
    modelCatalog = [];
  }
}

// Load accounts from API
async function loadAccounts() {
  try {
    const [res] = await Promise.all([
      fetch("/api/accounts"),
      loadModelCatalog(),
    ]);
    if (res.status === 401) {
      window.location.href = "./login.html";
      return;
    }
    accounts = await res.json();
    sortAccounts();
    renderPlatformTabs();
    renderAccounts();
    updateStats();
    autoRefreshWarpAccounts();
  } catch (err) {
    console.error("Failed to load accounts:", err);
    showToast("加载账号失败", "error");
  }
}

// Sort accounts (Default by ID desc)
function sortAccounts() {
  accounts.sort((a, b) => b.id - a.id);
}

// Normalize account type
function normalizeAccountType(acc) {
  return (acc.account_type || 'orchids').toLowerCase();
}

function getQuotaStats(acc) {
  if (!acc) return null;
  const type = normalizeAccountType(acc);
  if (type === "puter") {
    return { supported: false, unknown: true };
  }
  if (type === "warp") {
    const monthlyLimit = Math.max(0, Math.floor(acc.warp_monthly_limit || acc.usage_limit || 0));
    const monthlyRemainingRaw = acc.warp_monthly_remaining !== undefined && acc.warp_monthly_remaining !== null
      ? acc.warp_monthly_remaining
      : (monthlyLimit > 0 ? monthlyLimit - Math.floor(acc.usage_current || 0) : 0);
    const monthlyRemaining = Math.max(0, Math.floor(monthlyRemainingRaw || 0));
    const bonusRemaining = Math.max(0, Math.floor(acc.warp_bonus_remaining || 0));
    const remaining = monthlyRemaining + bonusRemaining;
    if (monthlyLimit > 0 || bonusRemaining > 0) {
      const displayTotal = monthlyLimit + bonusRemaining;
      const pctRemaining = displayTotal > 0 ? Math.min(100, Math.round((remaining / displayTotal) * 100)) : 0;
      return {
        supported: true,
        limit: monthlyLimit,
        remaining,
        used: Math.max(0, Math.floor(acc.usage_current || 0)),
        pctRemaining,
        monthlyLimit,
        monthlyRemaining,
        bonusRemaining,
        splitBonus: bonusRemaining > 0,
      };
    }
  }
  const explicitLimit = Math.floor(acc.quota_limit || 0);
  const hasExplicitRemaining = acc.quota_remaining !== undefined && acc.quota_remaining !== null;
  if (explicitLimit > 0 && hasExplicitRemaining) {
    const remaining = Math.max(0, Math.floor(acc.quota_remaining || 0));
    const used = Math.max(0, explicitLimit - remaining);
    const pctRemaining = explicitLimit > 0 ? Math.min(100, Math.round((remaining / explicitLimit) * 100)) : 0;
    return { supported: true, limit: explicitLimit, remaining, used, pctRemaining };
  }

  const limit = Math.floor(acc.usage_limit || 0);
  if (limit <= 0) return null;
  const current = Math.floor(acc.usage_current || 0);
  let remaining = 0;
  if (type === "warp") {
    remaining = Math.max(0, limit - current);
  } else {
    remaining = Math.max(0, current);
  }
  const used = Math.max(0, limit - remaining);
  const pctRemaining = limit > 0 ? Math.min(100, Math.round((remaining / limit) * 100)) : 0;
  return { supported: true, limit, remaining, used, pctRemaining };
}

function getAccountToken(acc) {
  if (!acc) return '';
  const type = normalizeAccountType(acc);
  if (type === 'warp') {
    return acc.refresh_token || acc.token || acc.client_cookie || '';
  }
  if (type === 'orchids') {
    return acc.client_cookie || acc.session_cookie || acc.token || '';
  }
  if (type === 'bolt') {
    return acc.session_cookie || acc.client_cookie || acc.token || '';
  }
  if (type === 'puter') {
    return acc.client_cookie || acc.token || acc.session_cookie || '';
  }
  if (type === 'v0') {
    return acc.client_cookie || acc.token || acc.session_cookie || '';
  }
  return acc.client_cookie || acc.token || '';
}

function applyTokenLabels(type) {
  const label = document.getElementById("tokenLabel");
  const input = document.getElementById("clientCookie");
  const hint = document.getElementById("tokenHint");
  const projectGroup = document.getElementById("projectIdGroup");
  const projectInput = document.getElementById("projectId");
  const accountId = String(document.getElementById("accountId")?.value || "");
  if (!label || !input || !hint) return;
  if (projectGroup && projectInput) {
    const boltMode = type === "bolt";
    projectGroup.style.display = boltMode ? "" : "none";
    projectInput.required = boltMode;
  }
  if (type === 'warp') {
    label.textContent = "Refresh Token";
    input.placeholder = "每行一个 refresh_token";
    hint.textContent = accountId
      ? "编辑时仅保存第一行 refresh_token"
      : "支持批量添加 Warp。每行一个 refresh_token";
    input.required = true;
  } else if (type === 'grok') {
    label.textContent = "SSO Token";
    input.placeholder = "每行一个 sso token（或包含 sso= 的 Cookie）";
    hint.textContent = accountId
      ? "编辑时仅保存第一行 SSO Token"
      : "支持批量添加 Grok。每行一个 sso token 或 Cookie 片段";
  } else if (type === 'bolt') {
    label.textContent = "__session";
    input.placeholder = "每行一个 Bolt __session";
    hint.textContent = accountId
      ? "Bolt 编辑时仅保存第一行 __session，并保留当前 project_id"
      : "支持批量添加 Bolt。每行一个 __session，project_id 共用下方输入";
    input.required = true;
  } else if (type === 'puter') {
    label.textContent = "Auth Token";
    input.placeholder = "每行一个 Puter auth_token";
    hint.textContent = accountId
      ? "Puter 编辑时仅保存第一行 auth_token"
      : "支持批量添加 Puter。每行一个 auth_token";
    input.required = true;
  } else if (type === 'v0') {
    label.textContent = "Cookie / user_session";
    input.placeholder = "优先粘贴完整 Cookie Header；至少包含 user_session";
    hint.textContent = accountId
      ? "v0 编辑时仅保存第一行。强烈建议使用浏览器完整 Cookie Header，只有裸 user_session 往往不足以发消息"
      : "支持批量添加 v0。每行一个完整 Cookie Header；若只填 user_session，可能只能查信息，无法正常发消息";
    input.required = true;
  } else {
    label.textContent = "Cookie / __client / __session";
    input.placeholder = "支持原始 __client、完整 Cookie Header 或 Cookie JSON";
    hint.textContent = accountId
      ? "支持直接粘贴 clerk.orchids.app 的原始 __client；完整 Cookie 成功率更高，只有 www.orchids.app 的 __session 通常不够"
      : "支持原始 __client、完整 Cookie Header 或 Cookie JSON；推荐同时带上 __client_uat 以提高补全成功率";
    input.required = true;
  }
}

function splitBatchCredentialInput(raw) {
  const text = String(raw || "").trim();
  if (!text) return [];
  if (/^[\[{]/.test(text)) {
    return [text];
  }
  const lines = text
    .split(/\r?\n/)
    .map(line => line.trim())
    .filter(Boolean);
  if (lines.length > 1) {
    return lines;
  }
  return [text];
}

function resolveAgentMode(type, preferredValue = "") {
  const preferred = String(preferredValue || "").trim();
  if (preferred) return preferred;
  const models = getModelsForAccountType(type);
  const defaultModel = models.find((m) => m.is_default) || models[0];
  return defaultModel ? String(defaultModel.model_id || "").trim() : "";
}

// Render platform filter tabs
function renderPlatformTabs() {
  const container = document.getElementById("platformFilters");
  if (!container) return;
  const defaultTypes = ["orchids", "warp", "bolt", "puter", "v0", "grok"];
  const types = new Set([...defaultTypes, ...accounts.map(normalizeAccountType)]);
  const sorted = Array.from(types).sort();
  const tabs = [...sorted];

  if (currentPlatform === '' || !tabs.includes(currentPlatform)) {
    currentPlatform = tabs.length > 0 ? tabs[0] : '';
  }

  container.innerHTML = "";
  tabs.forEach(type => {
    const label = String(type || "");
    const isActive = currentPlatform === label;
    const btn = document.createElement("button");
    btn.className = `tab-item ${isActive ? 'active' : ''}`.trim();
    btn.dataset.platform = encodeURIComponent(label);
    btn.textContent = label;
    btn.addEventListener("click", () => {
      const raw = btn.dataset.platform ? decodeURIComponent(btn.dataset.platform) : "";
      filterByPlatform(raw);
    });
    container.appendChild(btn);
  });
}

// Update account health status
function updateAccountHealth(id, ok, msg = '') {
  accountHealth[id] = {
    ok,
    msg,
    checkedAt: new Date().toISOString(),
  };
}

function normalizeStatusCode(statusCode) {
  if (statusCode === null || statusCode === undefined) return '';
  return String(statusCode).trim();
}

function evaluateAccountStatus(acc) {
  const health = accountHealth[acc.id];
  if (health && !health.ok) {
    return { normal: false, text: '异常', color: '#fb7185', bg: 'rgba(251, 113, 133, 0.16)', tip: health.msg || '状态同步失败' };
  }
  if (!acc.enabled) {
    return { normal: false, text: '禁用', color: '#fb7185', bg: 'rgba(251, 113, 133, 0.16)', tip: '账号已禁用' };
  }
  const statusCode = normalizeStatusCode(acc.status_code);
  if (statusCode) {
    switch (statusCode) {
      case '429':
        return { normal: false, text: '限流', color: '#f59e0b', bg: 'rgba(245, 158, 11, 0.16)', tip: '请求过于频繁 (429)' };
      case '401':
        return { normal: false, text: '未授权', color: '#fb7185', bg: 'rgba(251, 113, 133, 0.16)', tip: '认证失败 (401)' };
      case '403':
        return { normal: false, text: '禁止访问', color: '#fb7185', bg: 'rgba(251, 113, 133, 0.16)', tip: '访问被拒绝 (403)' };
      case '404':
        return { normal: false, text: '不存在', color: '#fb7185', bg: 'rgba(251, 113, 133, 0.16)', tip: '资源不存在 (404)' };
      default:
        return { normal: false, text: '异常', color: '#fb7185', bg: 'rgba(251, 113, 133, 0.16)', tip: '状态异常: ' + statusCode };
    }
  }

  const type = normalizeAccountType(acc);
  if (type === 'warp') {
    if (!getAccountToken(acc)) {
      return { normal: false, text: '待补全', color: '#f59e0b', bg: 'rgba(245, 158, 11, 0.16)', tip: '缺少 Refresh Token' };
    }
  } else if (type === 'grok') {
    if (!getAccountToken(acc)) {
      return { normal: false, text: '待补全', color: '#f59e0b', bg: 'rgba(245, 158, 11, 0.16)', tip: '缺少 SSO Token' };
    }
  } else if (type === 'bolt') {
    if (!getAccountToken(acc) || !acc.project_id) {
      return { normal: false, text: '待补全', color: '#f59e0b', bg: 'rgba(245, 158, 11, 0.16)', tip: '缺少 Bolt __session 或 project_id' };
    }
  } else if (type === 'puter') {
    if (!getAccountToken(acc)) {
      return { normal: false, text: '待补全', color: '#f59e0b', bg: 'rgba(245, 158, 11, 0.16)', tip: '缺少 Puter auth_token' };
    }
  } else if (type === 'v0') {
    if (!getAccountToken(acc)) {
      return { normal: false, text: '待补全', color: '#f59e0b', bg: 'rgba(245, 158, 11, 0.16)', tip: '缺少 v0 user_session' };
    }
  } else if (!acc.session_id && !acc.session_cookie) {
    return { normal: false, text: '待补全', color: '#f59e0b', bg: 'rgba(245, 158, 11, 0.16)', tip: '缺少会话信息' };
  }

  const quota = getQuotaStats(acc);
  if (quota && quota.limit > 0 && quota.remaining <= 0) {
    return { normal: false, text: '配额已满', color: '#fb7185', bg: 'rgba(251, 113, 133, 0.16)', tip: '配额已用尽 (剩余 0 / ' + quota.limit.toLocaleString() + ')' };
  }

  return { normal: true, text: '正常', color: '#34d399', bg: 'rgba(52, 211, 153, 0.16)', tip: '状态正常' };
}

function isAccountAbnormal(acc) {
  return !evaluateAccountStatus(acc).normal;
}

// Get status badge for account
function statusBadge(acc) {
  return evaluateAccountStatus(acc);
}

// Refresh single account via the shared check endpoint.
async function checkAccount(id, silent = false, actionText = "刷新") {
  const action = "check";
  try {
    const res = await fetch(`/api/accounts/${id}/${action}`);
    if (!res.ok) {
      throw new Error(await res.text());
    }
    const updated = await res.json();
    accounts = accounts.map(a => (a.id === id ? updated : a));
    updateAccountHealth(id, true);
    if (!silent) showToast(`账号 ${updated.name || updated.email || id} ${actionText}完成`, "success");
  } catch (err) {
    try {
      const latestRes = await fetch(`/api/accounts/${id}`);
      if (latestRes.ok) {
        const latest = await latestRes.json();
        accounts = accounts.map(a => (a.id === id ? latest : a));
        delete accountHealth[id];
      } else {
        updateAccountHealth(id, false, err.message || String(err));
      }
    } catch (_) {
      updateAccountHealth(id, false, err.message || String(err));
    }
    if (!silent) showToast(`账号 ${id} ${actionText}失败`, "error");
  } finally {
    renderAccounts();
    updateStats();
  }
}

async function autoRefreshWarpAccounts() {
  const warpAccounts = accounts.filter(acc => normalizeAccountType(acc) === 'warp');
  if (!warpAccounts.length) return;
  for (const acc of warpAccounts) {
    if (acc.token) continue;
    await checkAccount(acc.id, true);
  }
}

// Delete all accounts
async function deleteAllAccounts() {
  if (!accounts.length) return;
  if (!confirm(`确定要删除全部 ${accounts.length} 个账号吗？此操作不可恢复。`)) return;
  for (const acc of accounts) {
    await fetch(`/api/accounts/${acc.id}`, { method: "DELETE" });
  }
  await loadAccounts();
  showToast("已删除全部账号", "success");
}

// Clear abnormal accounts
async function clearAbnormalAccounts() {
  const abnormal = accounts.filter(isAccountAbnormal);
  if (abnormal.length === 0) {
    showToast("没有异常账号", "info");
    return;
  }
  if (confirm(`确定要清空 ${abnormal.length} 个异常账号吗？`)) {
    for (const acc of abnormal) {
      await fetch(`/api/accounts/${acc.id}`, { method: "DELETE" });
    }
    loadAccounts();
    showToast("已清空异常账号");
  }
}

// Batch delete accounts
async function batchDeleteAccounts() {
  const selected = Array.from(document.querySelectorAll(".row-checkbox:checked")).map(cb => cb.dataset.id);
  if (selected.length === 0) return;
  if (confirm(`确定要删除选中的 ${selected.length} 个账号吗？`)) {
    for (const id of selected) {
      await fetch(`/api/accounts/${id}`, { method: "DELETE" });
    }
    loadAccounts();
    showToast(`已成功删除 ${selected.length} 个账号`);
  }
}

function getSelectedAccountIDs() {
  return Array.from(document.querySelectorAll(".row-checkbox:checked"))
    .map((cb) => parseDataId(cb.dataset.id || ""))
    .map((id) => Number(id))
    .filter((id) => Number.isFinite(id) && id > 0);
}

// Enable NSFW for selected Grok accounts, or all Grok accounts when nothing selected.
async function enableNSFW() {
  const selectedIDs = getSelectedAccountIDs();
  const selectedGrokIDs = selectedIDs.filter((id) => {
    const acc = accounts.find((item) => item.id === id);
    return !!acc && normalizeAccountType(acc) === "grok";
  });

  const payload = { concurrency: 5 };
  let targetText = "全部 Grok 账号";

  if (selectedIDs.length > 0) {
    if (selectedGrokIDs.length === 0) {
      showToast("选中的账号中没有 Grok 账号", "info");
      return;
    }
    payload.account_ids = selectedGrokIDs;
    targetText = `选中的 ${selectedGrokIDs.length} 个 Grok 账号`;
    if (!confirm(`确认对${targetText}启用 NSFW 吗？`)) return;
  } else if (!confirm("未选中账号，将对全部 Grok 账号启用 NSFW，是否继续？")) {
    return;
  }

  try {
    showToast(`正在为${targetText}启用 NSFW...`, "info");
    const res = await fetch("/api/v1/admin/tokens/nsfw/enable", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(payload),
    });

    if (res.status === 401) {
      window.location.href = "./login.html";
      return;
    }
    if (!res.ok) {
      throw new Error(await res.text());
    }

    const out = await res.json();
    const summary = out && out.summary ? out.summary : {};
    const total = Number(summary.total || 0);
    const ok = Number(summary.ok || 0);
    const fail = Number(summary.fail || Math.max(0, total - ok));

    if (ok > 0) {
      await loadAccounts();
    }

    if (fail > 0) {
      const failedList = Object.entries(out && out.results ? out.results : {})
        .filter(([, item]) => !item || item.success !== true)
        .slice(0, 3)
        .map(([token, item]) => {
          const msg = item && item.error ? item.error : `HTTP ${item && item.http_status ? item.http_status : 0}`;
          return `${token}: ${msg}`;
        });
      if (failedList.length > 0) {
        console.warn("NSFW enable failures:", failedList.join(" | "));
      }
      showToast(`NSFW 启用完成：成功 ${ok}，失败 ${fail}`, "error");
      return;
    }

    showToast(`NSFW 启用成功：共 ${ok}/${total}`, "success");
  } catch (err) {
    showToast(`NSFW 启用失败: ${err.message || err}`, "error");
  }
}

// Render accounts table
function renderAccounts() {
  const container = domCache.accountsList || document.getElementById("accountsList");
  const filtered = accounts.filter(acc => {
    if (!currentPlatform) return true;
    const key = currentPlatform.toLowerCase();
    return normalizeAccountType(acc).includes(key);
  });

  const total = filtered.length;
  const totalPages = Math.ceil(total / pageSize) || 1;
  if (currentPage > totalPages) currentPage = totalPages;
  if (currentPage < 1) currentPage = 1;

  const start = (currentPage - 1) * pageSize;
  const end = start + pageSize;
  const pageItems = filtered.slice(start, end);

  if (pageItems.length === 0) {
    container.innerHTML = "";
    const empty = document.createElement("div");
    empty.className = "empty-state";
    empty.style.display = "flex";
    empty.style.flexDirection = "column";
    empty.style.alignItems = "center";
    empty.style.justifyContent = "center";
    empty.style.height = "300px";
    empty.style.color = "#94a3b8";
    const icon = document.createElement("span");
    icon.style.fontSize = "3rem";
    icon.style.marginBottom = "16px";
    icon.textContent = "📂";
    const text = document.createElement("p");
    text.textContent = `暂无 ${currentPlatform ? currentPlatform : ''} 账号数据`;
    empty.appendChild(icon);
    empty.appendChild(text);
    container.appendChild(empty);
    const paginationInfo = domCache.paginationInfo || document.getElementById("paginationInfo");
    paginationInfo.textContent = `共 0 条记录，第 1/1 页`;
    renderPagination(1, 1);
    return;
  }

  container.innerHTML = "";
  const wrap = document.createElement("div");
  wrap.className = "table-wrap";
  const table = document.createElement("table");
  table.className = "accounts-table";
  const thead = document.createElement("thead");
  const headRow = document.createElement("tr");
  const headers = [
    { label: "", style: "width: 40px;" },
    { label: "ID", style: "width: 60px;" },
    { label: "Token" },
    { label: "模型" },
    { label: "配额", style: "width: 140px;" },
    { label: "状态" },
    { label: "调用" },
    { label: "最后调用" },
    { label: "操作", style: "text-align: right;" },
  ];
  headers.forEach((h, idx) => {
    const th = document.createElement("th");
    if (h.style) th.style.cssText = h.style;
    if (h.label === "Token") th.classList.add("col-token");
    if (idx === 0) {
      const selectAll = document.createElement("input");
      selectAll.type = "checkbox";
      selectAll.dataset.action = "select-all";
      th.appendChild(selectAll);
    } else {
      th.textContent = h.label;
    }
    headRow.appendChild(th);
  });
  thead.appendChild(headRow);
  table.appendChild(thead);

  const tbody = document.createElement("tbody");
  
  // 使用 DocumentFragment 批量构建表格行
  const fragment = document.createDocumentFragment();
  pageItems.forEach((acc) => {
    const badge = statusBadge(acc);
    const tokenDisplay = formatTokenDisplay(acc);
    const tr = document.createElement("tr");

    const tdCheck = document.createElement("td");
    const cb = document.createElement("input");
    cb.type = "checkbox";
    cb.className = "row-checkbox";
    cb.dataset.action = "row-select";
    cb.dataset.id = encodeData(acc.id);
    tdCheck.appendChild(cb);
    tr.appendChild(tdCheck);

    const tdID = document.createElement("td");
    tdID.style.color = "#64748b";
    tdID.style.fontSize = "0.9rem";
    tdID.textContent = acc.id === null || acc.id === undefined ? "" : String(acc.id);
    tr.appendChild(tdID);

    const tdToken = document.createElement("td");
    tdToken.className = "col-token";
    const tokenSpan = document.createElement("span");
    tokenSpan.className = "token-text";
    tokenSpan.title = tokenDisplay;
    tokenSpan.style.fontFamily = "monospace";
    tokenSpan.style.color = "#94a3b8";
    tokenSpan.textContent = tokenDisplay;
    tdToken.appendChild(tokenSpan);
    tr.appendChild(tdToken);

    const tdModel = document.createElement("td");
    const modelSpan = document.createElement("span");
    modelSpan.className = "tag tag-free";
    modelSpan.textContent = acc.agent_mode || "auto";
    tdModel.appendChild(modelSpan);
    tr.appendChild(tdModel);

    const tdQuota = document.createElement("td");
    tdQuota.style.fontSize = "0.85rem";
    const quota = getQuotaStats(acc);
    if (quota && quota.unknown) {
      tdQuota.style.color = "#64748b";
      tdQuota.innerHTML = `<span>未知</span> <span style="color:#64748b;font-size:0.75rem">(Puter 暂无稳定额度接口)</span>`;
    } else if (quota) {
      const pct = quota.pctRemaining;
      const color = pct <= 10 ? "#fb7185" : pct <= 30 ? "#f59e0b" : "#34d399";
      if (normalizeAccountType(acc) === "warp" && quota.splitBonus) {
        tdQuota.innerHTML = `<span style="color:${color}">${quota.remaining.toLocaleString()}</span> <span style="color:#64748b;font-size:0.75rem">(剩余)</span><div style="color:#64748b;font-size:0.75rem">${quota.monthlyRemaining.toLocaleString()} 月度 + ${quota.bonusRemaining.toLocaleString()} 赠送</div>`;
      } else {
        tdQuota.innerHTML = `<span style="color:${color}">${quota.remaining.toLocaleString()} / ${quota.limit.toLocaleString()}</span> <span style="color:#64748b;font-size:0.75rem">(剩余)</span>`;
      }
    } else {
      tdQuota.style.color = "#64748b";
      tdQuota.textContent = "-";
    }
    tr.appendChild(tdQuota);

    const tdStatus = document.createElement("td");
    const statusWrap = document.createElement("div");
    statusWrap.style.display = "flex";
    statusWrap.style.alignItems = "center";
    statusWrap.style.gap = "6px";

    const statusSpan = document.createElement("span");
    statusSpan.className = "tag tag-status-normal";
    statusSpan.title = badge.tip || "";
    statusSpan.style.background = badge.bg;
    statusSpan.style.color = badge.color;
    statusSpan.style.border = "none";
    statusSpan.textContent = badge.text;
    statusWrap.appendChild(statusSpan);

    if (normalizeAccountType(acc) === "grok" && acc.nsfw_enabled === true) {
      const nsfwSpan = document.createElement("span");
      nsfwSpan.className = "tag";
      nsfwSpan.title = "已开启 NSFW";
      nsfwSpan.style.background = "rgba(239, 68, 68, 0.16)";
      nsfwSpan.style.color = "#fda4af";
      nsfwSpan.style.border = "none";
      nsfwSpan.textContent = "NSFW";
      statusWrap.appendChild(nsfwSpan);
    }

    tdStatus.appendChild(statusWrap);
    tr.appendChild(tdStatus);

    const tdCount = document.createElement("td");
    tdCount.style.fontSize = "0.9rem";
    tdCount.style.color = "#e2e8f0";
    tdCount.style.fontWeight = "500";
    tdCount.textContent = String(acc.request_count || 0);
    tr.appendChild(tdCount);

    const tdLast = document.createElement("td");
    tdLast.style.fontSize = "0.8rem";
    tdLast.style.color = "#64748b";
    tdLast.textContent = acc.last_used_at && !acc.last_used_at.startsWith('0001') ? formatTime(acc.last_used_at) : "-";
    tr.appendChild(tdLast);

    const tdActions = document.createElement("td");
    tdActions.style.textAlign = "right";
    const actionWrap = document.createElement("div");
    actionWrap.style.display = "flex";
    actionWrap.style.justifyContent = "flex-end";
    actionWrap.style.gap = "12px";

    const edit = document.createElement("i");
    edit.className = "action-icon";
    edit.dataset.action = "edit";
    edit.dataset.id = encodeData(acc.id);
    edit.title = "编辑";
    edit.textContent = "✏️";

    const refresh = document.createElement("i");
    refresh.className = "action-icon";
    refresh.dataset.action = "refresh";
    refresh.dataset.id = encodeData(acc.id);
    refresh.title = "刷新";
    refresh.textContent = "🔄";

    const del = document.createElement("i");
    del.className = "action-icon";
    del.dataset.action = "delete";
    del.dataset.id = encodeData(acc.id);
    del.title = "删除";
    del.textContent = "🗑️";

    actionWrap.appendChild(edit);
    actionWrap.appendChild(refresh);
    actionWrap.appendChild(del);
    tdActions.appendChild(actionWrap);
    tr.appendChild(tdActions);

    // 将行添加到 fragment 而不是直接添加到 tbody
    fragment.appendChild(tr);
  });
  
  // 一次性将所有行插入到 tbody
  tbody.appendChild(fragment);
  table.appendChild(tbody);
  wrap.appendChild(table);
  container.appendChild(wrap);

  const paginationInfo = domCache.paginationInfo || document.getElementById("paginationInfo");
  paginationInfo.textContent = `共 ${total} 条记录，第 ${currentPage}/${totalPages} 页`;
  renderPagination(currentPage, totalPages);
  updateSelectedCount();

  container.onclick = (e) => {
    const actionEl = e.target.closest("[data-action]");
    if (!actionEl || !container.contains(actionEl)) return;
    const action = actionEl.dataset.action;
    const idRaw = actionEl.dataset.id || "";
    const id = parseDataId(idRaw);
    if (action === "edit") editAccount(id);
    if (action === "refresh") refreshToken(id);
    if (action === "delete") deleteAccount(id);
  };

  container.onchange = (e) => {
    const target = e.target;
    if (!(target instanceof HTMLInputElement)) return;
    const action = target.dataset.action;
    if (action === "row-select") {
      updateSelectedCount();
      return;
    }
    if (action === "select-all") {
      toggleSelectAll(target.checked);
    }
  };
}

function renderPagination(current, total) {
  const container = domCache.paginationControls || document.getElementById("paginationControls");
  if (!container) return;

  container.innerHTML = "";
  const appendButton = (label, page, disabled, activeClass, extraStyle) => {
    const btn = document.createElement("button");
    btn.className = `btn ${activeClass}`.trim();
    btn.dataset.page = String(page);
    btn.disabled = disabled;
    btn.textContent = label;
    btn.style.padding = "4px 10px";
    if (extraStyle) {
      Object.keys(extraStyle).forEach((key) => {
        btn.style[key] = extraStyle[key];
      });
    }
    container.appendChild(btn);
  };

  // First & Prev
  appendButton("首页", 1, current === 1, "btn-outline");
  appendButton("上一页", current - 1, current === 1, "btn-outline");

  // Page Numbers (simplified logic: show surrounding)
  let startPage = Math.max(1, current - 2);
  let endPage = Math.min(total, startPage + 4);
  if (endPage - startPage < 4) {
    startPage = Math.max(1, endPage - 4);
  }

  for (let i = startPage; i <= endPage; i++) {
    const activeClass = i === current ? 'btn-primary' : 'btn-outline';
    appendButton(String(i), i, false, activeClass, { minWidth: "32px", justifyContent: "center" });
  }

  // Next & Last
  appendButton("下一页", current + 1, current === total, "btn-outline");
  appendButton("末页", total, current === total, "btn-outline");
  container.onclick = (e) => {
    const btn = e.target.closest("button[data-page]");
    if (!btn || !container.contains(btn) || btn.disabled) return;
    const page = parseInt(btn.dataset.page, 10);
    if (!Number.isNaN(page)) goToPage(page);
  };
}

function goToPage(page) {
  if (page < 1) return;
  // We can't check 'total' easily here without storing it or querying DOM
  // But renderAccounts will clamp it.
  currentPage = page;
  renderAccounts();
}

// Filter by platform
function filterByPlatform(platform) {
  currentPlatform = platform;
  currentPage = 1; // Reset to first page
  document.querySelectorAll("#platformFilters .tab-item").forEach(btn => {
    btn.classList.toggle("active", btn.textContent === platform);
  });
  const subtitle = document.getElementById("pageSubtitle");
  if (subtitle) {
    subtitle.textContent = currentPlatform ? `管理您的 ${currentPlatform} API 凭证` : "管理您的所有 API 凭证";
  }
  renderAccounts();
}

// Update page size
function updatePageSize(size) {
  pageSize = parseInt(size);
  currentPage = 1;
  renderAccounts();
}

// Update statistics
function updateStats() {
  const total = accounts.length;
  const abnormal = accounts.filter(isAccountAbnormal).length;
  const normal = Math.max(0, total - abnormal);

  document.getElementById("totalAccounts").textContent = total;
  document.getElementById("enabledAccounts").textContent = normal;
  document.getElementById("disabledAccounts").textContent = abnormal;

  // Attempt to update selected if element exists (it should)
  updateSelectedCount();

  // Update sidebar footer
  const footerTotal = document.getElementById("footerTotal");
  if (footerTotal) footerTotal.textContent = total;

  const footerNormal = document.getElementById("footerNormal");
  if (footerNormal) footerNormal.textContent = normal;

  const footerAbnormal = document.getElementById("footerAbnormal");
  if (footerAbnormal) footerAbnormal.textContent = abnormal;
}

// Update selected count
function updateSelectedCount() {
  const checked = document.querySelectorAll(".row-checkbox:checked").length;
  const el = document.getElementById("selectedCount");
  if (el) el.textContent = checked;
  const batchBtn = document.getElementById("batchDeleteBtn");
  if (batchBtn) {
    batchBtn.disabled = checked === 0;
    batchBtn.style.color = checked === 0 ? "#94a3b8" : "#fb7185";
    batchBtn.style.borderColor = checked === 0 ? "rgba(148,163,184,0.2)" : "rgba(251,113,133,0.45)";
  }
}

// Toggle select all
function toggleSelectAll(checked) {
  document.querySelectorAll(".row-checkbox").forEach(cb => cb.checked = checked);
  updateSelectedCount();
}

// Open modal
function openModal(account = null) {
  const modal = document.getElementById("accountModal");
  const title = document.getElementById("modalTitle");
  const form = document.getElementById("accountForm");
  const typeEl = document.getElementById("accountType");

  const finalizeModal = () => {
    applyTokenLabels(typeEl ? typeEl.value : "orchids");
    modal.classList.add("active");
    modal.style.display = "flex";
  };

  const applyValues = () => {
    if (account) {
      title.textContent = "编辑账号";
      document.getElementById("accountId").value = account.id;
      document.getElementById("accountType").value = normalizeAccountType(account);
      document.getElementById("clientCookie").value = getAccountToken(account);
      document.getElementById("projectId").value = account.project_id || "";
      document.getElementById("enabled").checked = account.enabled;
    } else {
      title.textContent = "添加账号";
      form.reset();
      document.getElementById("accountId").value = "";
      document.getElementById("accountType").value = "orchids";
      document.getElementById("enabled").checked = true;
      document.getElementById("clientCookie").value = "";
      document.getElementById("projectId").value = "";
    }
  };

  loadModelCatalog()
    .finally(() => {
      applyValues();
      finalizeModal();
    });
}

// Close modal
function closeModal() {
  const modal = document.getElementById("accountModal");
  modal.classList.remove("active");
  modal.style.display = "none";
}

// Save account
async function saveAccount(e) {
  e.preventDefault();
  const id = document.getElementById("accountId").value;
  const type = document.getElementById("accountType").value;
  const token = document.getElementById("clientCookie").value;
  const projectId = String(document.getElementById("projectId")?.value || "").trim();
  const credentials = splitBatchCredentialInput(token);
  const existing = id ? accounts.find((a) => String(a.id) === String(id)) : null;
  const data = {
    account_type: type,
    agent_mode: resolveAgentMode(type, existing ? existing.agent_mode : ""),
    weight: existing ? (parseInt(existing.weight, 10) || 1) : 1,
    enabled: document.getElementById("enabled").checked,
  };

  if (credentials.length === 0) {
    showToast("请填写至少一个账号凭证", "error");
    return;
  }
  if (type === "bolt" && !projectId) {
    showToast("请填写 Bolt project_id", "error");
    return;
  }

  try {
    if (id) {
      const payload = { ...data };
      if (type === 'warp') {
        payload.refresh_token = credentials[0];
      } else if (type === 'bolt') {
        payload.session_cookie = credentials[0];
        payload.project_id = projectId;
      } else if (type === 'puter' || type === 'v0') {
        payload.client_cookie = credentials[0];
      } else {
        payload.client_cookie = credentials[0];
      }
      const res = await fetch(`/api/accounts/${id}`, {
        method: "PUT",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(payload),
      });
      if (!res.ok) throw new Error(await res.text());
      closeModal();
      loadAccounts();
      showToast("保存成功");
      return;
    }

    if (credentials.length > 1) {
      let success = 0;
      let failed = 0;
      for (const item of credentials) {
        const payload = { ...data };
        if (type === 'warp') {
          payload.refresh_token = item;
        } else if (type === 'bolt') {
          payload.session_cookie = item;
          payload.project_id = projectId;
        } else if (type === 'puter' || type === 'v0') {
          payload.client_cookie = item;
        } else {
          payload.client_cookie = item;
        }
        const res = await fetch("/api/accounts", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify(payload),
        });
        if (res.ok) {
          success += 1;
        } else {
          failed += 1;
        }
      }
      closeModal();
      loadAccounts();
      showToast(`批量添加完成：成功 ${success}，失败 ${failed}`);
      return;
    }

    const payload = { ...data };
    if (type === 'warp') {
      payload.refresh_token = credentials[0];
    } else if (type === 'bolt') {
      payload.session_cookie = credentials[0];
      payload.project_id = projectId;
    } else if (type === 'puter' || type === 'v0') {
      payload.client_cookie = credentials[0];
    } else {
      payload.client_cookie = credentials[0];
    }
    const res = await fetch("/api/accounts", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(payload),
    });
    if (!res.ok) throw new Error(await res.text());
    closeModal();
    loadAccounts();
    showToast("保存成功");
  } catch (err) {
    showToast("保存失败: " + err.message, "error");
  }
}

// Edit account
function editAccount(id) {
  const account = accounts.find((a) => a.id === id);
  if (account) openModal(account);
}

// Refresh token
async function refreshToken(id) {
  const actionText = "刷新";
  showToast(`正在${actionText}账号信息...`, "info");
  await checkAccount(id, false, actionText);
}

// Delete account
async function deleteAccount(id) {
  if (!confirm("确定要删除这个账号吗？")) return;
  try {
    const res = await fetch(`/api/accounts/${id}`, { method: "DELETE" });
    if (!res.ok) throw new Error(await res.text());
    showToast("删除成功");
    loadAccounts();
  } catch (err) {
    showToast("删除失败: " + err.message, "error");
  }
}

// Escape HTML
function escapeHtml(text) {
  if (text === null || text === undefined) return '';
  const div = document.createElement("div");
  div.textContent = String(text);
  return div.innerHTML;
}

function encodeData(value) {
  return encodeURIComponent(value === null || value === undefined ? "" : String(value));
}

function decodeData(value) {
  if (!value) return "";
  try {
    return decodeURIComponent(value);
  } catch (err) {
    return value;
  }
}

function parseDataId(value) {
  const decoded = decodeData(value);
  if (decoded === "") return "";
  const num = Number(decoded);
  return Number.isNaN(num) ? decoded : num;
}

function formatTokenDisplay(acc) {
  const type = normalizeAccountType(acc);
  const token = acc.token;
  if (token) {
    if (token.length > 30) {
      if (type === 'warp') {
        // Warp tokens (JWTs) have long common prefixes, so show more of the end
        return token.substring(0, 10) + '...' + token.substring(token.length - 10);
      } else if (type === 'grok') {
        return token.substring(0, 8) + '...' + token.substring(token.length - 8);
      }
      return token.substring(0, 30) + '...';
    }
    return token;
  }
  if (type === 'grok' && getAccountToken(acc)) {
    const sso = getAccountToken(acc);
    return sso.length > 20 ? sso.substring(0, 8) + '...' + sso.substring(sso.length - 8) : sso;
  }
  if (type === 'warp' && getAccountToken(acc)) {
    const rt = getAccountToken(acc);
    return rt.length > 30 ? rt.substring(0, 10) + '...' + rt.substring(rt.length - 10) : rt;
  }
  if (type === 'bolt' && getAccountToken(acc)) {
    const session = getAccountToken(acc);
    return session.length > 24 ? session.substring(0, 8) + '...' + session.substring(session.length - 8) : session;
  }
  if (type === 'puter' && getAccountToken(acc)) {
    const token = getAccountToken(acc);
    return token.length > 24 ? token.substring(0, 8) + '...' + token.substring(token.length - 8) : token;
  }
  if (type === 'v0' && getAccountToken(acc)) {
    const token = getAccountToken(acc);
    return token.length > 24 ? token.substring(0, 8) + '...' + token.substring(token.length - 8) : token;
  }
  if (acc.session_id) {
    return acc.session_id.substring(0, 30) + '...';
  }
  return '-';
}

// Format time
function formatTime(iso) {
  const d = new Date(iso);
  const now = new Date();
  const diff = (now - d) / 1000;
  if (diff < 60) return "刚刚";
  if (diff < 3600) return Math.floor(diff / 60) + " 分钟前";
  if (diff < 86400) return Math.floor(diff / 3600) + " 小时前";
  return d.toLocaleDateString();
}

// Export accounts
function exportAccounts() {
  window.location.href = "/api/export";
}

// Import accounts
async function importAccounts(event) {
  const file = event.target.files[0];
  if (!file) return;
  try {
    const text = await file.text();
    const res = await fetch("/api/import", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: text,
    });
    const result = await res.json();
    showToast(`导入完成: 成功 ${result.imported}, 跳过 ${result.skipped}`);
    loadAccounts();
  } catch (err) {
    showToast("导入失败: " + err.message, "error");
  }
  event.target.value = "";
}

// Load accounts on page load
document.addEventListener('DOMContentLoaded', () => {
  initDOMCache();
  loadAccounts();
  const typeSelect = document.getElementById("accountType");
  if (typeSelect) {
    typeSelect.addEventListener("change", () => {
      applyTokenLabels(typeSelect.value);
    });
    applyTokenLabels(typeSelect.value);
  }
});
