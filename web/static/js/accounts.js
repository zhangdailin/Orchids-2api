// Accounts management JavaScript

let accounts = [];
let currentPlatform = '';
let accountHealth = {};
let pageSize = 20;
let currentPage = 1;

// DOM 缓存
const domCache = {
    accountsList: null,
    paginationInfo: null,
    paginationControls: null,
    accountImportStatus: null,
};

function initDOMCache() {
    domCache.accountsList = document.getElementById("accountsList");
    domCache.paginationInfo = document.getElementById("paginationInfo");
    domCache.paginationControls = document.getElementById("paginationControls");
    domCache.accountImportStatus = document.getElementById("accountImportStatus");
}

// Load accounts from API
async function loadAccounts() {
  try {
    const res = await fetch("/api/accounts");
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
  return normalizeSidebarAccountType(acc);
}

function getQuotaStats(acc) {
  if (!acc) return null;
  const base = getSidebarQuotaStats(acc);
  if (!base) return null;
  if (base.unknown) return base;
  const type = normalizeAccountType(acc);
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
  const limit = Math.max(0, Math.floor(base.limit || 0));
  const remaining = Math.max(0, Math.floor(base.remaining || 0));
  const used = Math.max(0, limit - remaining);
  const pctRemaining = limit > 0 ? Math.min(100, Math.round((remaining / limit) * 100)) : 0;
  return { ...base, limit, remaining, used, pctRemaining };
}

function getAccountToken(acc) {
  return getSidebarAccountToken(acc);
}

function normalizeAccountSubscription(acc) {
  const raw = String(acc?.subscription || "").trim().toLowerCase();
  if (!raw) return "";
  if (normalizeAccountType(acc) === "warp") {
    if (raw.includes("enterprise") || raw.includes("unlimited")) return "enterprise";
    if (raw.includes("max")) return "max";
    if (raw.includes("business")) return "build/business";
    if (raw.includes("build")) return "build/business";
    if (raw.includes("free")) return "free";
    if (raw.includes("unknown")) return "unknown";
    return raw;
  }
  if (raw.includes("heavy")) return "heavy";
  if (raw.includes("super") || raw.includes("pro")) return "super";
  if (raw.includes("lite")) return "lite";
  if (raw.includes("basic") || raw.includes("free")) return "basic";
  return raw;
}

function subscriptionBadge(acc) {
  const type = normalizeAccountType(acc);
  const level = normalizeAccountSubscription(acc);
  if (!level) {
    return { text: "-", bg: "rgba(100, 116, 139, 0.12)", color: "#94a3b8", tip: "暂无订阅等级" };
  }
  if (type === "warp") {
    switch (level) {
      case "enterprise":
        return { text: "Enterprise", bg: "rgba(251, 191, 36, 0.16)", color: "#fbbf24", tip: "Warp Enterprise / Unlimited 额度档" };
      case "max":
        return { text: "Max", bg: "rgba(56, 189, 248, 0.16)", color: "#38bdf8", tip: "Warp Max 额度档" };
      case "build/business":
        return { text: "Build/Business", bg: "rgba(167, 139, 250, 0.16)", color: "#c4b5fd", tip: "Warp 1,500 credits/月，Build 与 Business 额度相同" };
      case "free":
        return { text: "Free", bg: "rgba(52, 211, 153, 0.14)", color: "#34d399", tip: "Warp Free 额度档" };
      case "unknown":
        return { text: "Unknown", bg: "rgba(100, 116, 139, 0.12)", color: "#94a3b8", tip: "暂未识别 Warp 额度档" };
      default:
        return { text: level, bg: "rgba(100, 116, 139, 0.12)", color: "#cbd5e1", tip: `Warp 额度档: ${level}` };
    }
  }
  if (type !== "grok") {
    return { text: level, bg: "rgba(100, 116, 139, 0.12)", color: "#cbd5e1", tip: `订阅等级: ${level}` };
  }
  switch (level) {
    case "heavy":
      return { text: "heavy", bg: "rgba(251, 191, 36, 0.16)", color: "#fbbf24", tip: "Grok Heavy 账号池" };
    case "super":
      return { text: "super", bg: "rgba(56, 189, 248, 0.16)", color: "#38bdf8", tip: "Grok Super 账号池" };
    case "lite":
      return { text: "lite", bg: "rgba(167, 139, 250, 0.16)", color: "#c4b5fd", tip: "Grok Lite 账号池" };
    case "basic":
      return { text: "basic", bg: "rgba(52, 211, 153, 0.14)", color: "#34d399", tip: "Grok Basic 账号池" };
    default:
      return { text: level, bg: "rgba(100, 116, 139, 0.12)", color: "#cbd5e1", tip: `Grok 账号池: ${level}` };
  }
}

function buildSubscriptionMarkup(acc) {
  const badge = subscriptionBadge(acc);
  return `<span class="tag account-tier-tag" title="${escapeHtml(badge.tip || "")}" style="background:${badge.bg};color:${badge.color};border:none;">${escapeHtml(badge.text)}</span>`;
}

function shouldShowNSFWBadge(acc) {
  return normalizeAccountType(acc) === "grok" && !!acc?.nsfw_enabled;
}

function buildNSFWBadgeMarkup(acc) {
  if (!shouldShowNSFWBadge(acc)) return "";
  return `<span class="tag account-nsfw-tag" title="Grok NSFW 已开启" style="background:rgba(244, 114, 182, 0.14);color:#f472b6;border:none;">NSFW</span>`;
}

function applyTokenLabels(type) {
  const label = document.getElementById("tokenLabel");
  const input = document.getElementById("clientCookie");
  const hint = document.getElementById("tokenHint");
  const warpImportActions = document.getElementById("warpLocalImportActions");
  const accountId = String(document.getElementById("accountId")?.value || "");
  if (!label || !input || !hint) return;
  if (warpImportActions) {
    warpImportActions.hidden = type !== "warp";
  }
  if (type === 'warp') {
    label.textContent = "Warp Auth";
    input.placeholder = "每行一个 id_token.refresh_token、登录回跳 URL 或 User JSON";
    hint.textContent = accountId
      ? "编辑时仅保存第一行；可粘贴 warp://auth/... 回跳 URL / User JSON / id_token.refresh_token"
      : "支持批量添加 Warp。可粘贴 warp://auth/... 回跳 URL / User JSON / id_token.refresh_token";
    input.required = true;
  } else if (type === 'grok') {
    label.textContent = "SSO Token";
    input.placeholder = "每行一个 sso token（或包含 sso= 的 Cookie）";
    hint.textContent = accountId
      ? "编辑时仅保存第一行 SSO Token"
      : "支持批量添加 Grok。每行一个 sso token 或 Cookie 片段";
  } else if (type === 'puter') {
      label.textContent = "Auth Token";
      input.placeholder = "每行一个 Puter auth_token";
      hint.textContent = accountId
        ? "Puter 编辑时仅保存第一行 auth_token。可前往 https://docs.puter.com/playground/ai-chatgpt/ 获取"
        : "支持批量添加 Puter。每行一个 auth_token；可前往 https://docs.puter.com/playground/ai-chatgpt/ 获取";
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

function selectWarpUserFile() {
  const input = document.getElementById("warpUserFileInput");
  if (!input) return;
  input.value = "";
  input.click();
}

async function importWarpUserFile(file) {
  if (!file) return;
  const typeEl = document.getElementById("accountType");
  if (String(typeEl?.value || "").toLowerCase() !== "warp") return;

  try {
    renderAccountImportStatus("正在上传并解析 WARP User JSON / token...", "info", [file.name]);
    const form = new FormData();
    form.append("file", file, file.name || "dev.warp.Warp-User");
    const res = await fetch("/api/warp/import-user-file", {
      method: "POST",
      body: form,
    });
    if (!res.ok) throw new Error((await res.text()).trim() || "上传导入失败");
    const account = await res.json();
    renderAccountImportStatus("已解析并保存 Warp 账号", "info", [`账号 #${account.id || ""}`.trim()]);
    showToast("已保存上传的 WARP 账号");
    closeModal();
    loadAccounts();
  } catch (err) {
    renderAccountImportStatus("上传 User JSON / token 失败", "error", [err.message || String(err)]);
    showToast("上传导入失败: " + (err.message || String(err)), "error");
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

function normalizeCredentialForType(type, credential) {
  const normalizedType = String(type || "").trim().toLowerCase();
  const raw = String(credential || "").trim();
  if (!raw) return "";

  if (normalizedType === "warp") {
    try {
      const parsed = JSON.parse(raw);
      const token = findNestedWarpRefreshToken(parsed);
      if (token) return token;
    } catch (_) {
      // Not JSON; continue with URL/cookie/form extraction.
    }
    const match = raw.match(/(?:^|[?&;\s])refresh_token=([^&;\s]+)/i);
    return (match ? decodeURIComponent(match[1]) : raw).trim();
  }

  if (normalizedType === "grok") {
    const ssoMatch = raw.match(/(?:^|[;\s])sso=([^;\s]+)/i);
    return (ssoMatch ? ssoMatch[1] : raw).trim();
  }

  return raw;
}

function findNestedWarpRefreshToken(value) {
  if (!value || typeof value !== "object") return "";
  const preferred = ["id_token", "auth_tokens", "authTokens"];
  for (const key of preferred) {
    if (value[key]) {
      const token = findNestedWarpRefreshToken(value[key]);
      if (token) return token;
    }
  }
  for (const [key, item] of Object.entries(value)) {
    const normalizedKey = String(key || "").toLowerCase();
    if (normalizedKey === "refresh_token" || normalizedKey === "refreshtoken") {
      const token = String(item || "").trim();
      if (token) return token;
    }
    const token = findNestedWarpRefreshToken(item);
    if (token) return token;
  }
  return "";
}

function buildCredentialFingerprint(type, credential) {
  const normalizedType = String(type || "").trim().toLowerCase();
  const normalizedCredential = normalizeCredentialForType(normalizedType, credential);
  if (!normalizedType || !normalizedCredential) return "";
  return `${normalizedType}:${normalizedCredential}`;
}

function collectExistingCredentialFingerprints(type, excludeId = "") {
  const normalizedType = String(type || "").trim().toLowerCase();
  const excluded = String(excludeId || "").trim();
  const seen = new Set();
  (Array.isArray(accounts) ? accounts : []).forEach((acc) => {
    if (!acc) return;
    if (String(acc.id || "") === excluded) return;
    if (normalizeAccountType(acc) !== normalizedType) return;
    const token = getAccountToken(acc);
    const key = buildCredentialFingerprint(normalizedType, token);
    if (key) seen.add(key);
  });
  return seen;
}

function dedupeCredentialInputs(type, credentials) {
  const unique = [];
  const duplicates = [];
  const seen = new Set();

  (Array.isArray(credentials) ? credentials : []).forEach((credential) => {
    const trimmed = String(credential || "").trim();
    if (!trimmed) return;
    const key = buildCredentialFingerprint(type, trimmed) || `raw:${trimmed}`;
    if (seen.has(key)) {
      duplicates.push(trimmed);
      return;
    }
    seen.add(key);
    unique.push(trimmed);
  });

  return { unique, duplicates };
}

function filterExistingCredentialConflicts(type, credentials, excludeId = "") {
  const existing = collectExistingCredentialFingerprints(type, excludeId);
  const accepted = [];
  const conflicts = [];

  (Array.isArray(credentials) ? credentials : []).forEach((credential) => {
    const trimmed = String(credential || "").trim();
    if (!trimmed) return;
    const key = buildCredentialFingerprint(type, trimmed);
    if (key && existing.has(key)) {
      conflicts.push(trimmed);
      return;
    }
    accepted.push(trimmed);
  });

  return { accepted, conflicts };
}

function getAccountImportStatusNode() {
  return domCache.accountImportStatus || document.getElementById("accountImportStatus");
}

function escapeImportStatusText(text) {
  return String(text || "")
    .replaceAll("&", "&amp;")
    .replaceAll("<", "&lt;")
    .replaceAll(">", "&gt;");
}

function clearAccountImportStatus() {
  const node = getAccountImportStatusNode();
  if (!node) return;
  node.hidden = true;
  node.classList.remove("is-active", "is-error");
  node.innerHTML = "";
}

function renderAccountImportStatus(message, type = "info", details = []) {
  const node = getAccountImportStatusNode();
  if (!node) return;

  const safeMessage = escapeImportStatusText(message);
  const rows = Array.isArray(details) ? details.filter(Boolean).slice(0, 8) : [];
  const detailHTML = rows.length > 0
    ? `<div style="margin-top:8px">${rows.map((item) => `<div><code>${escapeImportStatusText(item)}</code></div>`).join("")}</div>`
    : "";

  node.hidden = false;
  node.classList.toggle("is-active", type === "info");
  node.classList.toggle("is-error", type === "error");
  node.innerHTML = `<strong>${safeMessage}</strong>${detailHTML}`;
}

function buildAccountPayload(type, baseData, credential) {
  const payload = { ...baseData };
  if (type === "warp") {
    payload.refresh_token = credential;
  } else {
    payload.client_cookie = credential;
  }
  return payload;
}

function accountTypeLabel(type) {
  switch (String(type || "").trim().toLowerCase()) {
    case "warp":
      return "Warp";
    case "puter":
      return "Puter";
    case "grok":
      return "Grok";
    case "orchids":
    default:
      return "Orchids";
  }
}

function getActiveAccountType() {
  const platform = String(currentPlatform || "").trim().toLowerCase();
  return platform || "orchids";
}

function setAccountModalType(type) {
  const normalized = String(type || "orchids").trim().toLowerCase() || "orchids";
  const typeEl = document.getElementById("accountType");
  const displayEl = document.getElementById("accountTypeDisplay");
  if (typeEl) typeEl.value = normalized;
  if (displayEl) displayEl.value = accountTypeLabel(normalized);
  applyTokenLabels(normalized);
}

async function createAccount(payload) {
  const res = await fetch("/api/accounts", {
    method: "POST",
    headers: {
      "Content-Type": "application/json",
      "X-Account-Sync": "async",
    },
    body: JSON.stringify(payload),
  });
  if (!res.ok) {
    throw new Error(await res.text());
  }
  return res.json();
}

function summarizeAccountCreateError(err) {
  const message = String(err && err.message ? err.message : err || "").trim();
  if (!message) return "未知错误";
  const compact = message.replace(/\s+/g, " ");
  return compact.length > 160 ? `${compact.slice(0, 157)}...` : compact;
}

async function runAccountCreatePool(payloads, concurrency = 6, onProgress = null) {
  let nextIndex = 0;
  let success = 0;
  let failed = 0;
  let completed = 0;
  const failures = [];
  const size = Math.max(1, Math.min(concurrency, payloads.length || 1));

  async function worker() {
    while (nextIndex < payloads.length) {
      const currentIndex = nextIndex;
      nextIndex += 1;
      const payload = payloads[currentIndex];
      try {
        await createAccount(payload);
        success += 1;
      } catch (err) {
        failed += 1;
        failures.push(`#${currentIndex + 1} ${summarizeAccountCreateError(err)}`);
        console.error("Failed to create account:", err);
      } finally {
        completed += 1;
        if (typeof onProgress === "function") {
          onProgress({
            total: payloads.length,
            completed,
            success,
            failed,
            currentIndex,
            payload,
            failures,
          });
        }
      }
    }
  }

  await Promise.all(Array.from({ length: size }, () => worker()));
  return { success, failed, failures };
}

// Render platform filter tabs
function renderPlatformTabs() {
  const container = document.getElementById("platformFilters");
  if (!container) return;
  const defaultTypes = ["orchids", "warp", "puter", "grok"];
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

function evaluateAccountStatus(acc) {
  const health = accountHealth[acc.id];
  if (health && !health.ok) {
    return { normal: false, text: '异常', color: '#fb7185', bg: 'rgba(251, 113, 133, 0.16)', tip: health.msg || '状态同步失败' };
  }
  if (!acc.enabled) {
    return { normal: false, text: '禁用', color: '#fb7185', bg: 'rgba(251, 113, 133, 0.16)', tip: '账号已禁用' };
  }
  const statusCode = normalizeSidebarStatusCode(acc.status_code);
  if (isQuotaOnlyStatus(acc)) {
    const quota = getQuotaStats(acc);
    const limitText = quota && quota.limit > 0 ? quota.limit.toLocaleString() : '未知';
    const type = normalizeAccountType(acc);
    const providerName = type === 'warp' ? 'Warp' : 'Puter';
    return {
      normal: true,
      text: '额度不足',
      color: '#f59e0b',
      bg: 'rgba(245, 158, 11, 0.16)',
      tip: providerName + ' 额度已用尽或余额不足，调度器会暂时跳过该账号 (剩余 0 / ' + limitText + ')',
      quotaOnly: true,
    };
  }
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
  } else if (type === 'puter') {
    if (!getAccountToken(acc)) {
      return { normal: false, text: '待补全', color: '#f59e0b', bg: 'rgba(245, 158, 11, 0.16)', tip: '缺少 Puter auth_token' };
    }
  } else if (!acc.session_id && !acc.session_cookie) {
    return { normal: false, text: '待补全', color: '#f59e0b', bg: 'rgba(245, 158, 11, 0.16)', tip: '缺少会话信息' };
  }

  const quota = getQuotaStats(acc);
  if (quota && quota.limit > 0 && quota.remaining <= 0) {
    if (normalizeAccountType(acc) === 'puter' || normalizeAccountType(acc) === 'warp') {
      const providerName = normalizeAccountType(acc) === 'warp' ? 'Warp' : 'Puter';
      return {
        normal: true,
        text: '额度不足',
        color: '#f59e0b',
        bg: 'rgba(245, 158, 11, 0.16)',
        tip: providerName + ' 额度已用尽或余额不足，调度器会暂时跳过该账号 (剩余 0 / ' + quota.limit.toLocaleString() + ')',
        quotaOnly: true,
      };
    }
    return { normal: false, text: '配额已满', color: '#fb7185', bg: 'rgba(251, 113, 133, 0.16)', tip: '配额已用尽 (剩余 0 / ' + quota.limit.toLocaleString() + ')' };
  }

  return { normal: true, text: '正常', color: '#34d399', bg: 'rgba(52, 211, 153, 0.16)', tip: '状态正常' };
}

function isAccountAbnormal(acc) {
  return !evaluateAccountStatus(acc).normal;
}

function matchesCurrentPlatform(acc) {
  if (!currentPlatform) return true;
  const key = String(currentPlatform || "").toLowerCase();
  return normalizeAccountType(acc).includes(key);
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
  const abnormal = accounts.filter((acc) => matchesCurrentPlatform(acc) && isAccountAbnormal(acc));
  if (abnormal.length === 0) {
    showToast(currentPlatform ? `当前 ${currentPlatform} 页面没有异常账号` : "没有异常账号", "info");
    return;
  }
  const scopeText = currentPlatform ? `当前 ${currentPlatform} 页面中的 ` : "";
  if (confirm(`确定要清空 ${scopeText}${abnormal.length} 个异常账号吗？`)) {
    for (const acc of abnormal) {
      await fetch(`/api/accounts/${acc.id}`, { method: "DELETE" });
    }
    loadAccounts();
    showToast(`已清空${scopeText}异常账号`);
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
  const filtered = accounts.filter(matchesCurrentPlatform);

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
    empty.className = "empty-state empty-state-panel";
    const icon = document.createElement("span");
    icon.className = "empty-state-mark";
    icon.textContent = "EMPTY";
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

  if (window.matchMedia("(max-width: 640px)").matches) {
    renderAccountsMobile(container, pageItems, total, totalPages);
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
    { label: "等级", style: "width: 90px;" },
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

    const tdTier = document.createElement("td");
    tdTier.innerHTML = buildSubscriptionMarkup(acc);
    tr.appendChild(tdTier);

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

    if (shouldShowNSFWBadge(acc)) {
      const nsfwSpan = document.createElement("span");
      nsfwSpan.className = "tag account-nsfw-tag";
      nsfwSpan.title = "Grok NSFW 已开启";
      nsfwSpan.style.background = "rgba(244, 114, 182, 0.14)";
      nsfwSpan.style.color = "#f472b6";
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
    edit.textContent = "Edit";

    const refresh = document.createElement("i");
    refresh.className = "action-icon";
    refresh.dataset.action = "refresh";
    refresh.dataset.id = encodeData(acc.id);
    refresh.title = "刷新";
    refresh.textContent = "Sync";

    const del = document.createElement("i");
    del.className = "action-icon";
    del.dataset.action = "delete";
    del.dataset.id = encodeData(acc.id);
    del.title = "删除";
    del.textContent = "Del";

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

function buildQuotaMarkup(acc) {
  const quota = getQuotaStats(acc);
  if (quota && quota.unknown) {
    return `<span>未知</span> <span style="color:#64748b;font-size:0.75rem">(Puter 暂无稳定额度接口)</span>`;
  }
  if (quota) {
    const pct = quota.pctRemaining;
    const color = pct <= 10 ? "#fb7185" : pct <= 30 ? "#f59e0b" : "#34d399";
    if (normalizeAccountType(acc) === "warp" && quota.splitBonus) {
      return `<span style="color:${color}">${quota.remaining.toLocaleString()}</span> <span style="color:#64748b;font-size:0.75rem">(剩余)</span><div style="color:#64748b;font-size:0.75rem">${quota.monthlyRemaining.toLocaleString()} 月度 + ${quota.bonusRemaining.toLocaleString()} 赠送</div>`;
    }
    return `<span style="color:${color}">${quota.remaining.toLocaleString()} / ${quota.limit.toLocaleString()}</span> <span style="color:#64748b;font-size:0.75rem">(剩余)</span>`;
  }
  return `<span style="color:#64748b">-</span>`;
}

function buildStatusMarkup(acc, badge) {
  return `<span class="tag" title="${escapeHtml(badge.tip || "")}" style="background:${badge.bg};color:${badge.color};border:none;">${escapeHtml(badge.text)}</span>${buildNSFWBadgeMarkup(acc)}`;
}

function renderAccountsMobile(container, pageItems, total, totalPages) {
  container.innerHTML = "";
  const list = document.createElement("div");
  list.className = "accounts-mobile-list";

  const fragment = document.createDocumentFragment();
  pageItems.forEach((acc) => {
    const badge = statusBadge(acc);
    const tokenDisplay = formatTokenDisplay(acc);
    const card = document.createElement("article");
    card.className = "account-mobile-card";
    card.innerHTML = `
      <div class="account-mobile-head">
        <label class="account-mobile-check">
          <input type="checkbox" class="row-checkbox" data-action="row-select" data-id="${encodeData(acc.id)}">
          <span>#${escapeHtml(acc.id === null || acc.id === undefined ? "" : String(acc.id))}</span>
        </label>
        <div class="account-mobile-actions">
          <button type="button" class="action-icon" data-action="edit" data-id="${encodeData(acc.id)}" title="编辑">Edit</button>
          <button type="button" class="action-icon" data-action="refresh" data-id="${encodeData(acc.id)}" title="刷新">Sync</button>
          <button type="button" class="action-icon" data-action="delete" data-id="${encodeData(acc.id)}" title="删除">Del</button>
        </div>
      </div>
      <div class="account-mobile-token">
        <span class="token-text" title="${escapeHtml(tokenDisplay)}">${escapeHtml(tokenDisplay)}</span>
      </div>
      <div class="account-mobile-grid">
        <div class="account-mobile-item">
          <span class="account-mobile-label">状态</span>
          <div class="account-mobile-inline">${buildStatusMarkup(acc, badge)}</div>
        </div>
        <div class="account-mobile-item">
          <span class="account-mobile-label">等级</span>
          <div class="account-mobile-inline">${buildSubscriptionMarkup(acc)}</div>
        </div>
        <div class="account-mobile-item">
          <span class="account-mobile-label">配额</span>
          <div class="account-mobile-value">${buildQuotaMarkup(acc)}</div>
        </div>
        <div class="account-mobile-item">
          <span class="account-mobile-label">调用</span>
          <span class="account-mobile-value">${escapeHtml(String(acc.request_count || 0))}</span>
        </div>
        <div class="account-mobile-item">
          <span class="account-mobile-label">最后调用</span>
          <span class="account-mobile-value">${escapeHtml(acc.last_used_at && !acc.last_used_at.startsWith("0001") ? formatTime(acc.last_used_at) : "-")}</span>
        </div>
      </div>
    `;
    fragment.appendChild(card);
  });

  list.appendChild(fragment);
  container.appendChild(list);

  const paginationInfo = domCache.paginationInfo || document.getElementById("paginationInfo");
  paginationInfo.textContent = `共 ${total} 条记录，第 ${currentPage}/${totalPages} 页`;
  renderPagination(currentPage, totalPages);
  updateSelectedCount();

  container.onclick = (e) => {
    const actionEl = e.target.closest("[data-action]");
    if (!actionEl || !container.contains(actionEl)) return;
    const action = actionEl.dataset.action;
    const id = parseDataId(actionEl.dataset.id || "");
    if (action === "edit") editAccount(id);
    if (action === "refresh") refreshToken(id);
    if (action === "delete") deleteAccount(id);
  };

  container.onchange = (e) => {
    const target = e.target;
    if (!(target instanceof HTMLInputElement)) return;
    if (target.dataset.action === "row-select") {
      updateSelectedCount();
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
  clearAccountImportStatus();

  const finalizeModal = () => {
    applyTokenLabels(typeEl ? typeEl.value : getActiveAccountType());
    modal.classList.add("active");
    modal.style.display = "flex";
  };

  const applyValues = () => {
    if (account) {
      title.textContent = "编辑账号";
      document.getElementById("accountId").value = account.id;
      setAccountModalType(normalizeAccountType(account));
      document.getElementById("clientCookie").value = getAccountToken(account);
      document.getElementById("enabled").checked = account.enabled;
    } else {
      title.textContent = "添加账号";
      form.reset();
      document.getElementById("accountId").value = "";
      setAccountModalType(getActiveAccountType());
      document.getElementById("enabled").checked = true;
      document.getElementById("clientCookie").value = "";
    }
  };

  applyValues();
  finalizeModal();
}

// Close modal
function closeModal() {
  const modal = document.getElementById("accountModal");
  modal.classList.remove("active");
  modal.style.display = "none";
  clearAccountImportStatus();
}

// Save account
async function saveAccount(e) {
  e.preventDefault();
  const id = document.getElementById("accountId").value;
  const type = document.getElementById("accountType").value;
  const token = document.getElementById("clientCookie").value;
  const splitCredentials = splitBatchCredentialInput(token);
  const { unique: dedupedCredentials, duplicates: duplicateInputs } = dedupeCredentialInputs(type, splitCredentials);
  const { accepted: credentials, conflicts: existingConflicts } = filterExistingCredentialConflicts(type, dedupedCredentials, id);
  const existing = id ? accounts.find((a) => String(a.id) === String(id)) : null;
  const data = {
    account_type: type,
    weight: existing ? (parseInt(existing.weight, 10) || 1) : 1,
    enabled: document.getElementById("enabled").checked,
  };

  if (credentials.length === 0) {
    if (duplicateInputs.length > 0 || existingConflicts.length > 0) {
      const details = []
        .concat(duplicateInputs.slice(0, 4).map((item) => `输入重复: ${item}`))
        .concat(existingConflicts.slice(0, 4).map((item) => `已存在: ${item}`));
      renderAccountImportStatus("没有可添加的新凭证，重复项已全部过滤", "error", details);
      showToast("没有可添加的新凭证，重复项已全部过滤", "error");
    } else {
      showToast("请填写至少一个账号凭证", "error");
    }
    return;
  }
  try {
    clearAccountImportStatus();
    if (duplicateInputs.length > 0 || existingConflicts.length > 0) {
      const details = []
        .concat(duplicateInputs.slice(0, 4).map((item) => `输入重复: ${item}`))
        .concat(existingConflicts.slice(0, 4).map((item) => `账号已存在: ${item}`));
      renderAccountImportStatus(
        `已过滤重复凭证：输入重复 ${duplicateInputs.length}，库内重复 ${existingConflicts.length}`,
        "info",
        details,
      );
    }
    if (id) {
      const payload = buildAccountPayload(type, data, credentials[0]);
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
      const payloads = credentials.map((item) => buildAccountPayload(type, data, item));
      renderAccountImportStatus(`正在批量添加账号 0/${payloads.length}`, "info");
      const { success, failed, failures } = await runAccountCreatePool(payloads, 6, (progress) => {
        renderAccountImportStatus(
          `正在批量添加账号 ${progress.completed}/${progress.total}，成功 ${progress.success}，失败 ${progress.failed}`,
          progress.failed > 0 ? "error" : "info",
          progress.failures,
        );
      });
      if (failed > 0) {
        renderAccountImportStatus(`批量添加完成：成功 ${success}，失败 ${failed}`, "error", failures);
      } else {
        renderAccountImportStatus(`批量添加完成：成功 ${success}，失败 ${failed}`, "info");
      }
      loadAccounts();
      if (failed === 0) {
        closeModal();
      }
      showToast(
        failed > 0 ? `批量添加完成：成功 ${success}，失败 ${failed}` : `批量添加完成：成功 ${success}`,
        failed > 0 ? "error" : "success",
      );
      return;
    }

    await createAccount(buildAccountPayload(type, data, credentials[0]));
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
  if (type === 'puter' && getAccountToken(acc)) {
    const token = getAccountToken(acc);
    return token.length > 24 ? token.substring(0, 8) + '...' + token.substring(token.length - 8) : token;
  }
  if (acc.session_id) {
    return acc.session_id.substring(0, 30) + '...';
  }
  return '-';
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
    applyTokenLabels(typeSelect.value);
  }
  const warpUserFileInput = document.getElementById("warpUserFileInput");
  if (warpUserFileInput) {
    warpUserFileInput.addEventListener("change", () => {
      const file = warpUserFileInput.files && warpUserFileInput.files[0];
      importWarpUserFile(file);
    });
  }
});
