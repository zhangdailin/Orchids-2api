// Configuration management JavaScript

let apiKeys = [];
let createdKeys = [];

// Switch between config tabs
function switchConfigTab(tab) {
  document.querySelectorAll("#configTabs .tab-item").forEach(btn => {
    btn.classList.toggle("active", btn.textContent.includes(tab === 'basic' ? '基础' : tab === 'auth' ? '授权' : '代理'));
  });
  document.getElementById("basicConfig").style.display = tab === 'basic' ? 'block' : 'none';
  document.getElementById("authConfig").style.display = tab === 'auth' ? 'block' : 'none';
  document.getElementById("proxyConfig").style.display = tab === 'proxy' ? 'block' : 'none';

  if (tab === 'auth') loadApiKeys();
}

// Update switch label
function updateSwitchLabel(el, text) {
  const span = document.getElementById("label_" + el.id);
  if (span) {
    span.textContent = text + (el.checked ? " (已开启)" : " (已关闭)");
  }
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
    document.getElementById("cfg_max_retries").value = cfg.max_retries || 3;
    document.getElementById("cfg_retry_delay").value = cfg.retry_delay || 1000;
    document.getElementById("cfg_switch_count").value = cfg.account_switch_count || 5;
    document.getElementById("cfg_request_timeout").value = cfg.request_timeout || 120;
    document.getElementById("cfg_refresh_interval").value = cfg.token_refresh_interval || 30;

    // Proxy Config Loading
    document.getElementById("cfg_proxy_http").value = cfg.proxy_http || "";
    document.getElementById("cfg_proxy_https").value = cfg.proxy_https || "";
    document.getElementById("cfg_proxy_user").value = cfg.proxy_user || "";
    document.getElementById("cfg_proxy_pass").value = cfg.proxy_pass || "";
    document.getElementById("cfg_proxy_bypass").value = (cfg.proxy_bypass || []).join("\n");

    const autoToken = document.getElementById("cfg_auto_refresh_token");
    autoToken.checked = cfg.auto_refresh_token || false;
    updateSwitchLabel(autoToken, "自动刷新Token");

    const autoUsage = document.getElementById("cfg_auto_refresh_usage");
    autoUsage.checked = cfg.auto_refresh_usage || false;
    updateSwitchLabel(autoUsage, "自动刷新用量");

    document.getElementById("cfg_output_token_count").checked = cfg.output_token_count || false;
    document.getElementById("cfg_cache_token_count").checked = cfg.cache_token_count || false;
    document.getElementById("cfg_cache_ttl").value = cfg.cache_ttl || 5;
    document.getElementById("cfg_cache_strategy").value = cfg.cache_strategy || "split";

  } catch (err) {
    showToast("加载配置失败", "error");
  }
}

// Save configuration to API
async function saveConfiguration() {
  const data = {
    admin_pass: document.getElementById("cfg_admin_pass").value,
    admin_token: document.getElementById("cfg_admin_token").value,
    max_retries: parseInt(document.getElementById("cfg_max_retries").value),
    retry_delay: parseInt(document.getElementById("cfg_retry_delay").value),
    account_switch_count: parseInt(document.getElementById("cfg_switch_count").value),
    request_timeout: parseInt(document.getElementById("cfg_request_timeout").value),
    token_refresh_interval: parseInt(document.getElementById("cfg_refresh_interval").value),
    auto_refresh_token: document.getElementById("cfg_auto_refresh_token").checked,
    auto_refresh_usage: document.getElementById("cfg_auto_refresh_usage").checked,
    output_token_count: document.getElementById("cfg_output_token_count").checked,
    cache_token_count: document.getElementById("cfg_cache_token_count").checked,
    cache_ttl: parseInt(document.getElementById("cfg_cache_ttl").value),
    cache_strategy: document.getElementById("cfg_cache_strategy").value,

    // Proxy Config Saving
    proxy_http: document.getElementById("cfg_proxy_http").value,
    proxy_https: document.getElementById("cfg_proxy_https").value,
    proxy_user: document.getElementById("cfg_proxy_user").value,
    proxy_pass: document.getElementById("cfg_proxy_pass").value,
    proxy_bypass: document.getElementById("cfg_proxy_bypass").value.split("\n").filter(line => line.trim() !== "")
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
    container.innerHTML = '<div class="empty-state"><p>暂无 API Key，点击上方按钮创建</p></div>';
    return;
  }

  const rows = apiKeys.map((k, idx) => {
    const keyDisplay = k.key_full || k.key_prefix + '****' + k.key_suffix;
    return `
      <tr>
        <td style="font-weight: 600;">${escapeHtml(k.name)}</td>
        <td>
          <div style="display: flex; align-items: center; gap: 8px;">
            <span style="cursor: pointer;" onclick="toggleKeyVisibility(${idx})">👁️</span>
            <span id="key-display-${idx}" style="font-family: monospace; color: #64748b; cursor: pointer;" onclick="copyToClipboard('${keyDisplay}')">
              ${k.key_prefix}****...${k.key_suffix}
            </span>
          </div>
        </td>
        <td>
          <label class="toggle" style="transform: scale(0.8);">
            <input type="checkbox" ${k.enabled ? "checked" : ""} onchange="toggleKeyStatus(${k.id}, this.checked)">
            <span class="toggle-slider"></span>
          </label>
        </td>
        <td style="color: #94a3b8; font-size: 0.8rem;">${k.last_used_at ? formatTime(k.last_used_at) : "从未使用"}</td>
        <td>
          <button class="btn btn-danger-outline" style="padding: 4px 8px;" onclick="openDeleteKeyModal(${k.id}, '${escapeHtml(k.name)}')">删除</button>
        </td>
      </tr>
    `;
  }).join("");

  container.innerHTML = `
    <table>
      <thead>
        <tr>
          <th>名称</th>
          <th>Key</th>
          <th>状态</th>
          <th>最后使用</th>
          <th>操作</th>
        </tr>
      </thead>
      <tbody>
        ${rows}
      </tbody>
    </table>`;
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
  const keyDisplays = createdKeys.map(k => `
    <div class="key-display" style="margin-bottom: 8px; padding: 12px; background: #f8fafc; border: 1px dashed #cbd5e1; border-radius: 8px;">
      <div style="font-size: 0.8rem; color: #64748b;">${escapeHtml(k.name)}</div>
      <div style="font-weight: bold; margin-top: 4px; word-break: break-all; color: #22c55e;">${escapeHtml(k.key || k.error)}</div>
    </div>
  `).join("");
  container.innerHTML = keyDisplays;
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
  div.textContent = text;
  return div.innerHTML;
}

// Load configuration on page load
document.addEventListener('DOMContentLoaded', () => {
  loadConfiguration();
});
