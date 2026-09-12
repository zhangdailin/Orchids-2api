// Accounts management JavaScript

let warpDeviceLoginId = "";
let warpDeviceLoginTimer = null;
let grokDeviceLoginId = "";
let grokDeviceLoginTimer = null;

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
    const loadedAccounts = await res.json();
    accounts = (Array.isArray(loadedAccounts) ? loadedAccounts : []).filter((account) => !(
      String(account?.account_type || "").trim().toLowerCase() === "grok" &&
      String(account?.grok_provider || "").trim().toLowerCase() === "console" &&
      Number(account?.grok_sso_parent_id || 0) > 0
    ));
    sortAccounts();
    renderPlatformTabs();
    renderAccounts();
    updateStats();
    // Fire-and-forget: the table renders immediately, refreshed rows stream in.
    autoSyncStaleAccounts();
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
  const type = normalizeAccountType(acc);
  if (type === "workbuddy") {
    const base = getSidebarQuotaStats(acc);
    if (!base) {
      return { supported: false, unknown: true, limit: 0, remaining: 0, used: 0, pctRemaining: 0 };
    }
    const limit = Math.max(0, base.limit || 0);
    const remaining = Math.max(0, base.remaining || 0);
    const used = Math.max(0, limit - remaining);
    const pctRemaining = limit > 0 ? Math.min(100, Math.round((remaining / limit) * 100)) : 0;
    return {
      ...base,
      limit,
      remaining,
      used,
      pctRemaining,
      workbuddy: true,
      unit: base.unit || "credits",
      resetAt: base.resetAt || "",
      packageRemaining: base.packageRemaining || 0,
    };
  }
  // Build billing and response throttling are different xAI products. Never
  // use request/token rate-limit headers as a paid-plan balance.
  if (type === "grok" && isSidebarGrokOAuthAccount(acc)) {
    const weekly = acc.grok_billing && acc.grok_billing.weekly;
    if (weekly && weekly.has_usage === true && Number.isFinite(Number(weekly.usage_percent))) {
      const used = Math.max(0, Math.min(100, Number(weekly.usage_percent)));
      return {
        supported: true,
        limit: 100,
        remaining: Math.max(0, 100 - used),
        used,
        pctRemaining: Math.max(0, 100 - used),
        weeklyPercent: true,
        resetAt: weekly.reset_at || "",
      };
    }
    return { supported: false, limit: 0, remaining: 0, used: 0, pctRemaining: 0, quotaUnavailable: true };
  }
  const base = getSidebarQuotaStats(acc);
  if (!base) return null;
  if (base.unknown) return base;
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
  if (raw.includes("xpremiumplus") || raw.includes("x_premium_plus")) return "x_premium_plus";
  if (raw.includes("xpremium") || raw.includes("x_premium")) return "x_premium";
  if (raw.includes("xbasic") || raw.includes("x_basic")) return "x_basic";
  if (raw.includes("supergrok")) return "supergrok";
  if (raw.includes("super") || raw.includes("pro")) return "super";
  if (raw.includes("lite")) return "lite";
  if (raw.includes("basic") || raw.includes("free")) return "basic";
  return raw;
}

function subscriptionBadge(acc) {
  const type = normalizeAccountType(acc);
  if (type === "workbuddy") {
    const plan = String(acc?.quota_plan || "").trim();
    if (plan) {
      return {
        text: plan,
        bg: "rgba(52, 211, 153, 0.16)",
        color: "#34d399",
        tip: `WorkBuddy 计量包: ${plan}${acc?.quota_unit ? `（单位 ${acc.quota_unit}）` : ""}`,
      };
    }
    // No meter snapshot yet: say so instead of showing a made-up level.
    return {
      text: "未同步",
      bg: "rgba(100, 116, 139, 0.12)",
      color: "#94a3b8",
      tip: "尚未读取到 WorkBuddy 计量包；点 Sync 刷新账号状态后重试",
    };
  }
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
    case "unknown":
      return { text: "未知", bg: "rgba(100, 116, 139, 0.12)", color: "#94a3b8", tip: "xAI 未返回可验证的 Grok 套餐等级" };
    case "x_premium_plus":
      return { text: "X Premium+", bg: "rgba(251, 191, 36, 0.16)", color: "#fbbf24", tip: "xAI 官方 X Premium+ 套餐" };
    case "x_premium":
      return { text: "X Premium", bg: "rgba(56, 189, 248, 0.16)", color: "#38bdf8", tip: "xAI 官方 X Premium 套餐" };
    case "x_basic":
      return { text: "X Basic", bg: "rgba(167, 139, 250, 0.16)", color: "#c4b5fd", tip: "xAI 官方 X Basic 套餐" };
    case "supergrok":
      return { text: "SuperGrok", bg: "rgba(251, 191, 36, 0.16)", color: "#fbbf24", tip: "xAI 官方 SuperGrok 套餐" };
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
  const accountId = String(document.getElementById("accountId")?.value || "");
  const puterWebLoginGroup = document.getElementById("puterWebLoginGroup");
  if (puterWebLoginGroup) puterWebLoginGroup.hidden = type !== "puter" || Boolean(accountId);
  // WorkBuddy login stays available while editing so an expired authorization can
  // be renewed by signing in again instead of deleting the account.
  const workbuddyLoginGroup = document.getElementById("workbuddyLoginGroup");
  if (workbuddyLoginGroup) workbuddyLoginGroup.hidden = type !== "workbuddy";
  const warpDeviceLoginGroup = document.getElementById("warpDeviceLoginGroup");
  if (warpDeviceLoginGroup) {
    warpDeviceLoginGroup.hidden = type !== "warp" || Boolean(accountId);
  }
  const saveButton = document.querySelector('#accountForm button[type="submit"]');
  if (saveButton) {
    // Warp and WorkBuddy are created by their official login flows, so the form
    // has nothing to submit for a new account of either type.
    saveButton.hidden = (type === "warp" || type === "workbuddy") && !accountId;
  }
  applyCredentialModeUI(type);
  if (!label || !input || !hint) return;
  if (type === 'warp') {
    input.value = "";
    input.required = false;
  } else if (type === 'workbuddy') {
    // OAuth-only channel: no manual credential field is exposed.
    input.value = "";
    input.required = false;
    label.textContent = "WorkBuddy 凭证";
    input.placeholder = "";
    hint.textContent = "该渠道只支持官方登录";
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
      ? "支持直接粘贴 "
      : "支持原始 __client、完整 Cookie Header 或 Cookie JSON；推荐同时带上 __client_uat 以提高补全成功率";
    input.required = true;
  }
}

function getWarpDeviceLoginStatusNode() {
  return document.getElementById("warpDeviceLoginStatus");
}

function getGrokDeviceLoginStatusNode() {
  return document.getElementById("grokDeviceLoginStatus");
}

function renderGrokDeviceLoginStatus(message, type = "info", html = "") {
  const node = getGrokDeviceLoginStatusNode();
  if (!node) return;
  node.hidden = false;
  node.classList.toggle("is-active", type === "info");
  node.classList.toggle("is-error", type === "error");
  node.innerHTML = `<strong>${escapeImportStatusText(message)}</strong>${html}`;
}

function resetGrokDeviceLoginStatus() {
  const node = getGrokDeviceLoginStatusNode();
  if (node) {
    node.hidden = true;
    node.classList.remove("is-active", "is-error");
    node.innerHTML = "";
  }
}

function stopGrokDeviceLogin(cancel = false) {
  if (grokDeviceLoginTimer) {
    clearTimeout(grokDeviceLoginTimer);
    grokDeviceLoginTimer = null;
  }
  const id = grokDeviceLoginId;
  grokDeviceLoginId = "";
  const button = document.getElementById("grokDeviceLoginButton");
  if (button) button.disabled = false;
  if (cancel && id) {
    fetch(`/api/grok/device-auth/${encodeURIComponent(id)}`, { method: "DELETE" }).catch(() => {});
  }
}

async function startGrokDeviceLogin() {
  if (grokDeviceLoginId) return;
  const button = document.getElementById("grokDeviceLoginButton");
  if (button) button.disabled = true;
  resetGrokDeviceLoginStatus();
  try {
    const res = await fetch("/api/grok/device-auth", { method: "POST" });
    if (!res.ok) throw new Error(await res.text());
    const login = await res.json();
    grokDeviceLoginId = String(login.id || "");
    if (!grokDeviceLoginId || login.status !== "pending") {
      throw new Error("Grok 登录初始化响应无效");
    }
    const link = String(login.verification_uri_complete || login.verification_uri || "");
    let safeLink = "";
    try {
      const parsed = new URL(link);
      if (parsed.protocol === "https:" && (parsed.hostname === "auth.x.ai" || parsed.hostname === "accounts.x.ai")) {
        safeLink = parsed.href;
      }
    } catch (_) {
      // Keep an unexpected upstream URL from becoming an open redirect.
    }
    const linkHTML = safeLink
      ? `<div style="margin-top:8px"><a href="${escapeImportStatusText(safeLink)}" target="_blank" rel="noopener noreferrer">打开 Grok 官方授权页面</a></div>`
      : "";
    renderGrokDeviceLoginStatus(`请在 Grok 官方页面输入设备码：${login.user_code || ""}`, "info", linkHTML);
    if (safeLink) window.open(safeLink, "_blank", "noopener");
    pollGrokDeviceLogin();
  } catch (err) {
    stopGrokDeviceLogin(false);
    renderGrokDeviceLoginStatus("无法启动 Grok 官方登录：" + (err.message || String(err)), "error");
  }
}

async function pollGrokDeviceLogin() {
  const id = grokDeviceLoginId;
  if (!id) return;
  try {
    const res = await fetch(`/api/grok/device-auth/${encodeURIComponent(id)}`);
    if (!res.ok) throw new Error(await res.text());
    const login = await res.json();
    if (id !== grokDeviceLoginId) return;
    if (login.status === "pending") {
      grokDeviceLoginTimer = setTimeout(pollGrokDeviceLogin, 1500);
      return;
    }
    stopGrokDeviceLogin(false);
    if (login.status === "complete") {
      renderGrokDeviceLoginStatus(login.message || "Grok 账号已添加", "info");
      showToast(login.message || "Grok 账号已添加", "success");
      loadAccounts();
      setTimeout(closeModal, 800);
      return;
    }
    renderGrokDeviceLoginStatus(login.message || "Grok 官方登录未完成", "error");
  } catch (err) {
    if (id !== grokDeviceLoginId) return;
    stopGrokDeviceLogin(false);
    renderGrokDeviceLoginStatus("Grok 登录状态查询失败：" + (err.message || String(err)), "error");
  }
}

function renderWarpDeviceLoginStatus(message, type = "info", html = "") {
  const node = getWarpDeviceLoginStatusNode();
  if (!node) return;
  node.hidden = false;
  node.classList.toggle("is-active", type === "info");
  node.classList.toggle("is-error", type === "error");
  node.innerHTML = `<strong>${escapeImportStatusText(message)}</strong>${html}`;
}

function resetWarpDeviceLoginStatus() {
  const node = getWarpDeviceLoginStatusNode();
  if (node) {
    node.hidden = true;
    node.classList.remove("is-active", "is-error");
    node.innerHTML = "";
  }
}

function stopWarpDeviceLogin(cancel = false) {
  if (warpDeviceLoginTimer) {
    clearTimeout(warpDeviceLoginTimer);
    warpDeviceLoginTimer = null;
  }
  const id = warpDeviceLoginId;
  warpDeviceLoginId = "";
  const button = document.getElementById("warpDeviceLoginButton");
  if (button) button.disabled = false;
  if (cancel && id) {
    fetch(`/api/warp/device-auth/${encodeURIComponent(id)}`, { method: "DELETE" }).catch(() => {});
  }
}

async function startWarpDeviceLogin() {
  if (warpDeviceLoginId) return;
  const button = document.getElementById("warpDeviceLoginButton");
  if (button) button.disabled = true;
  resetWarpDeviceLoginStatus();
  try {
    const res = await fetch("/api/warp/device-auth", { method: "POST" });
    if (!res.ok) throw new Error(await res.text());
    const login = await res.json();
    warpDeviceLoginId = String(login.id || "");
    if (!warpDeviceLoginId || login.status !== "pending") {
      throw new Error("Warp 登录初始化响应无效");
    }
    const link = String(login.verification_uri_complete || login.verification_uri || "");
    let safeLink = "";
    try {
      const parsed = new URL(link);
      if (parsed.origin === "https://app.warp.dev") safeLink = parsed.href;
    } catch (_) {
      // The backend only returns Warp's official URL; keep the UI safe if an
      // unexpected upstream response is ever received.
    }
    const linkHTML = safeLink
      ? `<div style="margin-top:8px"><a href="${escapeImportStatusText(safeLink)}" target="_blank" rel="noopener noreferrer">打开 Warp 官方授权页面</a></div>`
      : "";
    renderWarpDeviceLoginStatus(`请在 Warp 官方页面输入设备码：${login.user_code || ""}`, "info", linkHTML);
    if (safeLink) window.open(safeLink, "_blank", "noopener");
    pollWarpDeviceLogin();
  } catch (err) {
    stopWarpDeviceLogin(false);
    renderWarpDeviceLoginStatus("无法启动 Warp 官方登录：" + (err.message || String(err)), "error");
  }
}

async function pollWarpDeviceLogin() {
  const id = warpDeviceLoginId;
  if (!id) return;
  try {
    const res = await fetch(`/api/warp/device-auth/${encodeURIComponent(id)}`);
    if (!res.ok) throw new Error(await res.text());
    const login = await res.json();
    if (id !== warpDeviceLoginId) return;
    if (login.status === "pending") {
      warpDeviceLoginTimer = setTimeout(pollWarpDeviceLogin, 1500);
      return;
    }
    stopWarpDeviceLogin(false);
    if (login.status === "complete") {
      renderWarpDeviceLoginStatus(login.message || "Warp 账号已添加", "info");
      showToast(login.message || "Warp 账号已添加", "success");
      loadAccounts();
      setTimeout(closeModal, 800);
      return;
    }
    renderWarpDeviceLoginStatus(login.message || "Warp 官方登录未完成", "error");
  } catch (err) {
    if (id !== warpDeviceLoginId) return;
    stopWarpDeviceLogin(false);
    renderWarpDeviceLoginStatus("Warp 登录状态查询失败：" + (err.message || String(err)), "error");
  }
}

// Grok credential mode: SSO cookie vs Build CLI OAuth.
//
// SSO is created by pasting the cookie; the internal Console companion is
// plumbing the operator never chooses, so its picker is never exposed.
// Build CLI OAuth is only ever obtained through the official xAI device login:
// the access/refresh tokens are redacted server-side on read, so manual token
// inputs would be unusable anyway.
function applyCredentialModeUI(type) {
  const modeGroup = document.getElementById("credentialModeGroup");
  const modeSelect = document.getElementById("credentialType");
  if (!modeGroup || !modeSelect) return;
  const isGrok = String(type || "").trim().toLowerCase() === "grok";
  modeGroup.hidden = !isGrok;
  const mode = String(modeSelect?.value || "sso").trim().toLowerCase();
  const isOAuth = isGrok && mode === "oauth";
  // The credential textarea is hidden for the channels that only accept official
  // login (Warp) and for WorkBuddy, which is OAuth-only by product decision.
  const showToken = type !== "warp" && type !== "workbuddy" && !isOAuth;
  const providerGroup = document.getElementById("grokProviderGroup");
  if (providerGroup) providerGroup.hidden = true;
  document.getElementById("ssoCredentialGroup").hidden = !showToken;
  // No manual OAuth credential inputs: the device login owns them.
  const oauthCredentialGroup = document.getElementById("oauthCredentialGroup");
  if (oauthCredentialGroup) oauthCredentialGroup.hidden = true;
  const oauthRefreshGroup = document.getElementById("oauthRefreshGroup");
  if (oauthRefreshGroup) oauthRefreshGroup.hidden = true;
  const oauthExpiresGroup = document.getElementById("oauthExpiresGroup");
  if (oauthExpiresGroup) oauthExpiresGroup.hidden = true;
  const grokDeviceLoginGroup = document.getElementById("grokDeviceLoginGroup");
  // Kept in edit mode too: re-authorizing an account whose grant the upstream
  // retired (the 未授权 case) must not require deleting and re-adding it.
  if (grokDeviceLoginGroup) grokDeviceLoginGroup.hidden = !isOAuth;
  const clientCookie = document.getElementById("clientCookie");
  if (clientCookie) clientCookie.required = showToken;
}

function currentCredentialMode() {
  const modeSelect = document.getElementById("credentialType");
  return String(modeSelect?.value || "sso").trim().toLowerCase();
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

  if (normalizedType === "grok") {
    const ssoMatch = raw.match(/(?:^|[;\s])sso=([^;\s]+)/i);
    return (ssoMatch ? ssoMatch[1] : raw).trim();
  }

  return raw;
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
  if (type === "grok" && String(baseData.credential_type || "").toLowerCase() === "oauth") {
    // OAuth fields already carried in baseData; do not write a client_cookie.
    delete payload.client_cookie;
    return payload;
  }
  if (type === "warp") {
    // Warp edits contain settings only; credentials belong to web login.
    delete payload.refresh_token;
    delete payload.client_cookie;
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
    case "workbuddy":
      return "WorkBuddy";
    default:
      return "Warp";
  }
}

function getActiveAccountType() {
  const platform = String(currentPlatform || "").trim().toLowerCase();
  return platform || "warp";
}

// selectedPlatformAccountType resolves the account type for a brand-new account.
//
// The active platform tab is the source of truth: it is what the operator is
// looking at when they press 添加账号. The hidden field is only a fallback for the
// cold-load case (no tab rendered yet) — reading it first let a stale value from
// a previous modal interaction (or the HTML default "warp") silently win, which
// made the Grok tab open a Warp form.
function selectedPlatformAccountType(typeEl) {
  const active = String(currentPlatform || "").trim().toLowerCase();
  if (active) return platformAccountType(active);
  const selected = String(typeEl?.value || "").trim().toLowerCase();
  return selected || getActiveAccountType();
}

// platformAccountType maps a platform tab key to the account type it creates.
// Tab keys are account types (the tab list is built from `accounts[].account_type`),
// so an unrecognised value must not silently create a Warp account.
function platformAccountType(platform) {
  const key = String(platform || "").trim().toLowerCase();
  switch (key) {
    case "warp":
    case "grok":
    case "puter":
    case "workbuddy":
      return key;
    default:
      return getActiveAccountType();
  }
}

function setAccountModalType(type) {
  const normalized = String(type || "warp").trim().toLowerCase() || "warp";
  const typeEl = document.getElementById("accountType");
  const displayEl = document.getElementById("accountTypeDisplay");
  if (typeEl) typeEl.value = normalized;
  if (displayEl) displayEl.value = accountTypeLabel(normalized);
  applyTokenLabels(normalized);
}

// extractAdminErrorDetail turns an admin API error body into the sentence an
// operator should read. Create/update endpoints answer with the standard
// {"error":{"type":...,"message":...}} envelope, and showing that JSON verbatim
// hides the actionable part ("upstream rejected this credential") behind braces.
function extractAdminErrorDetail(raw) {
  const text = String(raw == null ? "" : raw).trim();
  if (!text) return "";
  try {
    const parsed = JSON.parse(text);
    const message = parsed && typeof parsed === "object"
      ? (parsed.error && typeof parsed.error === "object" ? parsed.error.message : parsed.error)
      : null;
    const detail = typeof message === "string" ? message.trim() : "";
    if (detail) return detail;
  } catch (_) {
    /* a plain-text error body is already the detail */
  }
  return text;
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
    throw new Error(extractAdminErrorDetail(await res.text()));
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
  const defaultTypes = ["warp", "puter", "workbuddy", "grok"];
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
  // A bare "401" or "429" hides the actionable cause (retired grant vs. a
  // partial write vs. upstream throttling), so the server's reason wins.
  const statusReason = String(acc.status_message || "").trim();
  if (isQuotaOnlyStatus(acc)) {
    const quota = getQuotaStats(acc);
    const limitText = quota && quota.limit > 0 ? quota.limit.toLocaleString() : '未知';
    const type = normalizeAccountType(acc);
    const providerName = accountTypeLabel(type);
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
    // When the server recorded a reason, it is the actionable text; the generic
    // per-code wording is only the fallback.
    const withReason = (fallback) => statusReason || fallback;
    switch (statusCode) {
      case '429':
        return { normal: false, text: '限流', color: '#f59e0b', bg: 'rgba(245, 158, 11, 0.16)', tip: withReason('请求过于频繁 (429)') };
      case '401':
        return { normal: false, text: '未授权', color: '#fb7185', bg: 'rgba(251, 113, 133, 0.16)', tip: withReason('认证失败 (401)') };
      case '403':
        return { normal: false, text: '禁止访问', color: '#fb7185', bg: 'rgba(251, 113, 133, 0.16)', tip: withReason('访问被拒绝 (403)') };
      case '404':
        return { normal: false, text: '不存在', color: '#fb7185', bg: 'rgba(251, 113, 133, 0.16)', tip: withReason('资源不存在 (404)') };
      default:
        return { normal: false, text: '异常', color: '#fb7185', bg: 'rgba(251, 113, 133, 0.16)', tip: withReason('状态异常: ' + statusCode) };
    }
  }

  const type = normalizeAccountType(acc);
  if (type === 'warp') {
    if (!hasSidebarAccountCredential(acc)) {
      return { normal: false, text: '待登录', color: '#f59e0b', bg: 'rgba(245, 158, 11, 0.16)', tip: '请使用 Warp 官方网页登录' };
    }
  } else if (type === 'grok') {
    // OAuth secrets are redacted by the account list API. credential_type is
    // the safe indicator that the server holds a Build OAuth credential.
    if (!isSidebarGrokOAuthAccount(acc) && !getAccountToken(acc)) {
      return { normal: false, text: '待补全', color: '#f59e0b', bg: 'rgba(245, 158, 11, 0.16)', tip: '缺少 SSO Token' };
    }
  } else if (type === 'puter') {
    if (!getAccountToken(acc)) {
      return { normal: false, text: '待补全', color: '#f59e0b', bg: 'rgba(245, 158, 11, 0.16)', tip: '缺少 Puter auth_token' };
    }
  } else if (type === 'workbuddy') {
    if (!getAccountToken(acc)) {
      return { normal: false, text: '待补全', color: '#f59e0b', bg: 'rgba(245, 158, 11, 0.16)', tip: '缺少 WorkBuddy 凭证（refreshToken / accessToken）' };
    }
  } else if (!acc.session_id && !acc.session_cookie) {
    return { normal: false, text: '待补全', color: '#f59e0b', bg: 'rgba(245, 158, 11, 0.16)', tip: '缺少会话信息' };
  }

  const quota = getQuotaStats(acc);
  if (quota && quota.limit > 0 && quota.remaining <= 0) {
    if (normalizeAccountType(acc) === 'puter' || normalizeAccountType(acc) === 'warp') {
      const providerName = accountTypeLabel(normalizeAccountType(acc));
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

// Refresh single account via the shared check endpoint. Returns true when the
// sync succeeded, so callers (auto-sync in particular) can tell a refreshed row
// from a failed attempt.
async function checkAccount(id, silent = false, actionText = "刷新") {
  const action = "check";
  let succeeded = false;
  try {
    const res = await fetch(`/api/accounts/${id}/${action}`);
    if (!res.ok) {
      throw new Error(await res.text());
    }
    const updated = await res.json();
    accounts = accounts.map(a => (a.id === id ? updated : a));
    updateAccountHealth(id, true);
    succeeded = true;
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
  return succeeded;
}

// Refresh-on-load: the account table is only as fresh as the last sync, and most
// channels do NOT report when their quota snapshot was taken (Warp and Puter have
// no timestamp at all). A page-session ledger plus a persisted one therefore
// drives auto-sync, with the channel's own snapshot timestamp used when present.
const ACCOUNT_SYNC_MAX_AGE_MS = 30 * 60 * 1000;
const ACCOUNT_SYNC_LEDGER_KEY = 'orchids_account_sync_v1';
const ACCOUNT_AUTO_SYNC_PACE_MS = 200;

function parseAccountTime(value) {
  const parsed = Date.parse(String(value || ""));
  return Number.isFinite(parsed) ? parsed : 0;
}

// accountSyncTimestamp resolves when this account's displayed numbers were last
// obtained from the upstream. 0 means "this channel does not report one".
function accountSyncTimestamp(acc) {
  if (!acc) return 0;
  const type = normalizeAccountType(acc);
  if (type === "workbuddy") {
    return parseAccountTime(acc?.workbuddy_quota?.synced_at) || parseAccountTime(acc?.workbuddy_models_synced_at);
  }
  if (type === "grok") {
    if (isSidebarGrokOAuthAccount(acc)) {
      // Build OAuth: billing windows carry the weekly/monthly allowance the
      // 配额 column renders.
      return parseAccountTime(acc?.grok_billing?.synced_at) || parseAccountTime(acc?.grok_models_synced_at);
    }
    return parseAccountTime(acc?.grok_web_quota?.synced_at) || parseAccountTime(acc?.grok_models_synced_at);
  }
  return 0;
}

// The ledger survives reloads so a channel without timestamps is refreshed at
// most once per ACCOUNT_SYNC_MAX_AGE_MS instead of on every page view.
const accountSyncLedger = {
  entries: {},
  loaded: false,
  load() {
    if (this.loaded) return;
    this.loaded = true;
    try {
      const raw = window.localStorage?.getItem(ACCOUNT_SYNC_LEDGER_KEY);
      const parsed = raw ? JSON.parse(raw) : null;
      if (parsed && typeof parsed === "object") this.entries = parsed;
    } catch (_) {
      this.entries = {};
    }
  },
  get(id) {
    this.load();
    const value = Number(this.entries[String(id)]);
    return Number.isFinite(value) ? value : 0;
  },
  set(id, at) {
    this.load();
    this.entries[String(id)] = at;
    try {
      window.localStorage?.setItem(ACCOUNT_SYNC_LEDGER_KEY, JSON.stringify(this.entries));
    } catch (_) {
      /* storage may be unavailable; the in-memory copy still applies */
    }
  },
};

const accountAutoSyncState = { attemptedThisLoad: false, inFlight: new Set() };

function accountLastSyncAt(acc) {
  if (!acc) return 0;
  return Math.max(accountSyncTimestamp(acc), accountSyncLedger.get(acc.id));
}

function shouldAutoSyncAccount(acc) {
  if (!acc || !acc.enabled || !acc.id) return false;
  if (accountAutoSyncState.inFlight.has(String(acc.id))) return false;
  // Warp credentials are never submitted by the UI, but a loaded account with no
  // settings snapshot still needs one official sync.
  if (normalizeAccountType(acc) === "warp" && acc.token) return false;

  const lastSync = accountLastSyncAt(acc);
  if (!lastSync) return true;
  return Date.now() - lastSync >= ACCOUNT_SYNC_MAX_AGE_MS;
}

function sleepForPace(ms) {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

// autoSyncStaleAccounts refreshes, at most once per page load, every enabled
// account whose last successful sync is older than the TTL. Sequential and paced
// on purpose: a channel may rate limit per account pool, and the table
// re-renders as results arrive.
async function autoSyncStaleAccounts() {
  if (accountAutoSyncState.attemptedThisLoad) return;
  accountAutoSyncState.attemptedThisLoad = true;
  let synced = 0;
  for (const acc of accounts) {
    if (!shouldAutoSyncAccount(acc)) continue;
    const id = String(acc.id);
    accountAutoSyncState.inFlight.add(id);
    const ok = await checkAccount(acc.id, true);
    accountAutoSyncState.inFlight.delete(id);
    if (ok) {
      accountSyncLedger.set(id, Date.now());
      synced += 1;
    }
    await sleepForPace(ACCOUNT_AUTO_SYNC_PACE_MS);
  }
  return synced;
}

// resetAutoSyncLoadGuard simulates a fresh page load for the per-load guard.
function resetAutoSyncLoadGuard() {
  accountAutoSyncState.attemptedThisLoad = false;
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
    { label: "账号 / 邮箱" },
    { label: "等级", style: "width: 130px;" },
    { label: "配额", style: "width: 150px;" },
    { label: "状态" },
    { label: "调用" },
    { label: "最后调用" },
    { label: "操作", style: "text-align: right;" },
  ];
  headers.forEach((h, idx) => {
    const th = document.createElement("th");
    if (h.style) th.style.cssText = h.style;
    if (h.label === "账号 / 邮箱") th.classList.add("col-token");
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
    // One shared renderer for the desktop table and the mobile cards.
    tdQuota.innerHTML = buildQuotaMarkup(acc);
    const quota = getQuotaStats(acc);
    if (normalizeAccountType(acc) === "workbuddy") {
      if (quota && quota.workbuddy) {
        tdQuota.title = [
          quota.plan ? `计量包: ${quota.plan}` : "",
          `单位: ${quota.unit || "credit"}`,
          "口径: 当前周期剩余 / 周期上限",
          quota.resetAt ? `重置: ${new Date(quota.resetAt).toLocaleString()}` : "",
        ].filter(Boolean).join(" · ");
      } else {
        tdQuota.title = "尚未读取到 WorkBuddy 计量额度；点 Sync 立即刷新";
      }
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
    tdCount.textContent = String(accountUsageCounter(acc));
    if (normalizeAccountType(acc) === "workbuddy") {
      tdCount.title = "WorkBuddy 按计量口径统计的已消耗额度（点 Sync 刷新）";
    }
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

// formatCredit renders meter values that the upstream reports with fractions.
function formatCredit(value) {
  const number = Number(value || 0);
  if (!Number.isFinite(number)) return "0";
  return Number.isInteger(number) ? number.toLocaleString() : number.toFixed(2);
}

// formatQuotaReset renders a reset timestamp as a compact remaining time.
function formatQuotaReset(iso) {
  const resetAt = Date.parse(String(iso || ""));
  if (!Number.isFinite(resetAt)) return "";
  const remaining = resetAt - Date.now();
  if (remaining <= 0) return "待重置";
  const hours = Math.floor(remaining / 3600000);
  if (hours < 24) return `${Math.max(1, hours)} 小时后重置`;
  return `${Math.floor(hours / 24)} 天后重置`;
}

// accountUsageCounter is the value the 调用 column shows. WorkBuddy accounts have
// no request counter of their own, so the credit meter's consumed units are the
// honest equivalent.
function accountUsageCounter(acc) {
  const type = normalizeAccountType(acc);
  if (type === "workbuddy") {
    const consumed = Number(acc?.quota_consumed_units);
    if (Number.isFinite(consumed) && consumed > 0) return consumed;
  }
  return Number(acc?.request_count || 0);
}

// buildMobileEmailMarkup surfaces the signed-in address, which is how operators
// actually recognise a WorkBuddy account (the nickname is the email).
function buildMobileEmailMarkup(acc) {
  const identity = normalizeAccountType(acc) === "workbuddy" ? workBuddyIdentityLabel(acc) : String(acc?.email || "").trim();
  if (!identity || identity === "-") return "";
  const label = normalizeAccountType(acc) === "workbuddy" ? "账号 / 邮箱" : "邮箱";
  return `
        <div class="account-mobile-item" style="grid-column: 1 / -1;">
          <span class="account-mobile-label">${label}</span>
          <span class="account-mobile-value" style="word-break: break-all;">${escapeHtml(identity)}</span>
        </div>`;
}

function buildQuotaMarkup(acc) {
  const quota = getQuotaStats(acc);
  if (quota && quota.quotaUnavailable) {
    return `<span style="color:#94a3b8">未知</span> <span style="color:#64748b;font-size:0.75rem">(xAI 未下发 Build 数值配额)</span>`;
  }
  if (quota && quota.unknown) {
    const hint = normalizeAccountType(acc) === "workbuddy"
      ? "WorkBuddy 计量接口未返回数据"
      : "Puter 暂无稳定额度接口";
    return `<span>未知</span> <span style="color:#64748b;font-size:0.75rem">(${hint})</span>`;
  }
  if (quota && quota.workbuddy) {
    const pct = quota.pctRemaining;
    const color = pct <= 10 ? "#fb7185" : pct <= 30 ? "#f59e0b" : "#34d399";
    const resetText = quota.resetAt ? `<div style="color:#64748b;font-size:0.75rem">${formatQuotaReset(quota.resetAt)}</div>` : "";
    return `<span style="color:${color}">${formatCredit(quota.remaining)} / ${formatCredit(quota.limit)}</span> <span style="color:#64748b;font-size:0.75rem">(剩余)</span>${resetText}`;
  }
  if (quota) {
    const pct = quota.pctRemaining;
    const color = pct <= 10 ? "#fb7185" : pct <= 30 ? "#f59e0b" : "#34d399";
    if (normalizeAccountType(acc) === "warp" && quota.splitBonus) {
      return `<span style="color:${color}">${quota.remaining.toLocaleString()}</span> <span style="color:#64748b;font-size:0.75rem">(剩余)</span><div style="color:#64748b;font-size:0.75rem">${quota.monthlyRemaining.toLocaleString()} 月度 + ${quota.bonusRemaining.toLocaleString()} 赠送</div>`;
    }
	if (quota.weeklyPercent) {
	  return `<span style="color:${color}">${quota.remaining.toLocaleString()}%</span> <span style="color:#64748b;font-size:0.75rem">/ 100% (周度剩余)</span>`;
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
          <span class="account-mobile-value">${escapeHtml(String(accountUsageCounter(acc)))}</span>
        </div>
        <div class="account-mobile-item">
          <span class="account-mobile-label">最后调用</span>
          <span class="account-mobile-value">${escapeHtml(acc.last_used_at && !acc.last_used_at.startsWith("0001") ? formatTime(acc.last_used_at) : "-")}</span>
        </div>
        ${buildMobileEmailMarkup(acc)}
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
  // Keep the modal's account type in step with the selected tab. The hidden
  // field is the only source of truth for "which provider am I adding", and it
  // must never silently fall back to Warp when a platform tab is selected.
  setAccountModalType(platformAccountType(platform));
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
  globalThis.PuterWebLogin?.stop();
  stopWorkBuddyLogin();
  const modal = document.getElementById("accountModal");
  const title = document.getElementById("modalTitle");
  const form = document.getElementById("accountForm");
  const typeEl = document.getElementById("accountType");
  stopWarpDeviceLogin(true);
  resetWarpDeviceLoginStatus();
  stopGrokDeviceLogin(true);
  resetGrokDeviceLoginStatus();
  clearAccountImportStatus();

  const finalizeModal = () => {
    applyTokenLabels(typeEl ? typeEl.value : getActiveAccountType());
    modal.classList.add("active");
    modal.style.display = "flex";
  };

  const applyValues = () => {
    const modeSelect = document.getElementById("credentialType");
    const providerSelect = document.getElementById("grokProvider");
    const providerHint = document.getElementById("grokProviderHint");
    // Every branch below sets the modal type via setAccountModalType; the
    // credential-mode UI must follow THAT type, never the ambient platform tab.
    // (Reading the tab here left a Grok edit without its device-login button when
    // the list was showing another channel.)
    let modalType = "";
    if (account) {
      title.textContent = "编辑账号";
      document.getElementById("accountId").value = account.id;
      modalType = normalizeAccountType(account);
      setAccountModalType(modalType);
      document.getElementById("clientCookie").value = getAccountToken(account);
      document.getElementById("enabled").checked = account.enabled;
      const isOAuth = String(account.credential_type || "").trim().toLowerCase() === "oauth";
      if (modeSelect) modeSelect.value = isOAuth ? "oauth" : "sso";
      if (providerSelect) providerSelect.value = "web";
      if (providerHint) {
        providerHint.textContent = "保存一个 Grok Web SSO 账号时，系统会在内部维护 Console 运行账号。登录凭据和调度设置由 Web 源账号同步，Console 的模型、额度和健康状态保持独立。";
      }
    } else {
      title.textContent = "添加账号";
      form.reset();
      document.getElementById("accountId").value = "";
      // The platform the operator explicitly selected wins over the ambient list
      // default and over any stale hidden field.
      modalType = selectedPlatformAccountType(typeEl);
      setAccountModalType(modalType);
      document.getElementById("enabled").checked = true;
      document.getElementById("clientCookie").value = "";
      if (modeSelect) modeSelect.value = "sso";
      if (providerSelect) providerSelect.value = "web";
      if (providerHint) providerHint.textContent = "保存一个 Grok Web SSO 账号时，系统会在内部维护 Console 运行账号。登录凭据和调度设置由 Web 源账号同步，Console 的模型、额度和健康状态保持独立。";
    }
    applyCredentialModeUI(modalType || normalizeAccountType({ account_type: typeEl?.value || getActiveAccountType() }));
  };

  applyValues();
  finalizeModal();
}

// WorkBuddy official login lifecycle. The login only ever starts from an
// explicit click on "使用 WorkBuddy 官方网页登录" (workbuddy-auth.js owns the
// popup); opening the modal must never navigate the operator anywhere.
function stopWorkBuddyLogin() {
  const login = globalThis.WorkBuddyLogin;
  if (login && typeof login.stop === "function") {
    login.stop();
  }
  const statusNode = document.getElementById("workbuddyLoginStatus");
  if (statusNode) {
    statusNode.hidden = true;
    statusNode.textContent = "";
    if (statusNode.classList) {
      statusNode.classList.remove("is-active", "is-error");
    }
  }
}

// Close modal
function closeModal() {
  globalThis.PuterWebLogin?.stop();
  stopWarpDeviceLogin(true);
  resetWarpDeviceLoginStatus();
  stopGrokDeviceLogin(true);
  resetGrokDeviceLoginStatus();
  stopWorkBuddyLogin();
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
  if (type === "warp" && !id) {
    showToast("请使用 Warp 官方网页登录添加账号", "error");
    return;
  }
  // WorkBuddy is OAuth-only: the official login flow creates and re-authorizes
  // the account, so the form must never submit a manually typed credential.
  if (type === "workbuddy" && !id) {
    showToast("请使用「使用 WorkBuddy 官方网页登录」添加账号", "error");
    return;
  }
  const token = document.getElementById("clientCookie").value;
  const mode = currentCredentialMode();
  const isOAuth = type === "grok" && mode === "oauth";
  // Build CLI OAuth has no manual inputs: it is created and renewed by the
  // official device login, which saves the account server-side. Submitting the
  // mode without the login would only produce an account without credentials.
  if (isOAuth && !id) {
    showToast("请使用「使用 Grok 官方网页登录」添加 Build CLI OAuth 账号", "error");
    return;
  }

  const splitCredentials = splitBatchCredentialInput(token);
  const { unique: dedupedCredentials, duplicates: duplicateInputs } = dedupeCredentialInputs(type, splitCredentials);
  const { accepted: credentials, conflicts: existingConflicts } = filterExistingCredentialConflicts(type, dedupedCredentials, id);
  const existing = id ? accounts.find((a) => String(a.id) === String(id)) : null;
  const data = {
    account_type: type,
    weight: existing ? (parseInt(existing.weight, 10) || 1) : 1,
    enabled: document.getElementById("enabled").checked,
  };
  if (type === "grok") {
    data.credential_type = isOAuth ? "oauth" : "";
    data.grok_provider = isOAuth ? "build" : "web";
  }

  // A WorkBuddy edit may legitimately keep the stored credential: the refresh
  // token is never returned to the browser, so an empty field means "unchanged".
  const keepStoredWorkBuddyCredential = Boolean(id) && type === "workbuddy" && splitCredentials.length === 0;
  if (type !== "warp" && !isOAuth && credentials.length === 0 && !keepStoredWorkBuddyCredential) {
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
      if (!res.ok) throw new Error(extractAdminErrorDetail(await res.text()));
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
  if (type === 'warp') {
    // Warp authenticates with a browser session that carries no email or
    // username, so identity is shown as a short digest of the session. Two
    // logins then look different instead of both reading 登录会话已配置.
    const fingerprint = String(acc.session_fingerprint || '').trim();
    if (!hasSidebarAccountCredential(acc)) return '待官网登录';
    return fingerprint ? `会话 ${fingerprint.substring(0, 6)}` : '登录会话已配置';
  }
  if (type === 'grok' && isSidebarGrokOAuthAccount(acc)) {
    const accessToken = String(acc.oauth_access_token || "");
    if (accessToken) {
      return accessToken.length > 20
        ? accessToken.substring(0, 8) + '...' + accessToken.substring(accessToken.length - 8)
        : accessToken;
    }
    return 'OAuth 已配置';
  }
  const token = acc.token;
  if (token) {
    if (token.length > 30) {
      if (type === 'grok') {
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
  if (type === 'puter' && getAccountToken(acc)) {
    const token = getAccountToken(acc);
    return token.length > 24 ? token.substring(0, 8) + '...' + token.substring(token.length - 8) : token;
  }
  if (type === 'workbuddy') {
    // The signed-in address identifies both the account and the login; the token
    // itself is deliberately not shown for this channel.
    return workBuddyIdentityLabel(acc);
  }
  if (acc.session_id) {
    return acc.session_id.substring(0, 30) + '...';
  }
  return '-';
}

// workBuddyIdentityLabel renders the account identity: the signed-in address when
// the profile was fetched, otherwise a short uid, otherwise the access token tail.
function workBuddyIdentityLabel(acc) {
  const email = String(acc?.email || "").trim();
  if (email) return email;
  const uid = String(acc?.workbuddy_uid || "").trim();
  if (uid) return uid.length > 20 ? `uid ${uid.substring(0, 8)}...${uid.substring(uid.length - 4)}` : `uid ${uid}`;
  const token = String(acc?.workbuddy_access_token || "").trim();
  if (token) {
    return token.length > 16 ? `token ${token.substring(0, 6)}...${token.substring(token.length - 6)}` : token;
  }
  return "-";
}


// Export accounts
function exportAccounts() {
  window.location.href = "/api/export";
}

// Load accounts on page load
document.addEventListener('DOMContentLoaded', () => {
  initDOMCache();
  loadAccounts();
  const typeSelect = document.getElementById("accountType");
  if (typeSelect) {
    applyTokenLabels(typeSelect.value);
  }
});
