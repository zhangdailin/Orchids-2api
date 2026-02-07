// Models management JavaScript

let models = [];
let currentModelChannel = '';

// Load models from API
async function loadModels() {
  try {
    const res = await fetch("/api/models");
    if (res.status === 401) {
      window.location.href = "./login.html";
      return;
    }
    models = await res.json() || [];
    renderChannelTabs();
    renderModels();
  } catch (err) {
    showToast("加载模型失败", "error");
  }
}

// Render channel filter tabs
function renderChannelTabs() {
  const container = document.getElementById("modelPlatformFilters");
  if (!container) return;

  const defaultChannels = ["Orchids", "Warp"];
  const channels = new Set([...defaultChannels, ...models.map(m => m.channel)]);
  const sorted = Array.from(channels).sort();
  const tabs = [...sorted];

  if (currentModelChannel === '' || !tabs.includes(currentModelChannel)) {
    currentModelChannel = tabs.length > 0 ? tabs[0] : '';
  }

  container.innerHTML = "";
  tabs.forEach(channel => {
    const label = String(channel || "");
    const isActive = currentModelChannel === label;
    const btn = document.createElement("button");
    btn.className = `tab-item ${isActive ? 'active' : ''}`.trim();
    btn.dataset.channel = encodeURIComponent(label);
    btn.textContent = label;
    btn.addEventListener("click", () => {
      const raw = btn.dataset.channel ? decodeURIComponent(btn.dataset.channel) : "";
      filterModelsByChannel(raw);
    });
    container.appendChild(btn);
  });
}

// Render models table
function renderModels() {
  const container = document.getElementById("modelsList");
  let filtered = models;
  if (currentModelChannel) {
    filtered = models.filter(m => m.channel.toLowerCase() === currentModelChannel.toLowerCase());
  }

  // Sort: SortOrder ascending
  filtered.sort((a, b) => {
    return a.sort_order - b.sort_order;
  });

  document.getElementById("totalModelCount").textContent = filtered.length;

  if (filtered.length === 0) {
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
    icon.textContent = "💎";
    const text = document.createElement("p");
    text.textContent = `暂无${currentModelChannel ? " " + currentModelChannel : ""} 模型数据`;
    empty.appendChild(icon);
    empty.appendChild(text);
    container.appendChild(empty);
    return;
  }

  container.innerHTML = "";
  const wrapper = document.createElement("div");
  wrapper.style.maxWidth = "100%";

  filtered.forEach((m) => {
    const card = document.createElement("div");
    card.style.padding = "24px";
    card.style.borderRadius = "12px";
    card.style.border = `1px solid ${m.is_default ? "#3b82f6" : "#e2e8f0"}`;
    card.style.background = "#fff";
    card.style.boxShadow = "0 1px 3px rgba(0,0,0,0.1)";
    card.style.marginBottom = "16px";

    const row = document.createElement("div");
    row.style.display = "flex";
    row.style.alignItems = "flex-start";
    row.style.justifyContent = "space-between";
    row.style.marginBottom = "12px";

    const left = document.createElement("div");
    left.style.flex = "1";

    const titleRow = document.createElement("div");
    titleRow.style.display = "flex";
    titleRow.style.alignItems = "center";
    titleRow.style.gap = "12px";
    titleRow.style.marginBottom = "8px";

    const h3 = document.createElement("h3");
    h3.style.fontWeight = "600";
    h3.style.fontSize = "1.1rem";
    h3.style.color = "#1e293b";
    h3.style.margin = "0";
    h3.textContent = m.name || "";

    const badge = document.createElement("span");
    badge.className = `tag badge-${sanitizeClassName(m.channel)}`;
    badge.style.fontSize = "0.75rem";
    badge.textContent = m.channel || "";

    titleRow.appendChild(h3);
    titleRow.appendChild(badge);

    if (m.is_default) {
      const def = document.createElement("span");
      def.style.background = "#dbeafe";
      def.style.color = "#1d4ed8";
      def.style.padding = "2px 8px";
      def.style.borderRadius = "4px";
      def.style.fontSize = "0.75rem";
      def.style.fontWeight = "500";
      def.textContent = "默认";
      titleRow.appendChild(def);
    }

    const modelId = document.createElement("div");
    modelId.style.fontFamily = "monospace";
    modelId.style.color = "#64748b";
    modelId.style.fontSize = "0.9rem";
    modelId.style.marginBottom = "8px";
    modelId.textContent = m.model_id || "";

    const desc = document.createElement("div");
    desc.style.color = "#64748b";
    desc.style.fontSize = "0.85rem";
    desc.style.lineHeight = "1.5";
    desc.textContent = `该模型用于 ${m.channel || ""} 渠道的 API 调用${m.is_default ? "，作为默认模型优先使用" : ""}`;

    left.appendChild(titleRow);
    left.appendChild(modelId);
    left.appendChild(desc);

    const right = document.createElement("div");
    right.style.display = "flex";
    right.style.alignItems = "center";
    right.style.gap = "12px";

    const label = document.createElement("label");
    label.className = "toggle";
    label.style.transform = "scale(0.9)";
    const checkbox = document.createElement("input");
    checkbox.type = "checkbox";
    checkbox.checked = m.status === "available" || m.status === true;
    checkbox.dataset.action = "toggle-status";
    checkbox.dataset.id = encodeData(m.id);
    const slider = document.createElement("span");
    slider.className = "toggle-slider";
    label.appendChild(checkbox);
    label.appendChild(slider);

    const actionWrap = document.createElement("div");
    actionWrap.style.display = "flex";
    actionWrap.style.gap = "8px";

    const edit = document.createElement("i");
    edit.className = "action-icon";
    edit.style.fontSize = "1.2rem";
    edit.style.cursor = "pointer";
    edit.dataset.action = "edit";
    edit.dataset.id = encodeData(m.id);
    edit.title = "编辑";
    edit.textContent = "✏️";

    const del = document.createElement("i");
    del.className = "action-icon";
    del.style.fontSize = "1.2rem";
    del.style.cursor = "pointer";
    del.style.color = "#ef4444";
    del.dataset.action = "delete";
    del.dataset.id = encodeData(m.id);
    del.title = "删除";
    del.textContent = "🗑️";

    actionWrap.appendChild(edit);
    actionWrap.appendChild(del);

    right.appendChild(label);
    right.appendChild(actionWrap);

    row.appendChild(left);
    row.appendChild(right);
    card.appendChild(row);

    if (!m.is_default) {
      const btn = document.createElement("button");
      btn.className = "btn btn-outline";
      btn.style.padding = "6px 16px";
      btn.style.fontSize = "0.85rem";
      btn.dataset.action = "set-default";
      btn.dataset.id = encodeData(m.id);
      btn.textContent = "设为默认模型";
      card.appendChild(btn);
    }

    wrapper.appendChild(card);
  });

  const tip = document.createElement("div");
  tip.style.marginTop = "24px";
  tip.style.padding = "16px";
  tip.style.background = "#eff6ff";
  tip.style.border = "1px solid #bfdbfe";
  tip.style.borderRadius = "8px";
  tip.style.color = "#1e40af";
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
    "• 默认模型将优先用于 API 调用",
    "• 禁用的模型不会出现在可用模型列表中",
    "• 排序值越小，在列表中越靠前",
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
  wrapper.appendChild(tip);
  container.appendChild(wrapper);

  container.onclick = (e) => {
    const target = e.target.closest("[data-action]");
    if (!target || !container.contains(target)) return;
    const action = target.dataset.action;
    const id = decodeData(target.dataset.id || "");
    if (!id) return;
    if (action === "edit") editModel(id);
    if (action === "delete") deleteModel(id);
    if (action === "set-default") setDefaultModel(id);
  };

  container.onchange = (e) => {
    const target = e.target;
    if (!(target instanceof HTMLInputElement)) return;
    if (target.dataset.action !== "toggle-status") return;
    const id = decodeData(target.dataset.id || "");
    if (!id) return;
    toggleModelStatus(id, target.checked);
  };
}

// Set model as default
async function setDefaultModel(id) {
  const model = models.find(m => m.id === id);
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
    loadModels();
  } catch (err) {
    showToast("设置失败: " + err.message, "error");
  }
}

// Filter models by channel
function filterModelsByChannel(channel) {
  currentModelChannel = channel;
  document.querySelectorAll("#modelPlatformFilters .tab-item").forEach(btn => {
    btn.classList.toggle("active", btn.textContent === channel);
  });

  const header = document.querySelector("#modelsHeader h1");
  const sub = document.querySelector("#modelsHeader p");

  document.getElementById("modelsListWrapper").style.display = 'block';
  document.getElementById("totalModelCount").parentElement.style.visibility = 'visible';
  if (header) header.textContent = "模型管理";
  if (sub) sub.textContent = "管理各渠道的可用模型列表";
  renderModels();
}

// Open model modal
function openModelModal(model = null) {
  const modal = document.getElementById("modelModal");
  const title = document.getElementById("modelModalTitle");
  const form = document.getElementById("modelForm");

  const setSelectValue = (el, value) => {
    if (!el) return;
    const raw = value === null || value === undefined ? "" : String(value);
    el.value = raw;
    // 如果 value 不在 option 列表里，浏览器会显示空白。
    // 这里兜底为第一个 option，避免“白框像没值”。
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
    setSelectValue(document.getElementById("modelStatus"), model.status);
    document.getElementById("modelIsDefault").checked = model.is_default;
  } else {
    title.textContent = "添加模型";
    form.reset();
    document.getElementById("modelId").value = "";
    setSelectValue(document.getElementById("modelChannel"), "Orchids");
    document.getElementById("modelSortOrder").value = "0";
    setSelectValue(document.getElementById("modelStatus"), "available");
  }
  modal.classList.add("active");
  modal.style.display = "flex";
}

// Close model modal
function closeModelModal() {
  const modal = document.getElementById("modelModal");
  modal.classList.remove("active");
  modal.style.display = "none";
}

// Save model
async function saveModel(e) {
  e.preventDefault();
  const id = document.getElementById("modelId").value;
  const data = {
    channel: document.getElementById("modelChannel").value,
    model_id: document.getElementById("modelModelId").value,
    name: document.getElementById("modelName").value,
    sort_order: parseInt(document.getElementById("modelSortOrder").value) || 0,
    status: document.getElementById("modelStatus").value,
    is_default: document.getElementById("modelIsDefault").checked
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
    loadModels();
    showToast("保存成功");
  } catch (err) {
    showToast("保存失败: " + err.message, "error");
  }
}

// Edit model
function editModel(id) {
  const model = models.find(m => m.id === id);
  if (model) openModelModal(model);
}

// Toggle model status
async function toggleModelStatus(id, enabled) {
  const model = models.find(m => m.id === id);
  if (!model) return;

  try {
    const updatedModel = { ...model, status: enabled ? 'available' : 'offline' };
    const res = await fetch(`/api/models/${id}`, {
      method: "PUT",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(updatedModel),
    });
    if (!res.ok) throw new Error(await res.text());
    showToast(enabled ? "模型已启用" : "模型已禁用");
    loadModels();
  } catch (err) {
    showToast("操作失败: " + err.message, "error");
  }
}

// Delete model
async function deleteModel(id) {
  if (!confirm("确定要删除这个模型吗？")) return;
  try {
    const res = await fetch(`/api/models/${id}`, { method: "DELETE" });
    if (!res.ok) throw new Error(await res.text());
    showToast("删除成功");
    loadModels();
  } catch (err) {
    showToast("删除失败: " + err.message, "error");
  }
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

function sanitizeClassName(text) {
  if (text === null || text === undefined) return "";
  return String(text).toLowerCase().replace(/[^a-z0-9_-]+/g, "-");
}

// Load models on page load
document.addEventListener('DOMContentLoaded', () => {
  loadModels();
});
