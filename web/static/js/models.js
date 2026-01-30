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

  const channels = new Set(models.map(m => m.channel));
  const sorted = Array.from(channels).sort();
  const tabs = [...sorted];

  if (currentModelChannel === '' || !tabs.includes(currentModelChannel)) {
    if (currentModelChannel !== '' && tabs.length > 0) {
      currentModelChannel = tabs[0];
    }
    if (currentModelChannel === '' && tabs.length > 0) currentModelChannel = tabs[0];
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

  container.innerHTML = `
    <div class="table-wrap">
      <table>
        <thead>
          <tr>
            <th style="width: 60px;">ID</th>
            <th>渠道</th>
            <th>模型ID</th>
            <th>模型名称</th>
            <th>状态</th>
            <th>默认</th>
            <th>排序</th>
            <th style="text-align: right;">操作</th>
          </tr>
        </thead>
        <tbody>
          ${filtered.map(m => `
            <tr>
              <td style="color: #64748b; font-size: 0.9rem;">${m.id}</td>
              <td><span class="tag badge-${m.channel.toLowerCase()}">${escapeHtml(m.channel)}</span></td>
              <td style="font-family: monospace; color: #94a3b8;">${escapeHtml(m.model_id)}</td>
              <td style="font-weight: 500; color: #e2e8f0;">${escapeHtml(m.name)}</td>
              <td>
                <span class="status-badge status-enabled">
                  ${m.status === 'available' || m.status === true ? '启用' : '禁用'}
                </span>
              </td>
              <td style="text-align: center;">
                ${m.is_default ? '<span class="default-check">✅</span>' : '-'}
              </td>
              <td>${m.sort_order}</td>
              <td style="text-align: right;">
                <div style="display: flex; justify-content: flex-end; align-items: center; gap: 12px;">
                  <i class="action-icon" style="font-size: 1.2rem;" onclick="editModel('${m.id}')" title="编辑">📋</i>
                  ${!m.is_default ? `<a class="set-default-link" onclick="setDefaultModel('${m.id}')">设为默认</a>` : ''}
                  <i class="action-icon" style="font-size: 1.2rem;" onclick="deleteModel('${m.id}')" title="删除">🗑️</i>
                </div>
              </td>
            </tr>
          `).join("")}
        </tbody>
      </table>
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
