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

  container.innerHTML = tabs.map(channel => {
    const label = channel;
    const isActive = currentModelChannel === label;
    return `<button class="tab-item ${isActive ? 'active' : ''}" onclick="filterModelsByChannel('${label}')">${label}</button>`;
  }).join("");
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
    container.innerHTML = `
      <div class="empty-state" style="display: flex; flex-direction: column; align-items: center; justify-content: center; height: 300px; color: #94a3b8;">
        <span style="font-size: 3rem; margin-bottom: 16px;">💎</span>
        <p>暂无${currentModelChannel ? ' ' + currentModelChannel : ''} 模型数据</p>
      </div>`;
    return;
  }

  const cards = filtered.map(m => `
    <div style="padding: 24px; border-radius: 12px; border: 1px solid ${m.is_default ? '#3b82f6' : '#e2e8f0'}; background: #fff; box-shadow: 0 1px 3px rgba(0,0,0,0.1); margin-bottom: 16px;">
      <div style="display: flex; align-items: flex-start; justify-content: space-between; margin-bottom: 12px;">
        <div style="flex: 1;">
          <div style="display: flex; align-items: center; gap: 12px; margin-bottom: 8px;">
            <h3 style="font-weight: 600; font-size: 1.1rem; color: #1e293b; margin: 0;">${escapeHtml(m.name)}</h3>
            <span class="tag badge-${m.channel.toLowerCase()}" style="font-size: 0.75rem;">${escapeHtml(m.channel)}</span>
            ${m.is_default ? '<span style="background: #dbeafe; color: #1d4ed8; padding: 2px 8px; border-radius: 4px; font-size: 0.75rem; font-weight: 500;">默认</span>' : ''}
          </div>
          <div style="font-family: monospace; color: #64748b; font-size: 0.9rem; margin-bottom: 8px;">${escapeHtml(m.model_id)}</div>
          <div style="color: #64748b; font-size: 0.85rem; line-height: 1.5;">
            该模型用于 ${m.channel} 渠道的 API 调用${m.is_default ? '，作为默认模型优先使用' : ''}
          </div>
        </div>
        <div style="display: flex; align-items: center; gap: 12px;">
          <label class="toggle" style="transform: scale(0.9);">
            <input type="checkbox" ${m.status === 'available' || m.status === true ? 'checked' : ''} onchange="toggleModelStatus('${m.id}', this.checked)">
            <span class="toggle-slider"></span>
          </label>
          <div style="display: flex; gap: 8px;">
            <i class="action-icon" style="font-size: 1.2rem; cursor: pointer;" onclick="editModel('${m.id}')" title="编辑">✏️</i>
            <i class="action-icon" style="font-size: 1.2rem; cursor: pointer; color: #ef4444;" onclick="deleteModel('${m.id}')" title="删除">🗑️</i>
          </div>
        </div>
      </div>
      ${!m.is_default ? `<button class="btn btn-outline" style="padding: 6px 16px; font-size: 0.85rem;" onclick="setDefaultModel('${m.id}')">设为默认模型</button>` : ''}
    </div>
  `).join("");

  container.innerHTML = `
    <div style="max-width: 100%;">
      ${cards}
      <div style="margin-top: 24px; padding: 16px; background: #eff6ff; border: 1px solid #bfdbfe; border-radius: 8px; color: #1e40af;">
        <div style="display: flex; gap: 8px; align-items: start;">
          <span style="font-size: 1.2rem;">💡</span>
          <div style="flex: 1;">
            <div style="font-weight: 600; margin-bottom: 4px;">提示</div>
            <div style="font-size: 0.9rem; line-height: 1.6;">
              • 默认模型将优先用于 API 调用<br>
              • 禁用的模型不会出现在可用模型列表中<br>
              • 排序值越小，在列表中越靠前
            </div>
          </div>
        </div>
      </div>
    </div>
  `;
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

  if (model) {
    title.textContent = "编辑模型";
    document.getElementById("modelId").value = model.id;
    document.getElementById("modelChannel").value = model.channel;
    document.getElementById("modelModelId").value = model.model_id;
    document.getElementById("modelName").value = model.name;
    document.getElementById("modelSortOrder").value = model.sort_order;
    document.getElementById("modelStatus").value = model.status;
    document.getElementById("modelIsDefault").checked = model.is_default;
  } else {
    title.textContent = "添加模型";
    form.reset();
    document.getElementById("modelId").value = "";
    document.getElementById("modelChannel").value = "Orchids";
    document.getElementById("modelSortOrder").value = "0";
    document.getElementById("modelStatus").value = "available";
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
  div.textContent = text;
  return div.innerHTML;
}

// Load models on page load
document.addEventListener('DOMContentLoaded', () => {
  loadModels();
});
