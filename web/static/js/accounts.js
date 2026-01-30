// Accounts management JavaScript

let accounts = [];
let currentPlatform = '';
let accountHealth = {};
let autoCheckTimer = null;
let autoCheckRunning = false;
let pageSize = 20;

// Load accounts from API
async function loadAccounts() {
  try {
    const res = await fetch("/api/accounts");
    if (res.status === 401) {
      window.location.href = "./login.html";
      return;
    }
    accounts = await res.json();
    renderPlatformTabs();
    renderAccounts();
    updateStats();
  } catch (err) {
    console.error("Failed to load accounts:", err);
    showToast("加载账号失败", "error");
  }
}

// Normalize account type
function normalizeAccountType(acc) {
  return (acc.account_type || 'orchids').toLowerCase();
}

// Render platform filter tabs
function renderPlatformTabs() {
  const container = document.getElementById("platformFilters");
  if (!container) return;
  const types = new Set(accounts.map(normalizeAccountType));
  const sorted = Array.from(types).sort();
  const tabs = [...sorted];

  if (currentPlatform === '' || !tabs.includes(currentPlatform)) {
    if (currentPlatform !== '' && tabs.length > 0) {
      currentPlatform = tabs[0];
    }
    if (currentPlatform === '' && tabs.length > 0) currentPlatform = tabs[0];
  }

  container.innerHTML = tabs.map(type => {
    const label = type;
    const isActive = currentPlatform === label;
    return `<button class="tab-item ${isActive ? 'active' : ''}" onclick="filterByPlatform('${label}')">${label}</button>`;
  }).join("");
}

// Update account health status
function updateAccountHealth(id, ok, msg = '') {
  accountHealth[id] = {
    ok,
    msg,
    checkedAt: new Date().toISOString(),
  };
}

// Get status badge for account
function statusBadge(acc) {
  const health = accountHealth[acc.id];
  if (health && !health.ok) {
    return { text: '异常', color: '#fb7185', bg: 'rgba(251, 113, 133, 0.16)', tip: health.msg || '检测失败' };
  }
  if (!acc.enabled) {
    return { text: '禁用', color: '#fb7185', bg: 'rgba(251, 113, 133, 0.16)', tip: '账号已禁用' };
  }
  if (!acc.session_id && !acc.session_cookie) {
    return { text: '待补全', color: '#f59e0b', bg: 'rgba(245, 158, 11, 0.16)', tip: '缺少会话信息' };
  }
  return { text: '正常', color: '#34d399', bg: 'rgba(52, 211, 153, 0.16)', tip: '状态正常' };
}

// Check single account
async function checkAccount(id, silent = false) {
  try {
    const res = await fetch(`/api/accounts/${id}/refresh`);
    if (!res.ok) {
      throw new Error(await res.text());
    }
    const updated = await res.json();
    accounts = accounts.map(a => (a.id === id ? updated : a));
    updateAccountHealth(id, true);
    if (!silent) showToast(`账号 ${updated.name || updated.email || id} 正常`, "success");
  } catch (err) {
    updateAccountHealth(id, false, err.message || String(err));
    if (!silent) showToast(`账号 ${id} 检测失败`, "error");
  } finally {
    renderAccounts();
    updateStats();
  }
}

// Check all accounts
async function checkAllAccounts() {
  if (autoCheckRunning) return;
  if (!accounts.length) return;
  autoCheckRunning = true;
  showToast("开始检测账号状态", "info");
  for (const acc of accounts) {
    await checkAccount(acc.id, true);
  }
  showToast("账号检测完成", "success");
  autoCheckRunning = false;
}

// Toggle auto check
function toggleAutoCheck(enabled) {
  if (enabled) {
    if (autoCheckTimer) clearInterval(autoCheckTimer);
    autoCheckTimer = setInterval(checkAllAccounts, 10 * 60 * 1000);
    checkAllAccounts();
    showToast("已开启自动检测", "success");
  } else {
    if (autoCheckTimer) clearInterval(autoCheckTimer);
    autoCheckTimer = null;
    showToast("已关闭自动检测", "info");
  }
  localStorage.setItem("autoCheckEnabled", enabled ? "true" : "false");
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
  const abnormal = accounts.filter(a => !a.enabled);
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

// Render accounts table
function renderAccounts() {
  const container = document.getElementById("accountsList");
  const filtered = accounts.filter(acc => {
    if (!currentPlatform) return true;
    const key = currentPlatform.toLowerCase();
    return normalizeAccountType(acc).includes(key)
      || acc.agent_mode?.toLowerCase().includes(key)
      || acc.name?.toLowerCase().includes(key);
  });

  if (filtered.length === 0) {
    container.innerHTML = `
      <div class="empty-state" style="display: flex; flex-direction: column; align-items: center; justify-content: center; height: 300px; color: #94a3b8;">
        <span style="font-size: 3rem; margin-bottom: 16px;">📂</span>
        <p>暂无 ${currentPlatform ? currentPlatform : ''} 账号数据</p>
      </div>`;
    document.getElementById("paginationInfo").textContent = `共 0 条记录，第 1/1 页`;
    return;
  }

  const rows = filtered.map(acc => {
    const badge = statusBadge(acc);
    return `
      <tr>
        <td><input type="checkbox" class="row-checkbox" data-id="${acc.id}" onchange="updateSelectedCount()"></td>
        <td style="color: #64748b; font-size: 0.9rem;">${acc.id}</td>
        <td>
          <span class="token-text" title="${escapeHtml(acc.token || '-')}">${escapeHtml(acc.token || (acc.session_id ? acc.session_id.substring(0, 20) + '...' : '-'))}</span>
        </td>
        <td><span class="tag tag-free">${acc.subscription || 'free'}</span></td>
        <td>
          <div style="display: flex; align-items: center; gap: 8px;">
            <div class="usage-progress-container">
              <div class="usage-progress-bar ${acc.usage_current / (acc.usage_total || 550) > 0.8 ? 'red' : acc.usage_current / (acc.usage_total || 550) > 0.5 ? 'orange' : 'green'}"
                   style="width: ${Math.min(100, (acc.usage_current / (acc.usage_total || 550)) * 100)}%;"></div>
            </div>
            <span style="font-size: 0.8rem; color: #64748b;">${(acc.usage_current || 0).toFixed(2)}/${acc.usage_total || 550}</span>
          </div>
        </td>
        <td>
          <span class="tag tag-status-normal" title="${escapeHtml(badge.tip)}" style="background: ${badge.bg}; color: ${badge.color}; border: none;">
            ${badge.text}
          </span>
        </td>
        <td style="font-size: 0.9rem; color: #e2e8f0; font-weight: 500;">${acc.request_count || 0}</td>
        <td style="font-size: 0.8rem; color: #64748b;">${acc.last_used_at && !acc.last_used_at.startsWith('0001') ? formatTime(acc.last_used_at) : '-'}</td>
        <td style="color: #64748b; font-size: 0.9rem;">${acc.reset_date || '-'}</td>
        <td style="text-align: right;">
          <div style="display: flex; justify-content: flex-end; gap: 12px;">
            <i class="action-icon" onclick="refreshToken(${acc.id})" title="刷新">🔄</i>
            <i class="action-icon" onclick="deleteAccount(${acc.id})" title="删除">🗑️</i>
          </div>
        </td>
      </tr>
    `;
  }).join("");

  container.innerHTML = `
    <div class="table-wrap">
      <table>
        <thead>
          <tr>
            <th style="width: 40px;"><input type="checkbox" onchange="toggleSelectAll(this.checked)"></th>
            <th style="width: 60px;">ID</th>
            <th>TOKEN</th>
            <th>订阅</th>
            <th style="width: 180px;">用量</th>
            <th>状态</th>
            <th>调用</th>
            <th>最后调用</th>
            <th>重置日期</th>
            <th style="text-align: right;">操作</th>
          </tr>
        </thead>
        <tbody>
          ${rows}
        </tbody>
      </table>
    </div>
  `;

  document.getElementById("paginationInfo").textContent = `共 ${filtered.length} 条记录，第 1/1 页`;
  updateSelectedCount();
}

// Update statistics
function updateStats() {
  const total = accounts.length;
  const enabled = accounts.filter((a) => a.enabled).length;
  const abnormal = accounts.filter((a) => !a.enabled || (accountHealth[a.id] && !accountHealth[a.id].ok)).length;

  document.getElementById("totalAccounts").textContent = total;
  document.getElementById("enabledAccounts").textContent = enabled;
  document.getElementById("disabledAccounts").textContent = abnormal;

  document.getElementById("footerTotal").textContent = total;
  document.getElementById("footerNormal").textContent = enabled;
  document.getElementById("footerAbnormal").textContent = abnormal;

  const totalUsage = accounts.reduce((sum, acc) => sum + (acc.usage_current || 0), 0);
  const totalLimit = accounts.reduce((sum, acc) => sum + (acc.usage_total || 550), 0);

  const usageText = document.querySelector('.sidebar-footer div[style*="font-size: 0.8rem"]');
  if (usageText && usageText.firstChild) {
    usageText.firstChild.textContent = `今日用量 `;
  }
  const usageSpan = document.querySelector('.sidebar-footer span[style*="float: right"]');
  if (usageSpan) {
    usageSpan.textContent = `${totalUsage.toFixed(1)}/${totalLimit}`;
  }

  const percent = totalLimit > 0 ? (totalUsage / totalLimit) * 100 : 0;
  const progressBar = document.getElementById("usageProgress");
  if (progressBar) {
    progressBar.style.width = Math.min(100, percent) + "%";
  }
}

// Update selected count
function updateSelectedCount() {
  const checked = document.querySelectorAll(".row-checkbox:checked").length;
  document.getElementById("selectedCount").textContent = checked;
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

// Filter by platform
function filterByPlatform(platform) {
  currentPlatform = platform;
  document.querySelectorAll("#platformFilters .tab-item").forEach(btn => {
    btn.classList.toggle("active", btn.textContent === currentPlatform);
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
  renderAccounts();
}

// Open modal
function openModal(account = null) {
  const modal = document.getElementById("accountModal");
  const title = document.getElementById("modalTitle");
  const form = document.getElementById("accountForm");

  if (account) {
    title.textContent = "编辑账号";
    document.getElementById("accountId").value = account.id;
    document.getElementById("name").value = account.name || '';
    document.getElementById("accountType").value = account.account_type || "orchids";
    document.getElementById("clientCookie").value = account.client_cookie || '';
    document.getElementById("agentMode").value = account.agent_mode || 'claude-opus-4.5';
    document.getElementById("weight").value = account.weight || 1;
    document.getElementById("enabled").checked = account.enabled;
  } else {
    title.textContent = "添加账号";
    form.reset();
    document.getElementById("accountId").value = "";
    document.getElementById("agentMode").value = "claude-opus-4.5";
    document.getElementById("weight").value = "1";
    document.getElementById("accountType").value = "orchids";
    document.getElementById("enabled").checked = true;
  }
  modal.classList.add("active");
  modal.style.display = "flex";
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
  const data = {
    name: document.getElementById("name").value,
    account_type: document.getElementById("accountType").value,
    client_cookie: document.getElementById("clientCookie").value,
    agent_mode: document.getElementById("agentMode").value,
    weight: parseInt(document.getElementById("weight").value) || 1,
    enabled: document.getElementById("enabled").checked,
  };

  try {
    const url = id ? `/api/accounts/${id}` : "/api/accounts";
    const method = id ? "PUT" : "POST";
    const res = await fetch(url, {
      method,
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(data),
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
  try {
    showToast("正在查询账号信息...", "info");
    const res = await fetch(`/api/accounts/${id}/refresh`);
    if (!res.ok) throw new Error(await res.text());
    const acc = await res.json();
    showToast(`账号 ${acc.email || id} 刷新完成`);
    loadAccounts();
  } catch (err) {
    showToast("刷新失败: " + err.message, "error");
  }
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
  if (!text) return '';
  const div = document.createElement("div");
  div.textContent = text;
  return div.innerHTML;
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
  loadAccounts();

  // Restore auto check setting
  const autoCheckEnabled = localStorage.getItem("autoCheckEnabled") === "true";
  if (autoCheckEnabled) {
    document.getElementById("autoCheckToggle").checked = true;
    toggleAutoCheck(true);
  }
});
