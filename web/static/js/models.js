// Models management JavaScript

let models = [];
let currentModelChannel = "";
let modelSearchTerm = "";
let modelStatusFilter = "";
let modelPageSize = 50;
let modelCurrentPage = 1;

function modelChannels() {
  const defaultChannels = ["Orchids", "Warp", "Bolt", "Puter", "Grok"];
  return Array.from(new Set([...defaultChannels, ...models.map((m) => String(m.channel || "").trim()).filter(Boolean)])).sort();
}

function normalizeModelStatus(status) {
  if (status === true) return "available";
  const value = String(status || "").trim().toLowerCase();
  return value || "offline";
}

function statusMeta(status) {
  switch (normalizeModelStatus(status)) {
    case "available":
      return { label: "可用", bg: "rgba(34, 197, 94, 0.12)", color: "#4ade80", border: "rgba(34, 197, 94, 0.22)" };
    case "maintenance":
      return { label: "维护中", bg: "rgba(245, 158, 11, 0.12)", color: "#fbbf24", border: "rgba(245, 158, 11, 0.24)" };
    default:
      return { label: "已下线", bg: "rgba(148, 163, 184, 0.12)", color: "#cbd5e1", border: "rgba(148, 163, 184, 0.24)" };
  }
}

function getFilteredModels() {
  let filtered = models.slice();

  if (currentModelChannel) {
    filtered = filtered.filter((m) => String(m.channel || "").toLowerCase() === currentModelChannel.toLowerCase());
  }

  if (modelStatusFilter) {
    filtered = filtered.filter((m) => normalizeModelStatus(m.status) === modelStatusFilter);
  }

  if (modelSearchTerm) {
    const term = modelSearchTerm.toLowerCase();
    filtered = filtered.filter((m) => {
      return [
        m.model_id,
        m.name,
        m.channel,
        m.id,
      ].some((value) => String(value || "").toLowerCase().includes(term));
    });
  }

  filtered.sort((a, b) => {
    const sortDiff = Number(a.sort_order || 0) - Number(b.sort_order || 0);
    if (sortDiff !== 0) return sortDiff;
    return String(a.model_id || "").localeCompare(String(b.model_id || ""));
  });

  return filtered;
}

function updateModelSummary(filtered) {
  const available = filtered.filter((m) => normalizeModelStatus(m.status) === "available").length;
  const maintenance = filtered.filter((m) => normalizeModelStatus(m.status) === "maintenance").length;
  const offline = filtered.filter((m) => normalizeModelStatus(m.status) === "offline").length;
  const channelLabel = currentModelChannel || "全部";

  document.getElementById("totalModelCount").textContent = String(filtered.length);
  document.getElementById("availableModelCount").textContent = String(available);
  document.getElementById("maintenanceModelCount").textContent = String(maintenance);
  document.getElementById("offlineModelCount").textContent = String(offline);
  document.getElementById("currentChannelLabel").textContent = channelLabel;
  document.getElementById("currentChannelPill").textContent = channelLabel;

  const panelTitle = document.getElementById("modelsPanelTitle");
  if (panelTitle) {
    panelTitle.textContent = `${channelLabel} 模型列表`;
  }

  const panelHint = document.getElementById("modelsPanelHint");
  if (panelHint) {
    panelHint.textContent = filtered.length > 0
      ? `当前条件下共有 ${filtered.length} 条模型记录，可直接在列表中调整状态、默认项和排序。`
      : "当前筛选条件下没有命中的模型记录。";
  }
}

function renderChannelTabs() {
  const container = document.getElementById("modelPlatformFilters");
  if (!container) return;

  const channels = modelChannels();
  if (!currentModelChannel || !channels.includes(currentModelChannel)) {
    currentModelChannel = channels[0] || "";
  }

  container.innerHTML = "";
  channels.forEach((channel) => {
    const btn = document.createElement("button");
    btn.className = `tab-item ${currentModelChannel === channel ? "active" : ""}`.trim();
    btn.type = "button";
    btn.textContent = channel;
    btn.dataset.channel = encodeData(channel);
    btn.addEventListener("click", () => {
      filterModelsByChannel(channel);
    });
    container.appendChild(btn);
  });
}

function renderPagination(current, total) {
  const container = document.getElementById("modelsPaginationControls");
  if (!container) return;

  container.innerHTML = "";

  const appendButton = (label, page, disabled, active) => {
    const btn = document.createElement("button");
    btn.type = "button";
    btn.className = `btn ${active ? "btn-primary" : "btn-outline"}`;
    btn.disabled = disabled;
    btn.dataset.page = String(page);
    btn.textContent = label;
    btn.style.padding = "6px 12px";
    container.appendChild(btn);
  };

  appendButton("首页", 1, current === 1, false);
  appendButton("上一页", current - 1, current === 1, false);

  let startPage = Math.max(1, current - 2);
  let endPage = Math.min(total, startPage + 4);
  if (endPage-startPage < 4) {
    startPage = Math.max(1, endPage - 4);
  }

  for (let page = startPage; page <= endPage; page += 1) {
    appendButton(String(page), page, false, page === current);
  }

  appendButton("下一页", current + 1, current === total, false);
  appendButton("末页", total, current === total, false);

  container.onclick = (event) => {
    const btn = event.target.closest("button[data-page]");
    if (!btn || btn.disabled) return;
    const page = parseInt(btn.dataset.page || "", 10);
    if (Number.isNaN(page)) return;
    modelCurrentPage = page;
    renderModels();
  };
}

function renderModels() {
  const container = document.getElementById("modelsList");
  const filtered = getFilteredModels();
  updateModelSummary(filtered);

  const total = filtered.length;
  const totalPages = Math.max(1, Math.ceil(total / modelPageSize));
  if (modelCurrentPage > totalPages) modelCurrentPage = totalPages;
  if (modelCurrentPage < 1) modelCurrentPage = 1;

  const start = (modelCurrentPage - 1) * modelPageSize;
  const pageItems = filtered.slice(start, start + modelPageSize);
  const pageCount = document.getElementById("currentPageCount");
  if (pageCount) {
    pageCount.textContent = String(pageItems.length);
  }

  const paginationInfo = document.getElementById("modelsPaginationInfo");
  if (paginationInfo) {
    paginationInfo.textContent = `共 ${total} 条记录，第 ${modelCurrentPage}/${totalPages} 页`;
  }
  renderPagination(modelCurrentPage, totalPages);

  if (pageItems.length === 0) {
    container.innerHTML = `
      <div class="models-empty">
        <span class="models-empty-icon">◈</span>
        <p>当前筛选条件下暂无模型数据</p>
      </div>
    `;
    return;
  }

  const rows = pageItems.map((m) => {
    const status = statusMeta(m.status);
    const defaultBadge = m.is_default
      ? `<span class="models-default-badge">默认</span>`
      : `<button type="button" class="btn btn-outline models-default-btn" data-action="set-default" data-id="${encodeData(m.id)}">设为默认</button>`;

    return `
      <tr>
        <td class="col-id">${escapeHtml(m.id)}</td>
        <td>
          <span class="models-channel-tag">${escapeHtml(m.channel || "-")}</span>
        </td>
        <td>
          <div class="models-cell-main">
            <strong>${escapeHtml(m.name || m.model_id || "-")}</strong>
            <span class="models-cell-sub">显示名称</span>
          </div>
        </td>
        <td>
          <div class="models-cell-main">
            <span class="models-model-id">${escapeHtml(m.model_id || "-")}</span>
            <span class="models-cell-sub">实际请求使用的模型 ID</span>
          </div>
        </td>
        <td class="col-default">${defaultBadge}</td>
        <td class="col-status">
          <span class="models-status-badge" style="background:${status.bg};color:${status.color};border-color:${status.border};">${status.label}</span>
        </td>
        <td class="col-sort">${escapeHtml(String(m.sort_order ?? 0))}</td>
        <td>
          <label class="toggle" title="${normalizeModelStatus(m.status) === "available" ? "点击下线" : "点击启用"}">
            <input type="checkbox" data-action="toggle-status" data-id="${encodeData(m.id)}" ${normalizeModelStatus(m.status) === "available" ? "checked" : ""} />
            <span class="toggle-slider"></span>
          </label>
        </td>
        <td class="col-actions">
          <div class="models-actions">
            <i class="action-icon" data-action="edit" data-id="${encodeData(m.id)}" title="编辑">✏️</i>
            <i class="action-icon" data-action="delete" data-id="${encodeData(m.id)}" title="删除" style="color:#ef4444;">🗑️</i>
          </div>
        </td>
      </tr>
    `;
  }).join("");

  container.innerHTML = `
    <div class="table-wrap models-table-wrap">
      <table class="models-table">
        <thead>
          <tr>
            <th class="col-id">ID</th>
            <th>渠道</th>
            <th>显示名称</th>
            <th>模型 ID</th>
            <th class="col-default">默认</th>
            <th class="col-status">状态</th>
            <th class="col-sort">排序</th>
            <th>启用</th>
            <th class="col-actions">操作</th>
          </tr>
        </thead>
        <tbody>${rows}</tbody>
      </table>
    </div>
  `;

  container.onclick = (event) => {
    const target = event.target.closest("[data-action]");
    if (!target || !container.contains(target)) return;
    const action = target.dataset.action;
    const id = decodeData(target.dataset.id || "");
    if (!id) return;
    if (action === "edit") editModel(id);
    if (action === "delete") deleteModel(id);
    if (action === "set-default") setDefaultModel(id);
  };

  container.onchange = (event) => {
    const target = event.target;
    if (!(target instanceof HTMLInputElement)) return;
    if (target.dataset.action !== "toggle-status") return;
    const id = decodeData(target.dataset.id || "");
    if (!id) return;
    toggleModelStatus(id, target.checked);
  };
}

async function loadModels() {
  try {
    const res = await fetch("/api/models");
    if (res.status === 401) {
      window.location.href = "./login.html";
      return;
    }
    models = await res.json() || [];
    renderChannelTabs();
    updateModelChannelOptions();
    renderModels();
  } catch (err) {
    showToast("加载模型失败", "error");
  }
}

async function setDefaultModel(id) {
  const model = models.find((item) => item.id === id);
  if (!model) return;

  try {
    const updatedModel = { ...model, is_default: true };
    const res = await fetch(`/api/models/${id}`, {
      method: "PUT",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(updatedModel),
    });
    if (!res.ok) throw new Error(await res.text());
    showToast("设为默认成功");
    await loadModels();
  } catch (err) {
    showToast(`设置失败: ${err.message}`, "error");
  }
}

function filterModelsByChannel(channel) {
  currentModelChannel = channel;
  modelCurrentPage = 1;
  document.querySelectorAll("#modelPlatformFilters .tab-item").forEach((btn) => {
    btn.classList.toggle("active", btn.textContent === channel);
  });
  renderModels();
}

function updateModelChannelOptions() {
  const select = document.getElementById("modelChannel");
  if (!select) return;

  const previous = select.value;
  select.innerHTML = "";
  modelChannels().forEach((channel) => {
    const option = document.createElement("option");
    option.value = channel;
    option.textContent = channel;
    select.appendChild(option);
  });

  if (previous && Array.from(select.options).some((option) => option.value === previous)) {
    select.value = previous;
  } else if (currentModelChannel && Array.from(select.options).some((option) => option.value === currentModelChannel)) {
    select.value = currentModelChannel;
  }
}

function openModelModal(model = null) {
  const modal = document.getElementById("modelModal");
  const title = document.getElementById("modelModalTitle");
  const form = document.getElementById("modelForm");

  updateModelChannelOptions();

  const setSelectValue = (el, value) => {
    if (!el) return;
    const raw = value === null || value === undefined ? "" : String(value);
    el.value = raw;
    if (el.tagName === "SELECT" && el.value !== raw) {
      el.selectedIndex = 0;
    }
  };

  if (model) {
    title.textContent = "编辑模型";
    document.getElementById("modelId").value = model.id;
    setSelectValue(document.getElementById("modelChannel"), model.channel);
    document.getElementById("modelModelId").value = model.model_id;
    document.getElementById("modelName").value = model.name;
    document.getElementById("modelSortOrder").value = model.sort_order;
    setSelectValue(document.getElementById("modelStatus"), normalizeModelStatus(model.status));
    document.getElementById("modelIsDefault").checked = !!model.is_default;
  } else {
    title.textContent = "添加模型";
    form.reset();
    document.getElementById("modelId").value = "";
    setSelectValue(document.getElementById("modelChannel"), currentModelChannel || "Orchids");
    document.getElementById("modelSortOrder").value = "0";
    setSelectValue(document.getElementById("modelStatus"), "available");
  }

  modal.classList.add("active");
  modal.style.display = "flex";
}

function closeModelModal() {
  const modal = document.getElementById("modelModal");
  modal.classList.remove("active");
  modal.style.display = "none";
}

async function saveModel(event) {
  event.preventDefault();

  const id = document.getElementById("modelId").value;
  const data = {
    channel: document.getElementById("modelChannel").value,
    model_id: document.getElementById("modelModelId").value,
    name: document.getElementById("modelName").value,
    sort_order: parseInt(document.getElementById("modelSortOrder").value, 10) || 0,
    status: document.getElementById("modelStatus").value,
    is_default: document.getElementById("modelIsDefault").checked,
  };

  if (id) {
    data.id = id;
  }

  try {
    const url = id ? `/api/models/${id}` : "/api/models";
    const method = id ? "PUT" : "POST";
    const res = await fetch(url, {
      method,
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(data),
    });
    if (!res.ok) throw new Error(await res.text());
    closeModelModal();
    await loadModels();
    showToast("保存成功");
  } catch (err) {
    showToast(`保存失败: ${err.message}`, "error");
  }
}

function editModel(id) {
  const model = models.find((item) => item.id === id);
  if (model) openModelModal(model);
}

async function toggleModelStatus(id, enabled) {
  const model = models.find((item) => item.id === id);
  if (!model) return;

  try {
    const updatedModel = { ...model, status: enabled ? "available" : "offline" };
    const res = await fetch(`/api/models/${id}`, {
      method: "PUT",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(updatedModel),
    });
    if (!res.ok) throw new Error(await res.text());
    showToast(enabled ? "模型已启用" : "模型已禁用");
    await loadModels();
  } catch (err) {
    showToast(`操作失败: ${err.message}`, "error");
  }
}

async function deleteModel(id) {
  if (!confirm("确定要删除这个模型吗？")) return;
  try {
    const res = await fetch(`/api/models/${id}`, { method: "DELETE" });
    if (!res.ok) throw new Error(await res.text());
    showToast("删除成功");
    await loadModels();
  } catch (err) {
    showToast(`删除失败: ${err.message}`, "error");
  }
}

function escapeHtml(text) {
  const div = document.createElement("div");
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

document.addEventListener("DOMContentLoaded", () => {
  const searchInput = document.getElementById("modelSearchInput");
  const statusFilter = document.getElementById("modelStatusFilter");
  const pageSize = document.getElementById("modelPageSize");

  if (searchInput) {
    searchInput.addEventListener("input", (event) => {
      modelSearchTerm = String(event.target.value || "").trim();
      modelCurrentPage = 1;
      renderModels();
    });
  }

  if (statusFilter) {
    statusFilter.addEventListener("change", (event) => {
      modelStatusFilter = String(event.target.value || "").trim();
      modelCurrentPage = 1;
      renderModels();
    });
  }

  if (pageSize) {
    pageSize.addEventListener("change", (event) => {
      modelPageSize = parseInt(event.target.value || "50", 10) || 50;
      modelCurrentPage = 1;
      renderModels();
    });
  }

  loadModels();
});
