// Configuration management JavaScript

let apiKeys = [];
let createdKeys = [];

// Switch between config tabs
function switchConfigTab(tab) {
  document.querySelectorAll("#configTabs .tab-item").forEach(btn => {
    btn.classList.toggle("active",
      (tab === 'basic' && btn.textContent.includes('基础')) ||
      (tab === 'auth' && btn.textContent.includes('API Key'))
    );
  });
  document.getElementById("basicConfig").style.display = tab === 'basic' ? 'block' : 'none';
  document.getElementById("authConfig").style.display = tab === 'auth' ? 'block' : 'none';

  if (tab === 'auth') loadApiKeys();
}

// Toggle password visibility
function togglePassword(fieldId) {
  const field = document.getElementById(fieldId);
  if (field) {
    field.type = field.type === 'password' ? 'text' : 'password';
  }
}

// Copy field value to clipboard
function copyFieldValue(fieldId) {
  const field = document.getElementById(fieldId);
  if (field && field.value) {
    copyToClipboard(field.value);
  }
}

function parseProxyBypass(raw) {
  if (!raw) return [];
  return raw
    .split(/[\n,]/)
    .map((item) => item.trim())
    .filter(Boolean);
}

function normalizeProxyBypass(value) {
  if (Array.isArray(value)) return value;
  if (typeof value === "string") return parseProxyBypass(value);
  return [];
}

// Load configuration from API
async function loadConfiguration() {
  try {
    const res = await fetch("/api/config");
    if (res.status === 401) {
      window.location.href = "./login.html";
      return;
    }
    const cfg = await res.json();

    document.getElementById("cfg_admin_pass").value = cfg.admin_pass || "";
    document.getElementById("cfg_admin_token").value = cfg.admin_token || "";
    document.getElementById("cfg_proxy_http").value = cfg.proxy_http || "";
    document.getElementById("cfg_proxy_https").value = cfg.proxy_https || "";
    document.getElementById("cfg_proxy_user").value = cfg.proxy_user || "";
    document.getElementById("cfg_proxy_pass").value = cfg.proxy_pass || "";
    const proxyBypass = normalizeProxyBypass(cfg.proxy_bypass);
    document.getElementById("cfg_proxy_bypass").value = proxyBypass.join("\n");

    const cacheTokenCount = document.getElementById("cfg_cache_token_count");
    const cacheEnabled = typeof cfg.cache_token_count === "boolean" ? cfg.cache_token_count : true;
    cacheTokenCount.checked = cacheEnabled;

    document.getElementById("cfg_cache_ttl").value = cfg.cache_ttl || 5;

    const rawStrategy = String(cfg.cache_strategy || "mix").toLowerCase();
    document.getElementById("cfg_cache_strategy").value = rawStrategy === "split" ? "split" : "mix";

  } catch (err) {
    showToast("加载配置失败", "error");
  }
}

// Save configuration to API
async function saveConfiguration() {
  const proxyBypassRaw = document.getElementById("cfg_proxy_bypass").value;
  const data = {
    admin_pass: document.getElementById("cfg_admin_pass").value,
    admin_token: document.getElementById("cfg_admin_token").value,
    proxy_http: document.getElementById("cfg_proxy_http").value.trim(),
    proxy_https: document.getElementById("cfg_proxy_https").value.trim(),
    proxy_user: document.getElementById("cfg_proxy_user").value.trim(),
    proxy_pass: document.getElementById("cfg_proxy_pass").value,
    proxy_bypass: parseProxyBypass(proxyBypassRaw),
    cache_token_count: document.getElementById("cfg_cache_token_count").checked,
    cache_ttl: parseInt(document.getElementById("cfg_cache_ttl").value, 10) || 5,
    cache_strategy: document.getElementById("cfg_cache_strategy").value,
  };

  try {
    const res = await fetch("/api/config", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(data)
    });
    if (!res.ok) throw new Error(await res.text());
    showToast("配置保存成功");
  } catch (err) {
    showToast("保存失败: " + err.message, "error");
  }
}

// Load API Keys
async function loadApiKeys() {
  try {
    const res = await fetch("/api/keys");
    if (res.status === 401) {
      window.location.href = "./login.html";
      return;
    }
    apiKeys = (await res.json()) || [];
    renderApiKeys();
  } catch (err) {
    showToast("加载 API Keys 失败", "error");
  }
}

// Render API Keys table
function renderApiKeys() {
  const container = document.getElementById("keysList");
  if (apiKeys.length === 0) {
    container.innerHTML = "";
    const empty = document.createElement("div");
    empty.className = "empty-state";
    const p = document.createElement("p");
    p.textContent = "暂无 API Key，点击上方按钮创建";
    empty.appendChild(p);
    container.appendChild(empty);
    return;
  }

  container.innerHTML = "";
  const table = document.createElement("table");
  const thead = document.createElement("thead");
  const headRow = document.createElement("tr");
  ["Token", "状态", "最后使用", "操作"].forEach((label) => {
    const th = document.createElement("th");
    th.textContent = label;
    headRow.appendChild(th);
  });
  thead.appendChild(headRow);
  table.appendChild(thead);

  const tbody = document.createElement("tbody");
  apiKeys.forEach((k, idx) => {
    const keyDisplay = k.key_full || `${k.key_prefix}****${k.key_suffix}`;
    const encodedKey = encodeURIComponent(keyDisplay);
    const encodedLabel = encodeURIComponent(`${k.key_prefix}...${k.key_suffix}`);
    const tr = document.createElement("tr");

    const tdToken = document.createElement("td");
    const tokenWrap = document.createElement("div");
    tokenWrap.style.display = "flex";
    tokenWrap.style.alignItems = "center";
    tokenWrap.style.gap = "8px";
    const toggle = document.createElement("span");
    toggle.className = "key-toggle";
    toggle.dataset.idx = String(idx);
    toggle.style.cursor = "pointer";
    toggle.textContent = "👁️";
    const display = document.createElement("span");
    display.id = `key-display-${idx}`;
    display.className = "key-display";
    display.dataset.key = encodedKey;
    display.style.fontFamily = "monospace";
    display.style.color = "var(--text-secondary)";
    display.style.cursor = "pointer";
    display.textContent = `${k.key_prefix || ""}****...${k.key_suffix || ""}`;
    tokenWrap.appendChild(toggle);
    tokenWrap.appendChild(display);
    tdToken.appendChild(tokenWrap);
    tr.appendChild(tdToken);

    const tdStatus = document.createElement("td");
    const label = document.createElement("label");
    label.className = "toggle";
    label.style.transform = "scale(0.8)";
    const checkbox = document.createElement("input");
    checkbox.type = "checkbox";
    checkbox.checked = !!k.enabled;
    checkbox.dataset.action = "toggle-key";
    checkbox.dataset.id = encodeData(k.id);
    const slider = document.createElement("span");
    slider.className = "toggle-slider";
    label.appendChild(checkbox);
    label.appendChild(slider);
    tdStatus.appendChild(label);
    tr.appendChild(tdStatus);

    const tdLast = document.createElement("td");
    tdLast.style.color = "var(--text-secondary)";
    tdLast.style.fontSize = "0.8rem";
    tdLast.textContent = k.last_used_at ? formatTime(k.last_used_at) : "从未使用";
    tr.appendChild(tdLast);

    const tdAction = document.createElement("td");
    const delBtn = document.createElement("button");
    delBtn.className = "btn btn-danger-outline";
    delBtn.style.padding = "4px 8px";
    delBtn.dataset.action = "delete-key";
    delBtn.dataset.id = encodeData(k.id);
    delBtn.dataset.label = encodedLabel;
    delBtn.textContent = "删除";
    tdAction.appendChild(delBtn);
    tr.appendChild(tdAction);

    tbody.appendChild(tr);
  });
  table.appendChild(tbody);
  container.appendChild(table);

  const tip = document.createElement("div");
  tip.style.marginTop = "24px";
  tip.style.padding = "16px";
  tip.style.background = "rgba(56, 189, 248, 0.1)";
  tip.style.border = "1px solid var(--accent-blue)";
  tip.style.borderRadius = "8px";
  tip.style.color = "var(--text-primary)";
  const tipRow = document.createElement("div");
  tipRow.style.display = "flex";
  tipRow.style.gap = "8px";
  tipRow.style.alignItems = "start";
  const tipIcon = document.createElement("span");
  tipIcon.style.fontSize = "1.2rem";
  tipIcon.textContent = "💡";
  const tipBody = document.createElement("div");
  tipBody.style.flex = "1";
  const tipTitle = document.createElement("div");
  tipTitle.style.fontWeight = "600";
  tipTitle.style.marginBottom = "4px";
  tipTitle.textContent = "提示";
  const tipText = document.createElement("div");
  tipText.style.fontSize = "0.9rem";
  tipText.style.lineHeight = "1.6";
  const tipLines = [
    "• API Key 用于访问接口的身份认证",
    "• 禁用的 Key 将无法访问 API",
    "• 请妥善保管您的 API Key，不要泄露给他人",
  ];
  tipLines.forEach((line, idx) => {
    if (idx > 0) tipText.appendChild(document.createElement("br"));
    tipText.appendChild(document.createTextNode(line));
  });
  tipBody.appendChild(tipTitle);
  tipBody.appendChild(tipText);
  tipRow.appendChild(tipIcon);
  tipRow.appendChild(tipBody);
  tip.appendChild(tipRow);
  container.appendChild(tip);

  container.onclick = (e) => {
    const display = e.target.closest(".key-display");
    if (display && container.contains(display)) {
      const encoded = display.dataset.key || "";
      const value = encoded ? decodeURIComponent(encoded) : (display.textContent || "");
      copyToClipboard(value);
      return;
    }
    const toggle = e.target.closest(".key-toggle");
    if (toggle && container.contains(toggle)) {
      const idx = parseInt(toggle.dataset.idx, 10);
      if (!Number.isNaN(idx)) toggleKeyVisibility(idx);
      return;
    }
    const actionEl = e.target.closest("[data-action]");
    if (!actionEl || !container.contains(actionEl)) return;
    const action = actionEl.dataset.action;
    if (action === "delete-key") {
      const id = decodeData(actionEl.dataset.id || "");
      const label = actionEl.dataset.label ? decodeURIComponent(actionEl.dataset.label) : "";
      if (id) openDeleteKeyModal(id, label);
    }
  };

  container.onchange = (e) => {
    const target = e.target;
    if (!(target instanceof HTMLInputElement)) return;
    if (target.dataset.action !== "toggle-key") return;
    const id = decodeData(target.dataset.id || "");
    if (!id) return;
    toggleKeyStatus(id, target.checked);
  };
}

// Toggle key visibility
function toggleKeyVisibility(idx) {
  const span = document.getElementById(`key-display-${idx}`);
  const k = apiKeys[idx];
  if (span.textContent.includes('****')) {
    span.textContent = k.key_full || (k.key_prefix + '****' + k.key_suffix);
  } else {
    span.textContent = `${k.key_prefix}****...${k.key_suffix}`;
  }
}

// Toggle key status
async function toggleKeyStatus(id, enabled) {
  try {
    await fetch(`/api/keys/${id}`, {
      method: "PATCH",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ enabled }),
    });
    showToast(enabled ? "已启用" : "已禁用");
  } catch (err) {
    showToast("操作失败", "error");
  }
}

// Open create key modal
function openCreateKeyModal() {
  document.getElementById("keyName").value = "";
  document.getElementById("createKeyModal").classList.add("active");
  document.getElementById("createKeyModal").style.display = "flex";
}

// Close create key modal
function closeCreateKeyModal() {
  document.getElementById("createKeyModal").classList.remove("active");
  document.getElementById("createKeyModal").style.display = "none";
}

// Create API key
async function createApiKey(e) {
  e.preventDefault();
  const names = document.getElementById("keyName").value.split("\n").filter(n => n.trim());
  if (names.length === 0) return;

  createdKeys = [];
  for (const name of names) {
    try {
      const res = await fetch("/api/keys", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ name }),
      });
      const data = await res.json();
      createdKeys.push({ name, key: data.key });
    } catch (err) {
      createdKeys.push({ name, error: err.message });
    }
  }
  closeCreateKeyModal();
  renderCreatedKeys();
  document.getElementById("showKeyModal").classList.add("active");
  document.getElementById("showKeyModal").style.display = "flex";
  loadApiKeys();
}

// Render created keys
function renderCreatedKeys() {
  const container = document.getElementById("fullKeyDisplay");
  container.innerHTML = "";
  createdKeys.forEach((k) => {
    const wrap = document.createElement("div");
    wrap.className = "key-display";
    wrap.style.marginBottom = "8px";
    wrap.style.padding = "12px";
    wrap.style.background = "var(--card-soft)";
    wrap.style.border = "1px dashed var(--border-color)";
    wrap.style.borderRadius = "8px";

    const name = document.createElement("div");
    name.style.fontSize = "0.8rem";
    name.style.color = "var(--text-secondary)";
    name.textContent = k.name || "";

    const key = document.createElement("div");
    key.style.fontWeight = "bold";
    key.style.marginTop = "4px";
    key.style.wordBreak = "break-all";
    key.style.color = "var(--accent-green)";
    key.textContent = k.key || k.error || "";

    wrap.appendChild(name);
    wrap.appendChild(key);
    container.appendChild(wrap);
  });
}

// Copy all keys
function copyAllKeys() {
  const text = createdKeys.map(k => `${k.name}: ${k.key || k.error}`).join("\n");
  copyToClipboard(text);
}

// Close show key modal
function closeShowKeyModal() {
  document.getElementById("showKeyModal").classList.remove("active");
  document.getElementById("showKeyModal").style.display = "none";
}

// Open delete key modal
function openDeleteKeyModal(id, name) {
  document.getElementById("deleteKeyId").value = id;
  document.getElementById("deleteKeyName").textContent = name;
  const modal = document.getElementById("deleteKeyModal");
  modal.classList.add("active");
  modal.style.display = "flex";
}

// Close delete key modal
function closeDeleteKeyModal() {
  const modal = document.getElementById("deleteKeyModal");
  modal.classList.remove("active");
  modal.style.display = "none";
}

// Confirm delete key
async function confirmDeleteKey() {
  const id = document.getElementById("deleteKeyId").value;
  try {
    await fetch(`/api/keys/${id}`, { method: "DELETE" });
    closeDeleteKeyModal();
    showToast("删除成功");
    loadApiKeys();
  } catch (err) {
    showToast("删除失败", "error");
  }
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

// Escape HTML to prevent XSS
function escapeHtml(text) {
  const div = document.createElement('div');
  div.textContent = text === null || text === undefined ? "" : String(text);
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

function formatBytes(bytes) {
  const n = Number(bytes) || 0;
  if (n >= 1024 * 1024) {
    return (n / (1024 * 1024)).toFixed(2) + " MB";
  }
  if (n >= 1024) {
    return (n / 1024).toFixed(2) + " KB";
  }
  return n + " B";
}

function toggleCacheConfig(checked) {
  const details = document.getElementById("cacheConfigDetails");
  if (!details) return;
  details.style.display = checked ? "block" : "none";
  if (checked) {
    updateMemoryEstimation();
    loadCacheStats();
  }
}

function updateMemoryEstimation() {
  const ttlInput = document.getElementById("cfg_cache_ttl");
  const strategyInput = document.getElementById("cfg_cache_strategy");
  if (!ttlInput || !strategyInput) return;

  const ttlMin = parseInt(ttlInput.value, 10) || 5;
  const strategy = strategyInput.value;
  const mult = strategy === "split" ? 2 : 1;
  const ttlSec = ttlMin * 60;

  const ttlEl = document.getElementById("estTTLSeconds");
  const multEl = document.getElementById("estStrategyMult");
  const titleEl = document.getElementById("memoryEstTitle");
  if (ttlEl) ttlEl.textContent = String(ttlSec);
  if (multEl) multEl.textContent = mult === 2 ? "× 2" : "× 1";
  if (titleEl) {
    titleEl.textContent = `内存估算 (当前: TTL=${ttlMin}分钟, ${strategy === "split" ? "分离缓存×2" : "混合缓存×1"})`;
  }

  const calc = (qps) => {
    const kb = qps * ttlSec * 0.5 * mult;
    if (kb > 1024) return (kb / 1024).toFixed(1) + "MB";
    return kb.toFixed(1) + "KB";
  };

  const lowEl = document.getElementById("estLow");
  const midEl = document.getElementById("estMid");
  const highEl = document.getElementById("estHigh");
  if (lowEl) lowEl.textContent = calc(10);
  if (midEl) midEl.textContent = calc(50);
  if (highEl) highEl.textContent = calc(100);
}

async function loadCacheStats() {
  const statsEl = document.getElementById("cacheStatsText");
  if (!statsEl) return;

  try {
    const res = await fetch("/api/config/cache/stats");
    if (!res.ok) {
      throw new Error(await res.text());
    }
    const data = await res.json();

    if (data.status === "disabled") {
      statsEl.textContent = "缓存未启用";
      return;
    }

    statsEl.textContent = `缓存条目: ${Number(data.count) || 0} 条，占用内存: ${formatBytes(data.size_bytes)}`;
  } catch (err) {
    statsEl.textContent = "缓存统计加载失败";
  }
}

async function clearCache() {
  if (!confirm("确定要清空 Token 用量缓存吗？")) return;
  try {
    const res = await fetch("/api/config/cache/clear", { method: "POST" });
    if (!res.ok) throw new Error(await res.text());
    showToast("缓存已清空");
    loadCacheStats();
  } catch (err) {
    showToast("清空失败: " + err.message, "error");
  }
}

// Load configuration on page load
document.addEventListener('DOMContentLoaded', () => {
  loadConfiguration().then(() => {
    const cacheEnabled = !!document.getElementById("cfg_cache_token_count")?.checked;
    toggleCacheConfig(cacheEnabled);
    updateMemoryEstimation();
    if (cacheEnabled) {
      loadCacheStats();
    }
    loadApiKeys();
  });
});
