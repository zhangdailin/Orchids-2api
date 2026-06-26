// Models management JavaScript

let models = [];
let currentModelChannel = "";
let modelSearchTerm = "";
let modelStatusFilter = "";
let modelPageSize = 50;
let modelCurrentPage = 1;
let modelRefreshInFlight = false;
let modelDeleteOfflineInFlight = false;
let modelRefreshResults = {};
let modelRefreshConcurrency = 4;

function modelChannels() {
  const defaultChannels = ["Orchids", "Warp", "Puter", "Grok"];
  const seen = new Set();
  const ordered = [];

  defaultChannels.forEach((channel) => {
    if (seen.has(channel.toLowerCase())) return;
    seen.add(channel.toLowerCase());
    ordered.push(channel);
  });

  models
    .map((m) => String(m.channel || "").trim())
    .filter(Boolean)
    .sort((a, b) => a.localeCompare(b))
    .forEach((channel) => {
      const key = channel.toLowerCase();
      if (seen.has(key)) return;
      seen.add(key);
      ordered.push(channel);
    });

  return ordered;
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

function sortModels(list) {
  return list.sort((a, b) => {
    const sortDiff = Number(a.sort_order || 0) - Number(b.sort_order || 0);
    if (sortDiff !== 0) return sortDiff;
    return String(a.model_id || "").localeCompare(String(b.model_id || ""));
  });
}

function getChannelScopedModels() {
  let scoped = models.slice();

  if (currentModelChannel) {
    scoped = scoped.filter((m) => String(m.channel || "").toLowerCase() === currentModelChannel.toLowerCase());
  }

  return sortModels(scoped);
}

function getFilteredModels() {
  let filtered = getChannelScopedModels().slice();

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

  return filtered;
}

function updateModelSummary(channelModels, filtered) {
  const channelLabel = currentModelChannel || "全部";

  const totalModelCount = document.getElementById("totalModelCount");
  if (totalModelCount) {
    totalModelCount.textContent = String(filtered.length);
  }

  const currentChannelPill = document.getElementById("currentChannelPill");
  if (currentChannelPill) {
    currentChannelPill.textContent = channelLabel;
  }

  const filterMeta = document.getElementById("modelsFilterMeta");
  if (filterMeta) {
    filterMeta.textContent = `当前渠道共 ${channelModels.length} 条，筛选后 ${filtered.length} 条。`;
  }

  const panelTitle = document.getElementById("modelsPanelTitle");
  if (panelTitle) {
    panelTitle.textContent = `${channelLabel} 动作区`;
  }

  const panelHint = document.getElementById("modelsPanelHint");
  if (panelHint) {
    panelHint.textContent = filtered.length > 0
      ? `当前筛选结果共有 ${filtered.length} 条，可直接在表格中编辑、删除或启停。默认模型请在编辑弹窗里维护。`
      : "当前筛选条件下没有命中的模型记录。";
  }
}

function refreshChannelKey(channel) {
  return String(channel || "").trim().toLowerCase();
}

function sortTextValues(values) {
  return values
    .map((value) => String(value || "").trim())
    .filter(Boolean)
    .sort((a, b) => a.localeCompare(b, undefined, { numeric: true, sensitivity: "base" }));
}

function normalizeModelRefreshResult(data, fallbackChannel) {
  return {
    channel: String(data.channel || fallbackChannel || "").trim(),
    source: String(data.source || "").trim(),
    concurrency: Number(data.concurrency ?? modelRefreshConcurrency),
    discovered: Number(data.discovered ?? 0),
    verified: Number(data.verified ?? 0),
    added: Number(data.added ?? 0),
    updated: Number(data.updated ?? 0),
    deleted: Number(data.deleted ?? 0),
    offline: Number(data.offline ?? 0),
    deletedModelIDs: sortTextValues(Array.isArray(data.deleted_model_ids) ? data.deleted_model_ids : []),
    offlineModelIDs: sortTextValues(Array.isArray(data.offline_model_ids) ? data.offline_model_ids : []),
  };
}

function modelRefreshSourceLabel(source) {
  const value = String(source || "").trim();
  if (!value) return "未知来源";
  const labels = {
    upstream_api: "账号上游 API",
    public_page: "Orchids 公开页面",
    puter_public_models: "Puter 公开模型 API",
    puter_public_models_test_mode: "Puter 公开 API + 账号验证",
    puter_public_models_unverified: "Puter 公开 API",
    grok_app_chat_static: "Grok App Chat 模型表",
  };
  if (labels[value]) return labels[value];
  if (value.startsWith("warp_graphql")) return "Warp 账号 GraphQL";
  return value;
}

function renderModelRefreshSummary() {
  const summary = document.getElementById("modelsRefreshSummary");
  const title = document.getElementById("modelsRefreshSummaryTitle");
  const meta = document.getElementById("modelsRefreshSummaryMeta");
  const statGrid = document.getElementById("modelsRefreshStatGrid");
  const deletedBlock = document.getElementById("modelsRefreshDeletedBlock");
  const deletedList = document.getElementById("modelsRefreshDeletedList");
  if (!summary || !title || !meta || !statGrid || !deletedBlock || !deletedList) return;

  const channel = currentModelChannel || modelChannels()[0] || "";
  const result = modelRefreshResults[refreshChannelKey(channel)];
  if (!result) {
    summary.hidden = true;
    statGrid.innerHTML = "";
    deletedList.innerHTML = "";
    deletedBlock.hidden = true;
    return;
  }

  summary.hidden = false;
  title.textContent = `${result.channel || channel} 最近一次刷新结果`;
  meta.textContent = `来源：${modelRefreshSourceLabel(result.source)}。并发数 ${result.concurrency || modelRefreshConcurrency}。刷新只会补充新模型，已有模型的状态、名称、排序和默认项会保持不变。`;

  const stats = [
    { label: "发现", value: result.discovered },
    { label: "同步", value: result.verified },
    { label: "新增", value: result.added },
    { label: "更新", value: result.updated },
    { label: "下线", value: result.offline },
  ];
  statGrid.innerHTML = stats.map((item) => `
    <div class="models-refresh-stat">
      <div class="models-refresh-stat-label">${escapeHtml(item.label)}</div>
      <div class="models-refresh-stat-value">${escapeHtml(String(item.value))}</div>
    </div>
  `).join("");

  const deletedItems = sortTextValues([...(result.offlineModelIDs || []), ...(result.deletedModelIDs || [])]);
  deletedBlock.hidden = deletedItems.length === 0;
  deletedList.innerHTML = deletedItems.map((item) => `
    <span class="models-refresh-deleted-item">${escapeHtml(item)}</span>
  `).join("");
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

  updateRefreshButton();
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
  const channelModels = getChannelScopedModels();
  const filtered = getFilteredModels();
  updateModelSummary(channelModels, filtered);
  renderModelRefreshSummary();

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
      <div class="models-empty empty-state-panel">
        <span class="models-empty-icon empty-state-mark">◈</span>
        <p>当前筛选条件下暂无模型数据</p>
      </div>
    `;
    return;
  }

  if (window.matchMedia("(max-width: 640px)").matches) {
    renderModelsMobile(container, pageItems);
    return;
  }

  const rows = pageItems.map((m) => {
    const status = statusMeta(m.status);
    const defaultBadge = m.is_default ? `<span class="models-default-badge">默认</span>` : "";

    return `
      <tr>
        <td class="col-model">
          <div class="models-cell-main">
            <div class="models-cell-title">
              <strong>${escapeHtml(m.name || m.model_id || "-")}</strong>
              ${defaultBadge}
            </div>
            <span class="models-model-id">${escapeHtml(m.model_id || "-")}</span>
            <span class="models-cell-sub">排序 ${escapeHtml(String(m.sort_order ?? 0))}</span>
          </div>
        </td>
        <td class="col-status">
          <span class="models-status-badge" style="background:${status.bg};color:${status.color};border-color:${status.border};">${status.label}</span>
        </td>
        <td class="col-sort">${escapeHtml(String(m.sort_order ?? 0))}</td>
        <td class="col-toggle">
          <label class="toggle" title="${normalizeModelStatus(m.status) === "available" ? "点击下线" : "点击启用"}">
            <input type="checkbox" data-action="toggle-status" data-id="${encodeData(m.id)}" ${normalizeModelStatus(m.status) === "available" ? "checked" : ""} />
            <span class="toggle-slider"></span>
          </label>
        </td>
        <td class="col-actions">
          <div class="models-actions">
            <button type="button" class="btn btn-outline models-action-btn" data-action="edit" data-id="${encodeData(m.id)}">编辑</button>
            <button type="button" class="btn btn-outline models-action-btn models-action-btn-danger" data-action="delete" data-id="${encodeData(m.id)}">删除</button>
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
            <th class="col-model">模型</th>
            <th class="col-status">状态</th>
            <th class="col-sort">排序</th>
            <th class="col-toggle">启用</th>
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

function renderModelsMobile(container, pageItems) {
  const cards = pageItems.map((m) => {
    const status = statusMeta(m.status);
    const defaultBadge = m.is_default ? `<span class="models-default-badge">默认</span>` : "";
    return `
      <article class="models-mobile-card">
        <div class="models-mobile-head">
          <div class="models-cell-title">
            <strong>${escapeHtml(m.name || m.model_id || "-")}</strong>
            ${defaultBadge}
          </div>
          <span class="models-status-badge" style="background:${status.bg};color:${status.color};border-color:${status.border};">${status.label}</span>
        </div>
        <div class="models-model-id">${escapeHtml(m.model_id || "-")}</div>
        <div class="models-mobile-grid">
          <div class="models-mobile-item">
            <span class="models-mobile-label">渠道</span>
            <span>${escapeHtml(m.channel || "-")}</span>
          </div>
          <div class="models-mobile-item">
            <span class="models-mobile-label">排序</span>
            <span>${escapeHtml(String(m.sort_order ?? 0))}</span>
          </div>
          <div class="models-mobile-item">
            <span class="models-mobile-label">启用</span>
            <label class="toggle" title="${normalizeModelStatus(m.status) === "available" ? "点击下线" : "点击启用"}">
              <input type="checkbox" data-action="toggle-status" data-id="${encodeData(m.id)}" ${normalizeModelStatus(m.status) === "available" ? "checked" : ""} />
              <span class="toggle-slider"></span>
            </label>
          </div>
        </div>
        <div class="models-mobile-actions">
          <button type="button" class="btn btn-outline models-action-btn" data-action="edit" data-id="${encodeData(m.id)}">编辑</button>
          <button type="button" class="btn btn-outline models-action-btn models-action-btn-danger" data-action="delete" data-id="${encodeData(m.id)}">删除</button>
        </div>
      </article>
    `;
  }).join("");

  container.innerHTML = `<div class="models-mobile-list">${cards}</div>`;

  container.onclick = (event) => {
    const target = event.target.closest("[data-action]");
    if (!target || !container.contains(target)) return;
    const action = target.dataset.action;
    const id = decodeData(target.dataset.id || "");
    if (!id) return;
    if (action === "edit") editModel(id);
    if (action === "delete") deleteModel(id);
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
    updateRefreshButton();
  } catch (err) {
    showToast("加载模型失败", "error");
  }
}

function filterModelsByChannel(channel) {
  currentModelChannel = channel;
  modelCurrentPage = 1;
  document.querySelectorAll("#modelPlatformFilters .tab-item").forEach((btn) => {
    btn.classList.toggle("active", btn.textContent === channel);
  });
  renderModels();
  updateRefreshButton();
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

function updateRefreshButton() {
  const button = document.getElementById("refreshModelsButton");
  const deleteButton = document.getElementById("deleteOfflineModelsButton");

  const channel = currentModelChannel || modelChannels()[0] || "";
  if (button) {
    button.disabled = modelRefreshInFlight || modelDeleteOfflineInFlight || !channel;
    if (!channel) {
      button.textContent = "刷新当前渠道";
    } else {
      button.textContent = modelRefreshInFlight
        ? `正在刷新 ${channel}...`
        : `刷新 ${channel} 列表`;
    }
  }
  if (deleteButton) {
    const offlineCount = getChannelScopedModels()
      .filter((m) => normalizeModelStatus(m.status) === "offline")
      .length;
    deleteButton.disabled = modelRefreshInFlight || modelDeleteOfflineInFlight || !channel || offlineCount === 0;
    deleteButton.textContent = modelDeleteOfflineInFlight
      ? "正在删除..."
      : `删除已下线${offlineCount > 0 ? ` (${offlineCount})` : ""}`;
  }
}

async function refreshModelsForCurrentChannel() {
  const channel = currentModelChannel || modelChannels()[0] || "";
  if (!channel || modelRefreshInFlight) return;

  modelRefreshInFlight = true;
  updateRefreshButton();

  try {
    const res = await fetch("/api/models/refresh", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ channel, concurrency: modelRefreshConcurrency }),
    });

    const raw = await res.text();
    let data = {};
    if (raw) {
      try {
        data = JSON.parse(raw);
      } catch (err) {
        data = { message: raw };
      }
    }

    if (!res.ok) {
      throw new Error(data.message || raw || "刷新失败");
    }

    const normalized = normalizeModelRefreshResult(data, channel);
    if (normalized.channel) {
      modelRefreshResults[refreshChannelKey(normalized.channel)] = normalized;
    }

    await loadModels();

    const parts = [
      `并发 ${data.concurrency ?? modelRefreshConcurrency}`,
      `发现 ${data.discovered ?? 0}`,
      `同步 ${data.verified ?? 0}`,
      `新增 ${data.added ?? 0}`,
      `更新 ${data.updated ?? 0}`,
      `删除 ${data.deleted ?? 0}`,
    ];
    const hasDeleted = normalized.deletedModelIDs.length > 0;
    showToast(`${channel} 刷新完成：${parts.join("，")}${hasDeleted ? "。已在下方列出删除模型" : ""}`);
  } catch (err) {
    showToast(`刷新失败: ${err.message}`, "error");
  } finally {
    modelRefreshInFlight = false;
    updateRefreshButton();
  }
}

async function deleteOfflineModelsForCurrentChannel() {
  const channel = currentModelChannel || modelChannels()[0] || "";
  if (!channel || modelDeleteOfflineInFlight) return;

  const targets = getChannelScopedModels()
    .filter((m) => normalizeModelStatus(m.status) === "offline");
  if (targets.length === 0) {
    showToast(`${channel} 没有已下线模型`, "info");
    updateRefreshButton();
    return;
  }
  if (!confirm(`确定删除 ${channel} 渠道的 ${targets.length} 个已下线模型吗？`)) {
    return;
  }

  modelDeleteOfflineInFlight = true;
  updateRefreshButton();
  let deleted = 0;
  const failures = [];
  try {
    for (const model of targets) {
      try {
        const res = await fetch(`/api/models/${encodeURIComponent(model.id)}`, { method: "DELETE" });
        if (!res.ok) throw new Error(await res.text());
        deleted += 1;
      } catch (err) {
        failures.push(`${model.model_id || model.id}: ${err.message || err}`);
      }
    }
    await loadModels();
    if (failures.length > 0) {
      showToast(`已删除 ${deleted} 个，失败 ${failures.length} 个`, "error");
      console.warn("delete offline models failures", failures);
    } else {
      showToast(`已删除 ${deleted} 个已下线模型`);
    }
  } finally {
    modelDeleteOfflineInFlight = false;
    updateRefreshButton();
  }
}



document.addEventListener("DOMContentLoaded", () => {
  const searchInput = document.getElementById("modelSearchInput");
  const statusFilter = document.getElementById("modelStatusFilter");
  const pageSize = document.getElementById("modelPageSize");
  const refreshConcurrency = document.getElementById("modelRefreshConcurrency");

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

  if (refreshConcurrency) {
    modelRefreshConcurrency = parseInt(refreshConcurrency.value || "4", 10) || 4;
    refreshConcurrency.addEventListener("change", (event) => {
      modelRefreshConcurrency = parseInt(event.target.value || "4", 10) || 4;
    });
  }

  loadModels();
});
