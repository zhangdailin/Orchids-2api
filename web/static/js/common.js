// Common JavaScript functions

function setSidebarAccountStats(total, normal, abnormal) {
  const footerTotal = document.getElementById("footerTotal");
  if (footerTotal) footerTotal.textContent = String(total || 0);

  const footerNormal = document.getElementById("footerNormal");
  if (footerNormal) footerNormal.textContent = String(normal || 0);

  const footerAbnormal = document.getElementById("footerAbnormal");
  if (footerAbnormal) footerAbnormal.textContent = String(abnormal || 0);
}

function normalizeSidebarAccountType(acc) {
  return String(acc?.account_type || "orchids").toLowerCase();
}

function normalizeSidebarStatusCode(statusCode) {
  if (statusCode === null || statusCode === undefined) return "";
  return String(statusCode).trim();
}

function getSidebarAccountToken(acc) {
  if (!acc) return "";
  const type = normalizeSidebarAccountType(acc);
  if (type === "warp") {
    return acc.refresh_token || acc.token || acc.client_cookie || "";
  }
  if (type === "orchids") {
    return acc.client_cookie || acc.session_cookie || acc.token || "";
  }
  if (type === "puter") {
    return acc.client_cookie || acc.token || acc.session_cookie || "";
  }
  return acc.client_cookie || acc.token || "";
}

function getSidebarQuotaStats(acc) {
  if (!acc) return null;
  const type = normalizeSidebarAccountType(acc);
  if (type === "warp") {
    const monthlyLimit = Math.max(0, Math.floor(acc.warp_monthly_limit || acc.usage_limit || 0));
    const monthlyRemainingRaw = acc.warp_monthly_remaining !== undefined && acc.warp_monthly_remaining !== null
      ? acc.warp_monthly_remaining
      : (monthlyLimit > 0 ? monthlyLimit - Math.floor(acc.usage_current || 0) : 0);
    const monthlyRemaining = Math.max(0, Math.floor(monthlyRemainingRaw || 0));
    const bonusRemaining = Math.max(0, Math.floor(acc.warp_bonus_remaining || 0));
    const remaining = monthlyRemaining + bonusRemaining;
    if (monthlyLimit > 0 || bonusRemaining > 0) {
      return {
        supported: true,
        limit: monthlyLimit + bonusRemaining,
        remaining,
      };
    }
  }
  const explicitLimit = Math.floor(acc.quota_limit || 0);
  const hasExplicitRemaining = acc.quota_remaining !== undefined && acc.quota_remaining !== null;
  if (explicitLimit > 0 && hasExplicitRemaining) {
    return { supported: true, limit: explicitLimit, remaining: Math.max(0, Math.floor(acc.quota_remaining || 0)) };
  }

  const limit = Math.floor(acc.usage_limit || 0);
  if (limit <= 0) return null;
  const current = Math.floor(acc.usage_current || 0);
  const remaining = type === "warp" ? Math.max(0, limit - current) : Math.max(0, current);
  return { supported: true, limit, remaining };
}

function isQuotaOnlyStatus(acc) {
  if (!acc) return false;
  const type = normalizeSidebarAccountType(acc);
  if (type !== "puter" && type !== "warp") return false;
  const quota = getSidebarQuotaStats(acc);
  const statusCode = normalizeSidebarStatusCode(acc.status_code);
  if (statusCode === "402") return true;
  if (type === "puter") {
    return Boolean(quota && quota.limit > 0 && quota.remaining <= 0);
  }
  return statusCode === "429" && Boolean(quota && quota.limit > 0 && quota.remaining <= 0);
}

function isPuterQuotaOnlyStatus(acc) {
  return Boolean(acc && normalizeSidebarAccountType(acc) === "puter" && isQuotaOnlyStatus(acc));
}

function isWarpQuotaOnlyStatus(acc) {
  return Boolean(acc && normalizeSidebarAccountType(acc) === "warp" && isQuotaOnlyStatus(acc));
}

function isSidebarAccountAbnormal(acc) {
  if (!acc || !acc.enabled) return true;

  if (isQuotaOnlyStatus(acc)) {
    return false;
  }

  if (normalizeSidebarStatusCode(acc.status_code)) {
    return true;
  }

  const type = normalizeSidebarAccountType(acc);
  if (type === "warp") {
    if (!getSidebarAccountToken(acc)) return true;
  } else if (type === "grok") {
    if (!getSidebarAccountToken(acc)) return true;
  } else if (type === "puter") {
    if (!getSidebarAccountToken(acc)) return true;
  } else if (!acc.session_id && !acc.session_cookie) {
    return true;
  }

  const quota = getSidebarQuotaStats(acc);
  if (quota && quota.limit > 0 && quota.remaining <= 0 && !isQuotaOnlyStatus(acc)) {
    return true;
  }

  return false;
}

function computeSidebarAccountStats(accounts) {
  const list = Array.isArray(accounts) ? accounts : [];
  const total = list.length;
  const abnormal = list.filter(isSidebarAccountAbnormal).length;
  const normal = Math.max(0, total - abnormal);
  return { total, normal, abnormal };
}

async function refreshSidebarAccountStats() {
  if (!document.getElementById("footerTotal")) return;
  if (document.getElementById("accountsList") && document.getElementById("totalAccounts")) return;

  try {
    const res = await fetch("/api/accounts");
    if (!res.ok) return;
    const accounts = await res.json();
    const stats = computeSidebarAccountStats(accounts);
    setSidebarAccountStats(stats.total, stats.normal, stats.abnormal);
  } catch (err) {
    console.debug("Failed to refresh sidebar account stats:", err);
  }
}

function setSidebarOpen(open) {
  const sidebar = document.getElementById("sidebar");
  const overlay = document.querySelector(".sidebar-overlay");
  const shouldOpen = Boolean(open);
  if (sidebar) {
    sidebar.classList.toggle("mobile-open", shouldOpen);
  }
  if (overlay) {
    overlay.classList.toggle("active", shouldOpen);
  }
  document.body.classList.toggle("sidebar-open", shouldOpen);
}

function toggleSidebar(forceOpen) {
  const sidebar = document.getElementById("sidebar");
  if (!sidebar) return;
  if (typeof forceOpen === "boolean") {
    setSidebarOpen(forceOpen);
    return;
  }
  setSidebarOpen(!sidebar.classList.contains("mobile-open"));
}

// Show toast notification
function showToast(msg, type = 'success') {
  let container = document.getElementById("toastContainer");
  if (!container) {
    container = document.createElement("div");
    container.id = "toastContainer";
    container.className = "toast-container";
    document.body.appendChild(container);
  }
  const toast = document.createElement("div");
  toast.className = "toast";
  const color = type === 'success' ? '#34d399' : type === 'info' ? '#38bdf8' : '#fb7185';
  const icon = type === 'success' ? '✅' : type === 'info' ? 'ℹ️' : '❌';
  toast.style.borderLeft = `4px solid ${color}`;
  const iconSpan = document.createElement("span");
  iconSpan.textContent = icon;
  toast.appendChild(iconSpan);
  toast.appendChild(document.createTextNode(` ${msg}`));
  container.appendChild(toast);
  requestAnimationFrame(() => toast.classList.add("show"));
  setTimeout(() => {
    toast.classList.remove("show");
    setTimeout(() => toast.remove(), 400);
  }, 2800);
}

// Copy text to clipboard
async function copyToClipboard(text) {
  try {
    if (navigator.clipboard && window.isSecureContext) {
      await navigator.clipboard.writeText(text);
    } else {
      const el = document.createElement('textarea');
      el.value = text;
      document.body.appendChild(el);
      el.select();
      document.execCommand('copy');
      document.body.removeChild(el);
    }
    showToast("已复制到剪切板");
  } catch (err) {
    showToast("复制失败", "error");
  }
}

// Logout function
async function logout() {
  if (confirm("确定要退出登录吗？")) {
    try {
      await fetch("/api/logout", { method: "POST" });
      window.location.href = "./login.html";
    } catch (err) {
      window.location.href = "./login.html";
    }
  }
}

// Switch between tabs (placeholder - will be implemented with proper routing)
function switchTab(tabName, skipSidebar = false) {
  if (!skipSidebar) {
    setSidebarOpen(false);
  }
  const url = new URL(window.location);
  url.searchParams.set('tab', tabName);
  window.location.href = url.toString();
}

document.addEventListener("DOMContentLoaded", () => {
  refreshSidebarAccountStats();
  document.addEventListener("keydown", (event) => {
    if (event.key === "Escape") {
      setSidebarOpen(false);
    }
  });
});

// Centralized utility helper functions
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

function formatTime(iso) {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return "-";
  const now = new Date();
  const diff = (now - d) / 1000;
  if (diff < 60) return "刚刚";
  if (diff < 3600) return Math.floor(diff / 60) + " 分钟前";
  if (diff < 86400) return Math.floor(diff / 3600) + " 小时前";
  return d.toLocaleString("zh-CN", {
    month: "2-digit",
    day: "2-digit",
    hour: "2-digit",
    minute: "2-digit",
    hour12: false,
  });
}

function formatBytes(bytes) {
  const num = Number(bytes || 0);
  if (!Number.isFinite(num) || num <= 0) return "0 B";
  const units = ["B", "KB", "MB", "GB", "TB"];
  let value = num;
  let idx = 0;
  while (value >= 1024 && idx < units.length - 1) {
    value /= 1024;
    idx++;
  }
  return `${value.toFixed(value >= 10 ? 1 : 2)} ${units[idx]}`;
}
