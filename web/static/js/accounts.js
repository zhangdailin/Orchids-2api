// Accounts management JavaScript

let accounts = [];
let currentPlatform = '';
let accountHealth = {};
let pageSize = 20;
let currentPage = 1;

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
  return (acc.account_type || 'orchids').toLowerCase();
}

function getAccountToken(acc) {
  if (!acc) return '';
  const type = normalizeAccountType(acc);
  if (type === 'warp') {
    return acc.refresh_token || acc.token || acc.client_cookie || '';
  }
  return acc.client_cookie || '';
}

function applyTokenLabels(type) {
  const label = document.getElementById("tokenLabel");
  const input = document.getElementById("clientCookie");
  const hint = document.getElementById("tokenHint");
  if (!label || !input || !hint) return;
  if (type === 'warp') {
    label.textContent = "Refresh Token";
    input.placeholder = "粘贴 refresh_token";
    hint.textContent = "Warp 只需要 refresh_token";
  } else {
    label.textContent = "Client Cookie / JWT";
    input.placeholder = "粘贴 Clerk Cookie 或 JWT";
    hint.textContent = "支持完整 Cookie（含 __session）或纯 JWT；运行时自动获取";
  }
}

// Render platform filter tabs
function renderPlatformTabs() {
  const container = document.getElementById("platformFilters");
  if (!container) return;
  const defaultTypes = ["orchids", "warp"];
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

// Get status badge for account
function statusBadge(acc) {
  const health = accountHealth[acc.id];
  if (health && !health.ok) {
    return { text: '异常', color: '#fb7185', bg: 'rgba(251, 113, 133, 0.16)', tip: health.msg || '检测失败' };
  }
  if (!acc.enabled) {
    return { text: '禁用', color: '#fb7185', bg: 'rgba(251, 113, 133, 0.16)', tip: '账号已禁用' };
  }
  // Check backend status_code
  if (acc.status_code) {
    switch (acc.status_code) {
      case '429':
        return { text: '限流', color: '#f59e0b', bg: 'rgba(245, 158, 11, 0.16)', tip: '请求过于频繁 (429)' };
      case '401':
        return { text: '未授权', color: '#fb7185', bg: 'rgba(251, 113, 133, 0.16)', tip: '认证失败 (401)' };
      case '403':
        return { text: '禁止访问', color: '#fb7185', bg: 'rgba(251, 113, 133, 0.16)', tip: '访问被拒绝 (403)' };
      case '404':
        return { text: '不存在', color: '#fb7185', bg: 'rgba(251, 113, 133, 0.16)', tip: '资源不存在 (404)' };
      default:
        return { text: '异常', color: '#fb7185', bg: 'rgba(251, 113, 133, 0.16)', tip: '状态异常: ' + acc.status_code };
    }
  }
  const type = normalizeAccountType(acc);
  if (type === 'warp') {
    if (!getAccountToken(acc)) {
      return { text: '待补全', color: '#f59e0b', bg: 'rgba(245, 158, 11, 0.16)', tip: '缺少 Refresh Token' };
    }
  } else if (!acc.session_id && !acc.session_cookie) {
    return { text: '待补全', color: '#f59e0b', bg: 'rgba(245, 158, 11, 0.16)', tip: '缺少会话信息' };
  }
  if (acc.usage_limit > 0) {
    const used = acc.usage_current || 0;
    if (used >= acc.usage_limit) {
      return { text: '配额已满', color: '#fb7185', bg: 'rgba(251, 113, 133, 0.16)', tip: '配额已用尽 (已用 ' + Math.floor(used) + ' / ' + Math.floor(acc.usage_limit) + ')' };
    }
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
  const abnormal = accounts.filter(a => !a.enabled || a.status_code || (accountHealth[a.id] && !accountHealth[a.id].ok));
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
    document.getElementById("paginationInfo").textContent = `共 0 条记录，第 1/1 页`;
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
    { label: "今日用量", style: "width: 100px;" },
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

    const tdUsage = document.createElement("td");
    const usageCurrent = acc.usage_daily || 0;
    tdUsage.style.fontSize = "0.9rem";
    tdUsage.style.color = "#94a3b8";
    tdUsage.textContent = usageCurrent.toFixed(2);
    tr.appendChild(tdUsage);

    const tdQuota = document.createElement("td");
    tdQuota.style.fontSize = "0.85rem";
    if (acc.usage_limit > 0) {
      const used = Math.floor(acc.usage_current || 0);
      const limit = Math.floor(acc.usage_limit);
      const pct = limit > 0 ? Math.min(100, Math.round((used / limit) * 100)) : 0;
      const color = pct >= 90 ? "#fb7185" : pct >= 70 ? "#f59e0b" : "#34d399";
      tdQuota.innerHTML = `<span style="color:${color}">${used.toLocaleString()} / ${limit.toLocaleString()}</span> <span style="color:#64748b;font-size:0.75rem">(${pct}%)</span>`;
    } else {
      tdQuota.style.color = "#64748b";
      tdQuota.textContent = "-";
    }
    tr.appendChild(tdQuota);

    const tdStatus = document.createElement("td");
    const statusSpan = document.createElement("span");
    statusSpan.className = "tag tag-status-normal";
    statusSpan.title = badge.tip || "";
    statusSpan.style.background = badge.bg;
    statusSpan.style.color = badge.color;
    statusSpan.style.border = "none";
    statusSpan.textContent = badge.text;
    tdStatus.appendChild(statusSpan);
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

    tbody.appendChild(tr);
  });
  table.appendChild(tbody);
  wrap.appendChild(table);
  container.appendChild(wrap);

  document.getElementById("paginationInfo").textContent = `共 ${total} 条记录，第 ${currentPage}/${totalPages} 页`;
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
  const container = document.getElementById("paginationControls");
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
  const enabled = accounts.filter((a) => a.enabled).length;
  const abnormal = accounts.filter((a) => !a.enabled || a.status_code || (accountHealth[a.id] && !accountHealth[a.id].ok)).length;

  document.getElementById("totalAccounts").textContent = total;
  document.getElementById("enabledAccounts").textContent = enabled;
  document.getElementById("disabledAccounts").textContent = abnormal;

  // Attempt to update selected if element exists (it should)
  updateSelectedCount();

  const totalUsage = accounts.reduce((sum, acc) => sum + (acc.usage_daily || 0), 0);

  // Update sidebar footer
  const footerTotal = document.getElementById("footerTotal");
  if (footerTotal) footerTotal.textContent = total;

  const footerNormal = document.getElementById("footerNormal");
  if (footerNormal) footerNormal.textContent = enabled;

  const footerAbnormal = document.getElementById("footerAbnormal");
  if (footerAbnormal) footerAbnormal.textContent = abnormal;

  const footerUsage = document.getElementById("footerUsageText");
  if (footerUsage) {
    footerUsage.textContent = `${Math.floor(totalUsage)}`;
  }
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

  if (account) {
    title.textContent = "编辑账号";
    document.getElementById("accountId").value = account.id;
    document.getElementById("accountType").value = account.account_type || "orchids";
    document.getElementById("clientCookie").value = getAccountToken(account);
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
  applyTokenLabels(document.getElementById("accountType").value);
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
  const type = document.getElementById("accountType").value;
  const token = document.getElementById("clientCookie").value;
  const data = {
    account_type: type,
    agent_mode: document.getElementById("agentMode").value,
    weight: parseInt(document.getElementById("weight").value) || 1,
    enabled: document.getElementById("enabled").checked,
  };
  if (type === 'warp') {
    data.refresh_token = token;
  } else {
    data.client_cookie = token;
  }

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
      }
      return token.substring(0, 30) + '...';
    }
    return token;
  }
  if (type === 'warp' && getAccountToken(acc)) {
    const rt = getAccountToken(acc);
    return rt.length > 30 ? rt.substring(0, 10) + '...' + rt.substring(rt.length - 10) : rt;
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
  loadAccounts();
  const typeSelect = document.getElementById("accountType");
  if (typeSelect) {
    typeSelect.addEventListener("change", () => applyTokenLabels(typeSelect.value));
    applyTokenLabels(typeSelect.value);
  }
});
