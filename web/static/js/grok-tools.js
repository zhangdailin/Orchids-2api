(() => {
  const imagineState = {
    running: false,
    mode: "auto",
    effectiveMode: "auto",
    taskIDs: [],
    wsSockets: [],
    sseStreams: [],
    imageCount: 0,
    latencySum: 0,
    latencyCount: 0,
    fallbackTimer: null,
  };
  let imagineDirectoryHandle = null;
  let imagineUseFileSystemAPI = false;
  let imagineSelectionMode = false;
  let imagineLightboxIndex = -1;
  let imagineFinalMinBytes = 100000;
  let imagineStreamSequence = 0;
  const imagineSelectedImages = new Set();
  const imagineStreamImageMap = new Map();
  const IMAGINE_BATCH_SIZE = 6;
  const IMAGINE_BATCH_PARALLELISM = 2;

  const cacheOnlineState = {
    selectedTokens: new Set(),
    accounts: [],
    details: [],
    online: {},
    onlineScope: "none",
    accountMap: new Map(),
    detailMap: new Map(),
  };

  const cacheBatchState = {
    running: false,
    action: "",
    taskID: "",
    total: 0,
    processed: 0,
    statusText: "空闲",
    eventSource: null,
  };

  const videoState = {
    taskID: "",
    stream: null,
    running: false,
    fileDataURL: "",
    startAt: 0,
    elapsedTimer: null,
    contentBuffer: "",
    progressBuffer: "",
    collectingContent: false,
    lastProgress: 0,
    currentPreviewItem: null,
    previewCount: 0,
    pollTimer: null,
  };

  const voiceState = {
    running: false,
    stopping: false,
    room: null,
    visualizerTimer: null,
    outputMuted: false,
    reconnecting: false,
  };
  const grokLazyState = {
    imagineReady: false,
    cacheReady: false,
    livekitPromise: null,
  };

  const chatState = {
    sessions: [],
    activeId: "",
    sending: false,
    abortController: null,
    pendingFile: null,
    sidebarOpen: false,
    model: "grok-4.3",
    models: [
      "grok-4.3",
      "grok-4.3-latest",
      "grok-latest",
      "grok-3",
      "grok-3-mini",
      "grok-3-thinking",
      "grok-4",
      "grok-4-mini",
      "grok-4-thinking",
      "grok-4-heavy",
      "grok-4.1-mini",
      "grok-4.1-fast",
      "grok-4.1-expert",
      "grok-4.1-thinking",
    ],
  };
  const chatStorageKey = "grok_tools_chat_sessions_v1";
  const MAX_CHAT_MESSAGES = 5;
  const chatSidebarStateKey = "grok_tools_chat_sidebar_collapsed";
  const grokToolsUIStorageKey = "grok_tools_ui_v1";
  const i18nMap = {
    "common.notConnected": "未连接",
    "common.connecting": "连接中...",
    "common.generating": "生成中",
    "common.done": "已完成",
    "common.connectionError": "连接中断",
    "common.createTaskFailed": "创建任务失败",
    "common.enterPrompt": "请输入 Prompt",
    "video.alreadyRunning": "Video 任务已在运行中",
    "video.enterPrompt": "请输入 Video Prompt",
    "video.superResolution": "超分辨率处理中...",
    "video.downloadFailed": "视频下载失败",
    "video.taskEmpty": "创建任务失败：空 task_id",
    "video.startFailed": "启动失败",
    "video.generatingPlaceholder": "视频生成中...",
    "voice.alreadyRunning": "Voice 会话已在运行中",
    "voice.tokenUnavailable": "voice token unavailable",
    "voice.connected": "已连接",
    "voice.stopped": "未连接",
    "voice.connectFailed": "连接失败",
  };

  function t(key, vars) {
    let text = i18nMap[key] || key;
    if (vars && typeof vars === "object") {
      Object.keys(vars).forEach((name) => {
        text = text.replace(new RegExp(`\\{${name}\\}`, "g"), String(vars[name]));
      });
    }
    return text;
  }

  function handleUnauthorized(res) {
    if (res && res.status === 401) {
      window.location.href = "/admin/login.html?next=" + encodeURIComponent("/admin/?tab=grok-tools");
      return true;
    }
    return false;
  }

  function detectImageMime(b64) {
    const raw = String(b64 || "");
    if (raw.startsWith("iVBOR")) return "image/png";
    if (raw.startsWith("/9j/")) return "image/jpeg";
    if (raw.startsWith("R0lGOD")) return "image/gif";
    return "image/jpeg";
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

  function formatTimeMS(ms) {
    const num = Number(ms || 0);
    if (!Number.isFinite(num) || num <= 0) return "-";
    return `${Math.round(num)} ms`;
  }

  function formatDateTime(ms) {
    const num = Number(ms || 0);
    if (!Number.isFinite(num) || num <= 0) return "-";
    return new Date(num).toLocaleString();
  }

  function relativeTime(ms) {
    const num = Number(ms || 0);
    if (!Number.isFinite(num) || num <= 0) return "-";
    const diff = Math.max(0, Math.round((Date.now() - num) / 1000));
    if (diff < 60) return "刚刚";
    if (diff < 3600) return `${Math.floor(diff / 60)} 分钟前`;
    if (diff < 86400) return `${Math.floor(diff / 3600)} 小时前`;
    return `${Math.floor(diff / 86400)} 天前`;
  }

  function loadGrokToolsUIState() {
    try {
      const raw = localStorage.getItem(grokToolsUIStorageKey);
      if (!raw) return {};
      const parsed = JSON.parse(raw);
      return parsed && typeof parsed === "object" ? parsed : {};
    } catch (err) {
      return {};
    }
  }

  function saveGrokToolsUIState(patch) {
    try {
      const current = loadGrokToolsUIState();
      localStorage.setItem(grokToolsUIStorageKey, JSON.stringify({ ...current, ...patch }));
    } catch (err) {
      // ignore storage failures
    }
  }

  function setChatSendButtonState(sending) {
    const btn = document.getElementById("grokSendBtn");
    if (!btn) return;
    btn.disabled = false;
    if (sending) {
      btn.title = "停止生成";
      btn.innerHTML = `
        <svg width="14" height="14" viewBox="0 0 24 24" fill="currentColor" aria-hidden="true">
          <rect x="6" y="6" width="12" height="12" rx="2"></rect>
        </svg>
      `;
      return;
    }
    btn.title = "发送";
    btn.innerHTML = `
      <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" aria-hidden="true">
        <path d="M22 2L11 13"></path>
        <path d="M22 2L15 22L11 13L2 9L22 2Z"></path>
      </svg>
    `;
  }

  function setChatSidebarCollapsed(collapsed) {
    const layout = document.querySelector("#grokChatSection .chat-layout");
    if (!layout) return;
    layout.classList.toggle("collapsed", collapsed);
    try {
      localStorage.setItem(chatSidebarStateKey, collapsed ? "1" : "0");
    } catch (err) {
      // ignore storage failures
    }
  }

  function closeChatSidebar() {
    if (isMobileChatSidebar()) {
      chatState.sidebarOpen = false;
      const sidebar = document.getElementById("grokChatSidebar");
      const overlay = document.getElementById("grokChatSidebarOverlay");
      sidebar?.classList.remove("open");
      sidebar?.classList.remove("show");
      overlay?.classList.remove("open");
      overlay?.classList.remove("show");
      return;
    }
    setChatSidebarCollapsed(true);
    const expandBtn = document.getElementById("grokChatExpandBtn");
    if (expandBtn) expandBtn.style.display = "inline-flex";
  }

  function openChatSidebar() {
    if (isMobileChatSidebar()) {
      chatState.sidebarOpen = true;
      const sidebar = document.getElementById("grokChatSidebar");
      const overlay = document.getElementById("grokChatSidebarOverlay");
      sidebar?.classList.add("open");
      sidebar?.classList.add("show");
      overlay?.classList.add("open");
      overlay?.classList.add("show");
      return;
    }
    setChatSidebarCollapsed(false);
    const expandBtn = document.getElementById("grokChatExpandBtn");
    if (expandBtn) expandBtn.style.display = "none";
  }

  function toggleChatSidebar() {
    if (isMobileChatSidebar()) {
      const sidebar = document.getElementById("grokChatSidebar");
      if (sidebar?.classList.contains("open") || sidebar?.classList.contains("show")) {
        closeChatSidebar();
      } else {
        openChatSidebar();
      }
      return;
    }
    const layout = document.querySelector("#grokChatSection .chat-layout");
    if (!layout) return;
    if (layout.classList.contains("collapsed")) {
      openChatSidebar();
    } else {
      closeChatSidebar();
    }
  }

  function isMobileChatSidebar() {
    return window.matchMedia("(max-width: 1024px)").matches;
  }

  function syncChatSidebarState() {
    if (isMobileChatSidebar()) {
      if (chatState.sidebarOpen) {
        openChatSidebar();
      } else {
        closeChatSidebar();
      }
      return;
    }
    const sidebar = document.getElementById("grokChatSidebar");
    const overlay = document.getElementById("grokChatSidebarOverlay");
    sidebar?.classList.remove("open");
    sidebar?.classList.remove("show");
    overlay?.classList.remove("open");
    overlay?.classList.remove("show");
    let collapsed = false;
    try {
      collapsed = localStorage.getItem(chatSidebarStateKey) === "1";
    } catch (err) {
      collapsed = false;
    }
    setChatSidebarCollapsed(collapsed);
    const expandBtn = document.getElementById("grokChatExpandBtn");
    if (expandBtn) expandBtn.style.display = collapsed ? "inline-flex" : "none";
  }

  function resolveOnlineStatusText(status) {
    const raw = String(status || "").trim();
    if (raw === "ok") return "连接正常";
    if (raw === "not_loaded") return "未加载";
    if (raw === "no_token") return "无可用 Token";
    if (!raw) return "未知";
    return raw;
  }

  function normalizeOnlineToken(raw) {
    const token = String(raw || "").trim();
    if (!token) return "";
    if (!token.includes("sso=")) return token;
    const idx = token.indexOf("sso=");
    const tail = token.slice(idx + 4);
    const semi = tail.indexOf(";");
    return (semi >= 0 ? tail.slice(0, semi) : tail).trim();
  }

  function formatTokenMask(token) {
    const raw = String(token || "").trim();
    if (!raw) return "";
    if (raw.length <= 24) return raw;
    return `${raw.slice(0, 8)}...${raw.slice(-16)}`;
  }

  function toNumberOrZero(value) {
    const n = Number(value || 0);
    return Number.isFinite(n) ? n : 0;
  }

  function closeCacheBatchStream() {
    const es = cacheBatchState.eventSource;
    cacheBatchState.eventSource = null;
    if (!es) return;
    try {
      es.close();
    } catch (err) {
      // ignore
    }
  }

  function updateCacheBatchUI() {
    const statusEl = document.getElementById("cacheOnlineBatchStatus");
    const progressEl = document.getElementById("cacheOnlineBatchProgress");
    const barEl = document.getElementById("cacheOnlineBatchBar");
    const cancelBtn = document.getElementById("cacheOnlineBatchCancelBtn");
    const loadSelectedBtn = document.getElementById("cacheOnlineLoadSelectedBtn");
    const loadAllBtn = document.getElementById("cacheOnlineLoadAllBtn");
    const clearSelectedBtn = document.getElementById("cacheOnlineClearSelectedBtn");

    const total = Math.max(0, Math.floor(toNumberOrZero(cacheBatchState.total)));
    const processed = Math.max(0, Math.floor(toNumberOrZero(cacheBatchState.processed)));
    const safeTotal = total > 0 ? total : 0;
    const safeProcessed = total > 0 ? Math.min(processed, total) : 0;
    const percent = safeTotal > 0 ? Math.floor((safeProcessed / safeTotal) * 100) : 0;

    if (statusEl) {
      const text = String(cacheBatchState.statusText || "").trim();
      statusEl.textContent = text || (cacheBatchState.running ? "运行中" : "空闲");
    }
    if (progressEl) progressEl.textContent = `${safeProcessed}/${safeTotal}`;
    if (barEl) barEl.value = percent;
    if (cancelBtn) {
      cancelBtn.style.display = cacheBatchState.running ? "inline-flex" : "none";
      cancelBtn.disabled = !cacheBatchState.running;
    }
    if (loadSelectedBtn) loadSelectedBtn.disabled = cacheBatchState.running;
    if (loadAllBtn) loadAllBtn.disabled = cacheBatchState.running;
    if (clearSelectedBtn) clearSelectedBtn.disabled = cacheBatchState.running;
  }

  function applyCacheBatchProgress(msg) {
    if (!msg || typeof msg !== "object") return;
    if (typeof msg.total === "number" && Number.isFinite(msg.total)) {
      cacheBatchState.total = Math.max(0, Math.floor(msg.total));
    }
    if (typeof msg.processed === "number" && Number.isFinite(msg.processed)) {
      cacheBatchState.processed = Math.max(0, Math.floor(msg.processed));
    } else if (typeof msg.done === "number" && Number.isFinite(msg.done)) {
      cacheBatchState.processed = Math.max(0, Math.floor(msg.done));
    }
    if (cacheBatchState.total > 0 && cacheBatchState.processed > cacheBatchState.total) {
      cacheBatchState.total = cacheBatchState.processed;
    }
    updateCacheBatchUI();
  }

  function beginCacheBatch(action, taskID, total, statusText) {
    closeCacheBatchStream();
    cacheBatchState.running = true;
    cacheBatchState.action = String(action || "").trim();
    cacheBatchState.taskID = String(taskID || "").trim();
    cacheBatchState.total = Math.max(0, Math.floor(toNumberOrZero(total)));
    cacheBatchState.processed = 0;
    cacheBatchState.statusText = String(statusText || "运行中");
    updateCacheBatchUI();
  }

  function finishCacheBatch(statusText) {
    cacheBatchState.running = false;
    cacheBatchState.action = "";
    cacheBatchState.taskID = "";
    cacheBatchState.statusText = String(statusText || "空闲");
    closeCacheBatchStream();
    updateCacheBatchUI();
  }

  function openCacheBatchStream(taskID, handlers = {}) {
    const cleanTaskID = String(taskID || "").trim();
    if (!cleanTaskID) throw new Error("empty task_id");

    const url = `/api/v1/admin/batch/${encodeURIComponent(cleanTaskID)}/stream?t=${Date.now()}`;
    const es = new EventSource(url);
    cacheBatchState.eventSource = es;
    let ended = false;

    const doneOnce = (fn) => {
      if (ended) return;
      ended = true;
      closeCacheBatchStream();
      if (typeof fn === "function") {
        Promise.resolve()
          .then(() => fn())
          .catch((err) => {
            showToast(err?.message || String(err || "批量任务处理失败"), "error");
          });
      }
    };

    es.onmessage = (event) => {
      let msg = null;
      try {
        msg = JSON.parse(event.data);
      } catch (err) {
        return;
      }
      if (!msg || typeof msg !== "object") return;
      const msgTaskID = String(msg.task_id || "").trim();
      if (msgTaskID && msgTaskID !== cleanTaskID) return;

      applyCacheBatchProgress(msg);
      const type = String(msg.type || "").trim().toLowerCase();
      if (type === "snapshot" || type === "progress") {
        return;
      }
      if (type === "done") {
        doneOnce(() => {
          if (typeof handlers.onDone === "function") {
            handlers.onDone(msg);
          }
        });
        return;
      }
      if (type === "cancelled") {
        doneOnce(() => {
          if (typeof handlers.onCancelled === "function") {
            handlers.onCancelled(msg);
          }
        });
        return;
      }
      if (type === "error") {
        doneOnce(() => {
          if (typeof handlers.onError === "function") {
            handlers.onError(String(msg.error || "unknown error"), msg);
          }
        });
      }
    };

    es.onerror = () => {
      doneOnce(() => {
        if (typeof handlers.onError === "function") {
          handlers.onError("连接中断", null);
        }
      });
    };
  }

  function setImagineStatus(text) {
    const el = document.getElementById("imagineStatus");
    if (el) el.textContent = String(text || "");
  }

  function bindPersistedCheckbox(id, storageKey) {
    const input = document.getElementById(id);
    if (!input) return;
    const uiState = loadGrokToolsUIState();
    if (typeof uiState[storageKey] === "boolean") {
      input.checked = uiState[storageKey];
    }
    input.addEventListener("change", () => {
      saveGrokToolsUIState({ [storageKey]: !!input.checked });
    });
  }

  function syncImagineModeUI(mode, persist = true) {
    const normalized = ["auto", "ws", "sse"].includes(String(mode || "").toLowerCase())
      ? String(mode || "").toLowerCase()
      : "auto";
    const select = document.getElementById("imagineMode");
    if (select) select.value = normalized;
    document.querySelectorAll("[data-imagine-mode]").forEach((btn) => {
      const active = String(btn.dataset.imagineMode || "").toLowerCase() === normalized;
      btn.classList.toggle("active", active);
    });
    if (persist) {
      saveGrokToolsUIState({ imagineMode: normalized });
    }
  }

  function applyImagineMode(mode, persist = true) {
    const normalized = ["auto", "ws", "sse"].includes(String(mode || "").toLowerCase())
      ? String(mode || "").toLowerCase()
      : "auto";
    syncImagineModeUI(normalized, persist);
    imagineState.mode = normalized;
    if (imagineState.running && Array.isArray(imagineState.taskIDs) && imagineState.taskIDs.length > 0) {
      const prompt = String(document.getElementById("imaginePrompt")?.value || "").trim();
      const ratio = String(document.getElementById("imagineRatio")?.value || "2:3");
      if (normalized === "sse") {
        startImagineSSE(imagineState.taskIDs);
      } else if (normalized === "ws") {
        startImagineWS(imagineState.taskIDs, prompt, ratio, false);
      } else {
        startImagineWS(imagineState.taskIDs, prompt, ratio, true);
      }
    }
  }

  function setImagineButtons(running) {
    const startBtn = document.getElementById("imagineStartBtn");
    const stopBtn = document.getElementById("imagineStopBtn");
    if (startBtn) {
      startBtn.disabled = false;
      startBtn.classList.remove("hidden");
      startBtn.innerHTML = running
        ? '<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M10 7v10"></path><path d="M14 7v10"></path></svg>'
        : '<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M12 5v14"></path><path d="m6 11 6-6 6 6"></path></svg>';
      startBtn.setAttribute("aria-label", running ? "停止" : "生成");
      startBtn.setAttribute("title", running ? "停止" : "生成");
    }
    if (stopBtn) {
      stopBtn.disabled = !running;
      stopBtn.classList.add("hidden");
    }
  }

  function setImagineControlsDisabled(disabled) {
    const prompt = document.getElementById("imaginePrompt");
    if (prompt) prompt.disabled = !!disabled;
    const ratio = document.getElementById("imagineRatio");
    if (ratio) ratio.disabled = !!disabled;
    document.querySelectorAll("#imagineRunModeToggle button, #imagineQualityToggle button").forEach((btn) => {
      btn.disabled = !!disabled;
    });
  }

  function imagineOptionEnabled(id, fallback) {
    const input = document.getElementById(id);
    if (!input) return !!fallback;
    return !!input.checked;
  }

  function updateImagineActiveCount() {
    const el = document.getElementById("imagineActive");
    if (!el) return;
    if (imagineState.effectiveMode === "sse") {
      const count = imagineState.sseStreams.filter((s) => s && s.readyState !== 2).length;
      el.textContent = String(count);
      return;
    }
    const count = imagineState.wsSockets.filter((w) => w && w.readyState === 1).length;
    el.textContent = String(count);
  }

  function resetImagineMetrics() {
    imagineState.imageCount = 0;
    imagineState.latencySum = 0;
    imagineState.latencyCount = 0;
    const count = document.getElementById("imagineCount");
    const latency = document.getElementById("imagineLatency");
    if (count) count.textContent = "0";
    if (latency) latency.textContent = "-";
  }

  function isLikelyBase64(raw) {
    if (!raw) return false;
    if (raw.startsWith("data:")) return true;
    if (raw.startsWith("http://") || raw.startsWith("https://")) return false;
    const head = raw.slice(0, 16);
    if (head.startsWith("/9j/") || head.startsWith("iVBOR") || head.startsWith("R0lGOD")) return true;
    return /^[A-Za-z0-9+/=\s]+$/.test(raw);
  }

  function inferMime(base64) {
    if (!base64) return "image/jpeg";
    if (base64.startsWith("data:")) {
      const match = base64.match(/data:(.*?);base64/);
      return match ? match[1] : "image/jpeg";
    }
    return detectImageMime(base64);
  }

  function estimateBase64Bytes(raw) {
    if (!raw) return null;
    if (raw.startsWith("http://") || raw.startsWith("https://")) {
      return null;
    }
    if (raw.startsWith("/") && !isLikelyBase64(raw)) {
      return null;
    }
    let base64 = raw;
    if (raw.startsWith("data:")) {
      const comma = raw.indexOf(",");
      base64 = comma >= 0 ? raw.slice(comma + 1) : "";
    }
    base64 = base64.replace(/\s/g, "");
    if (!base64) return 0;
    let padding = 0;
    if (base64.endsWith("==")) padding = 2;
    else if (base64.endsWith("=")) padding = 1;
    return Math.max(0, Math.floor((base64.length * 3) / 4) - padding);
  }

  function getFinalMinBytes() {
    return Number.isFinite(imagineFinalMinBytes) && imagineFinalMinBytes >= 0 ? imagineFinalMinBytes : 100000;
  }

  function imagineAspectRatioCss(value) {
    const ratio = String(value || "1:1").trim();
    if (!ratio.includes(":")) return "1 / 1";
    const parts = ratio.split(":");
    const width = Math.max(1, Number(parts[0]) || 1);
    const height = Math.max(1, Number(parts[1]) || 1);
    return `${width} / ${height}`;
  }

  function imagineSizeForRatio(value) {
    switch (String(value || "").trim()) {
      case "16:9":
      case "3:2":
        return "1792x1024";
      case "9:16":
      case "2:3":
        return "1024x1792";
      case "1:1":
      default:
        return "1024x1024";
    }
  }

  function imagineReadToggle(selector, attr, fallback) {
    const active = document.querySelector(`${selector} .is-active`);
    return String(active?.dataset?.[attr] || fallback || "").trim();
  }

  function imagineSetToggle(selector, attr, value) {
    document.querySelectorAll(`${selector} [data-${attr.replace(/[A-Z]/g, (m) => `-${m.toLowerCase()}`)}]`).forEach((btn) => {
      const active = String(btn.dataset[attr] || "") === String(value || "");
      btn.classList.toggle("is-active", active);
      btn.setAttribute("aria-pressed", active ? "true" : "false");
    });
  }

  function normalizeImagineQuality(value) {
    const raw = String(value || "").trim().toLowerCase();
    if (raw === "basic" || raw === "speed") return "basic";
    if (raw === "quality") return "quality";
    return "lite";
  }

  function imagineQualityModel(quality) {
    switch (normalizeImagineQuality(quality)) {
      case "basic":
        return "grok-imagine-image-lite";
      case "quality":
        return "grok-imagine-image-pro";
      case "lite":
      default:
        return "grok-imagine-image";
    }
  }

  function imagineQualityModels(quality) {
    return [imagineQualityModel(quality)];
  }

  function imagineQualityLabel(quality) {
    switch (normalizeImagineQuality(quality)) {
      case "basic":
        return "Basic";
      case "quality":
        return "Quality";
      case "lite":
      default:
        return "Lite";
    }
  }

  function syncImagineRatioUI() {
    const ratio = String(document.getElementById("imagineRatio")?.value || "2:3");
    const wrap = document.getElementById("imagineRatioWrap");
    if (wrap) wrap.dataset.ratio = ratio;
  }

  function resizeImaginePrompt() {
    const input = document.getElementById("imaginePrompt");
    if (!input) return;
    input.style.height = "52px";
    input.style.height = `${Math.min(Math.max(input.scrollHeight, 52), 160)}px`;
    input.style.overflowY = input.scrollHeight > 160 ? "auto" : "hidden";
  }

  function setImagineEmptyState() {
    const grid = document.getElementById("imagineGrid");
    const empty = document.getElementById("imagineEmpty");
    if (!grid || !empty) return;
    const hasBatch = grid.querySelector(".imagine-masonry-batch") !== null;
    empty.hidden = hasBatch;
    empty.style.display = hasBatch ? "none" : "";
  }

  function createImagineBatch(prompt, ratio, quality, round) {
    const grid = document.getElementById("imagineGrid");
    if (!grid) return null;

    const batch = document.createElement("section");
    batch.className = "imagine-masonry-batch";

    const head = document.createElement("header");
    head.className = "imagine-masonry-batch-head";

    const promptEl = document.createElement("div");
    promptEl.className = "imagine-masonry-batch-prompt";
    promptEl.textContent = prompt;

    const meta = document.createElement("div");
    meta.className = "imagine-masonry-batch-meta";
    const chips = [
      ["is-round", `第 ${round} 轮`],
      ["is-param", ratio],
      ["is-param", imagineQualityLabel(quality)],
      ["is-count", `0/${IMAGINE_BATCH_SIZE}`],
      ["is-state", "正在生成"],
    ];
    chips.forEach(([cls, text]) => {
      const chip = document.createElement("span");
      chip.className = `imagine-masonry-batch-chip ${cls}`;
      chip.textContent = text;
      if (cls === "is-state") chip.dataset.state = "generating";
      meta.appendChild(chip);
    });

    head.appendChild(promptEl);
    head.appendChild(meta);

    const slotGrid = document.createElement("div");
    slotGrid.className = "imagine-masonry-grid";
    slotGrid.style.setProperty("--tile-aspect", imagineAspectRatioCss(ratio));

    const slots = Array.from({ length: IMAGINE_BATCH_SIZE }, (_, idx) => {
      const tile = document.createElement("article");
      tile.className = "imagine-masonry-tile waterfall-item is-pending";
      tile.dataset.prompt = prompt;

      const checkbox = document.createElement("div");
      checkbox.className = "image-checkbox";

      const badge = document.createElement("div");
      badge.className = "imagine-masonry-tile-badge";
      badge.textContent = String(idx + 1);

      const body = document.createElement("div");
      body.className = "imagine-masonry-tile-body";

      const progress = document.createElement("div");
      progress.className = "imagine-masonry-tile-progress";
      progress.innerHTML = '<div class="imagine-masonry-tile-progress-value">0%</div><div class="imagine-masonry-tile-progress-track"><div class="imagine-masonry-tile-progress-fill"></div></div>';
      body.appendChild(progress);

      tile.appendChild(checkbox);
      tile.appendChild(badge);
      tile.appendChild(body);
      slotGrid.appendChild(tile);
      return { tile, body, progress: 0, url: "", status: "pending" };
    });

    batch.appendChild(head);
    batch.appendChild(slotGrid);
    grid.prepend(batch);
    setImagineEmptyState();

    return {
      el: batch,
      countEl: meta.querySelector(".is-count"),
      stateEl: meta.querySelector(".is-state"),
      slots,
      ready: 0,
      failed: 0,
      round,
    };
  }

  function updateImagineBatchMeta(batch, final) {
    if (!batch) return;
    if (batch.countEl) batch.countEl.textContent = `${batch.ready}/${IMAGINE_BATCH_SIZE}`;
    if (!batch.stateEl) return;
    if (!final) {
      batch.stateEl.dataset.state = "generating";
      batch.stateEl.textContent = "正在生成";
      return;
    }
    if (batch.ready >= IMAGINE_BATCH_SIZE) {
      batch.stateEl.dataset.state = "success";
      batch.stateEl.textContent = "生成成功";
    } else if (batch.ready > 0) {
      batch.stateEl.dataset.state = "partial";
      batch.stateEl.textContent = "部分失败";
    } else {
      batch.stateEl.dataset.state = "failed";
      batch.stateEl.textContent = "生成失败";
    }
  }

  function setImagineSlotProgress(slot, value) {
    if (!slot || slot.status !== "pending") return;
    const progress = Math.max(0, Math.min(99, Number(value) || 0));
    slot.progress = progress;
    const text = slot.tile.querySelector(".imagine-masonry-tile-progress-value");
    const fill = slot.tile.querySelector(".imagine-masonry-tile-progress-fill");
    if (text) text.textContent = `${progress}%`;
    if (fill) fill.style.width = `${progress}%`;
  }

  function finishImagineSlot(slot, raw, meta) {
    if (!slot || slot.status !== "pending") return;
    const value = String(raw || "");
    if (!value) {
      failImagineSlot(slot, "失败");
      return;
    }
    const isURL = value.startsWith("http://") || value.startsWith("https://") || value.startsWith("/") || value.startsWith("data:");
    const src = isURL ? value : `data:${inferMime(value)};base64,${value}`;
    slot.status = "ready";
    slot.url = src;
    slot.tile.classList.remove("is-pending", "is-filtered");
    slot.tile.classList.add("is-ready");
    slot.tile.dataset.imageUrl = src;
    if (imagineSelectionMode) slot.tile.classList.add("selection-mode");

    const link = document.createElement("div");
    link.className = "imagine-masonry-tile-link";
    const img = document.createElement("img");
    img.loading = "lazy";
    img.decoding = "async";
    img.alt = meta && meta.sequence ? `image-${meta.sequence}` : "image";
    img.src = src;
    link.appendChild(img);
    slot.body.replaceChildren(link);

    imagineState.imageCount += 1;
    const count = document.getElementById("imagineCount");
    if (count) count.textContent = String(imagineState.imageCount);
  }

  function failImagineSlot(slot, label) {
    if (!slot || slot.status !== "pending") return;
    slot.status = "failed";
    slot.tile.classList.remove("is-pending", "is-ready");
    slot.tile.classList.add("is-filtered");
    const message = document.createElement("div");
    message.className = "imagine-masonry-tile-label";
    message.textContent = label || "失败";
    slot.body.replaceChildren(message);
  }

  function extractImagineImageValue(data) {
    const list = Array.isArray(data?.data) ? data.data : [];
    const item = list[0] || {};
    return String(item.url || item.b64_json || item.base64 || item.b64 || "");
  }

  async function requestImagineImage(prompt, ratio, model, nsfw, signal) {
    const res = await fetch("/grok/v1/images/generations", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      signal,
      body: JSON.stringify({
        model,
        prompt,
        n: 1,
        size: imagineSizeForRatio(ratio),
        response_format: "url",
        nsfw,
      }),
    });
    if (handleUnauthorized(res)) throw new Error("unauthorized");
    if (!res.ok) throw new Error(await res.text());
    const data = await res.json();
    const image = extractImagineImageValue(data);
    if (!image) throw new Error("no image generated");
    return image;
  }

  async function runImagineSlot(batch, slot, prompt, ratio, models, nsfw, signal) {
    const startedAt = Date.now();
    let tick = 0;
    const timer = window.setInterval(() => {
      tick += 1;
      setImagineSlotProgress(slot, Math.min(92, 8 + tick * 7));
    }, 900);
    try {
      const candidates = Array.isArray(models) && models.length ? models : [String(models || "grok-imagine-image-lite")];
      let image = "";
      let lastErr = null;
      for (let i = 0; i < candidates.length; i += 1) {
        try {
          image = await requestImagineImage(prompt, ratio, candidates[i], nsfw, signal);
          break;
        } catch (err) {
          lastErr = err;
          if (signal?.aborted || String(err?.message || err) === "unauthorized") throw err;
          if (i < candidates.length - 1) {
            setImagineSlotProgress(slot, Math.max(slot.progress || 0, 35));
          }
        }
      }
      if (!image) throw lastErr || new Error("no image generated");
      finishImagineSlot(slot, image, { elapsed_ms: Date.now() - startedAt });
      if (batch) {
        batch.ready += 1;
        updateImagineBatchMeta(batch, false);
      }
      return true;
    } catch (err) {
      if (signal?.aborted) {
        failImagineSlot(slot, "已停止");
      } else {
        failImagineSlot(slot, "失败");
      }
      if (batch) {
        batch.failed += 1;
        updateImagineBatchMeta(batch, false);
      }
      return false;
    } finally {
      window.clearInterval(timer);
    }
  }

  async function runImagineBatchSlots(batch, prompt, ratio, models, nsfw, signal) {
    if (!batch || !Array.isArray(batch.slots)) return [];
    const results = new Array(batch.slots.length).fill(false);
    let cursor = 0;
    const workerCount = Math.min(IMAGINE_BATCH_PARALLELISM, batch.slots.length);
    const workers = Array.from({ length: workerCount }, async () => {
      while (cursor < batch.slots.length) {
        if (signal?.aborted) break;
        const index = cursor;
        cursor += 1;
        results[index] = await runImagineSlot(batch, batch.slots[index], prompt, ratio, models, nsfw, signal);
      }
    });
    await Promise.all(workers);
    return results;
  }

  function dataUrlToBlob(dataUrl) {
    const parts = String(dataUrl || "").split(",");
    if (parts.length < 2) return null;
    const header = parts[0];
    const b64 = parts.slice(1).join(",");
    const match = header.match(/data:(.*?);base64/);
    const mime = match ? match[1] : "application/octet-stream";
    try {
      const byteString = atob(b64);
      const ab = new ArrayBuffer(byteString.length);
      const ia = new Uint8Array(ab);
      for (let i = 0; i < byteString.length; i++) {
        ia[i] = byteString.charCodeAt(i);
      }
      return new Blob([ab], { type: mime });
    } catch (err) {
      return null;
    }
  }

  async function saveToFileSystem(base64, filename) {
    try {
      if (!imagineDirectoryHandle) {
        return false;
      }
      const mime = inferMime(base64);
      const ext = mime === "image/png" ? "png" : "jpg";
      const finalFilename = filename.endsWith(`.${ext}`) ? filename : `${filename}.${ext}`;
      const fileHandle = await imagineDirectoryHandle.getFileHandle(finalFilename, { create: true });
      const writable = await fileHandle.createWritable();
      const byteString = atob(base64);
      const ab = new ArrayBuffer(byteString.length);
      const ia = new Uint8Array(ab);
      for (let i = 0; i < byteString.length; i++) {
        ia[i] = byteString.charCodeAt(i);
      }
      const blob = new Blob([ab], { type: mime });
      await writable.write(blob);
      await writable.close();
      return true;
    } catch (err) {
      return false;
    }
  }

  function downloadImage(raw, filename) {
    if (!raw) return;
    let href = raw;
    if (!href.startsWith("data:") && !/^https?:/i.test(href) && !href.startsWith("/")) {
      const mime = inferMime(raw);
      href = `data:${mime};base64,${raw}`;
    }
    const link = document.createElement("a");
    link.href = href;
    link.download = filename;
    link.style.display = "none";
    document.body.appendChild(link);
    link.click();
    document.body.removeChild(link);
  }

  function setImagineImageStatus(item, state, label) {
    if (!item) return;
    const statusEl = item.querySelector(".image-status");
    if (!statusEl) return;
    statusEl.textContent = label;
    statusEl.classList.remove("running", "done", "error");
    if (state) {
      statusEl.classList.add(state);
    }
  }

  function appendImagineImage(b64, meta, fileURL) {
    const waterfall = document.getElementById("imagineGrid");
    const empty = document.getElementById("imagineEmpty");
    if (!waterfall) return;
    if (empty) empty.style.display = "none";

    if (imagineOptionEnabled("imagineAutoFilter", false)) {
      const bytes = estimateBase64Bytes(b64 || "");
      const minBytes = getFinalMinBytes();
      if (bytes !== null && bytes < minBytes) {
        return;
      }
    }

    const item = document.createElement("div");
    item.className = "waterfall-item";

    const checkbox = document.createElement("div");
    checkbox.className = "image-checkbox";

    const img = document.createElement("img");
    img.loading = "lazy";
    img.decoding = "async";
    img.alt = meta && meta.sequence ? `image-${meta.sequence}` : "image";
    const mime = inferMime(b64);
    const dataUrl = fileURL || (b64.startsWith("data:") ? b64 : (b64 ? `data:${mime};base64,${b64}` : ""));
    if (!dataUrl) return;
    img.src = dataUrl;

    const metaBar = document.createElement("div");
    metaBar.className = "waterfall-meta";
    const left = document.createElement("div");
    left.textContent = meta && meta.sequence ? `#${meta.sequence}` : "#";
    const rightWrap = document.createElement("div");
    rightWrap.className = "meta-right";
    const status = document.createElement("span");
    status.className = "image-status done";
    status.textContent = "完成";
    const right = document.createElement("span");
    if (meta && meta.elapsed_ms) {
      right.textContent = `${meta.elapsed_ms}ms`;
    } else {
      right.textContent = "";
    }
    rightWrap.appendChild(status);
    rightWrap.appendChild(right);
    metaBar.appendChild(left);
    metaBar.appendChild(rightWrap);

    item.appendChild(checkbox);
    item.appendChild(img);
    item.appendChild(metaBar);

    const prompt = meta && meta.prompt ? String(meta.prompt) : String(document.getElementById("imaginePrompt")?.value || "").trim();
    item.dataset.imageUrl = dataUrl;
    item.dataset.prompt = prompt || "image";
    if (imagineSelectionMode) {
      item.classList.add("selection-mode");
    }

    if (imagineOptionEnabled("imagineReverseInsert", true)) {
      waterfall.prepend(item);
    } else {
      waterfall.appendChild(item);
    }

    if (imagineOptionEnabled("imagineAutoScroll", true)) {
      if (imagineOptionEnabled("imagineReverseInsert", true)) {
        window.scrollTo({ top: 0, behavior: "smooth" });
      } else {
        window.scrollTo({ top: document.body.scrollHeight, behavior: "smooth" });
      }
    }

    if (imagineOptionEnabled("imagineAutoDownload", false)) {
      const timestamp = Date.now();
      const seq = meta && meta.sequence ? meta.sequence : "unknown";
      const ext = mime === "image/png" ? "png" : "jpg";
      const filename = `imagine_${timestamp}_${seq}.${ext}`;
      if (imagineUseFileSystemAPI && imagineDirectoryHandle && b64 && !b64.startsWith("data:")) {
        saveToFileSystem(b64, filename).catch(() => {
          downloadImage(dataUrl, filename);
        });
      } else {
        downloadImage(dataUrl, filename);
      }
    }
  }

  function upsertStreamImage(raw, meta, imageId, isFinal) {
    const waterfall = document.getElementById("imagineGrid");
    const empty = document.getElementById("imagineEmpty");
    if (!waterfall || !raw) return;
    if (empty) empty.style.display = "none";

    if (isFinal && imagineOptionEnabled("imagineAutoFilter", false)) {
      const bytes = estimateBase64Bytes(raw);
      const minBytes = getFinalMinBytes();
      if (bytes !== null && bytes < minBytes) {
        const existing = imageId ? imagineStreamImageMap.get(imageId) : null;
        if (existing) {
          if (imagineSelectedImages.has(existing)) {
            imagineSelectedImages.delete(existing);
            updateImagineSelectedCount();
          }
          existing.remove();
          imagineStreamImageMap.delete(imageId);
          if (imagineState.imageCount > 0) {
            imagineState.imageCount -= 1;
            const count = document.getElementById("imagineCount");
            if (count) count.textContent = String(imagineState.imageCount);
          }
        }
        return;
      }
    }

    const isDataUrl = typeof raw === "string" && raw.startsWith("data:");
    const looksLikeBase64 = typeof raw === "string" && isLikelyBase64(raw);
    const isHttpUrl = typeof raw === "string" && (raw.startsWith("http://") || raw.startsWith("https://") || (raw.startsWith("/") && !looksLikeBase64));
    const mime = isDataUrl || isHttpUrl ? "" : inferMime(raw);
    const dataUrl = isDataUrl || isHttpUrl ? raw : `data:${mime};base64,${raw}`;

    let item = imageId ? imagineStreamImageMap.get(imageId) : null;
    let isNew = false;
    if (!item) {
      isNew = true;
      imagineStreamSequence += 1;
      const sequence = imagineStreamSequence;

      item = document.createElement("div");
      item.className = "waterfall-item";

      const checkbox = document.createElement("div");
      checkbox.className = "image-checkbox";

      const img = document.createElement("img");
      img.loading = "lazy";
      img.decoding = "async";
      img.alt = imageId ? `image-${imageId}` : "image";
      img.src = dataUrl;

      const metaBar = document.createElement("div");
      metaBar.className = "waterfall-meta";
      const left = document.createElement("div");
      left.textContent = `#${sequence}`;
      const rightWrap = document.createElement("div");
      rightWrap.className = "meta-right";
      const status = document.createElement("span");
      status.className = `image-status ${isFinal ? "done" : "running"}`;
      status.textContent = isFinal ? "完成" : "生成中";
      const right = document.createElement("span");
      right.textContent = "";
      if (meta && meta.elapsed_ms) {
        right.textContent = `${meta.elapsed_ms}ms`;
      }
      rightWrap.appendChild(status);
      rightWrap.appendChild(right);
      metaBar.appendChild(left);
      metaBar.appendChild(rightWrap);

      item.appendChild(checkbox);
      item.appendChild(img);
      item.appendChild(metaBar);

      const prompt = meta && meta.prompt ? String(meta.prompt) : String(document.getElementById("imaginePrompt")?.value || "").trim();
      item.dataset.imageUrl = dataUrl;
      item.dataset.prompt = prompt || "image";

      if (imagineSelectionMode) {
        item.classList.add("selection-mode");
      }

      if (imagineOptionEnabled("imagineReverseInsert", true)) {
        waterfall.prepend(item);
      } else {
        waterfall.appendChild(item);
      }

      if (imageId) {
        imagineStreamImageMap.set(imageId, item);
      }

      imagineState.imageCount += 1;
      const count = document.getElementById("imagineCount");
      if (count) count.textContent = String(imagineState.imageCount);
    } else {
      const img = item.querySelector("img");
      if (img) {
        img.src = dataUrl;
      }
      item.dataset.imageUrl = dataUrl;
      const right = item.querySelector(".waterfall-meta .meta-right span:last-child");
      if (right && meta && meta.elapsed_ms) {
        right.textContent = `${meta.elapsed_ms}ms`;
      }
    }

    setImagineImageStatus(item, isFinal ? "done" : "running", isFinal ? "完成" : "生成中");

    if (isNew && imagineOptionEnabled("imagineAutoScroll", true)) {
      if (imagineOptionEnabled("imagineReverseInsert", true)) {
        window.scrollTo({ top: 0, behavior: "smooth" });
      } else {
        window.scrollTo({ top: document.body.scrollHeight, behavior: "smooth" });
      }
    }

    if (isFinal && imagineOptionEnabled("imagineAutoDownload", false)) {
      const timestamp = Date.now();
      const ext = mime === "image/png" ? "png" : "jpg";
      const filename = `imagine_${timestamp}_${imageId || imagineStreamSequence}.${ext}`;
      if (imagineUseFileSystemAPI && imagineDirectoryHandle && !isHttpUrl && !isDataUrl) {
        saveToFileSystem(raw, filename).catch(() => {
          downloadImage(raw, filename);
        });
      } else {
        downloadImage(dataUrl, filename);
      }
    }
  }

  function handleImagineMessage(payload) {
    if (!payload || typeof payload !== "object") return;

    if (payload.type === "image_generation.partial_image" || payload.type === "image_generation.completed") {
      const imageId = payload.image_id || payload.imageId;
      const raw = payload.b64_json || payload.url || payload.image;
      if (!raw || !imageId) return;
      const isFinal = payload.type === "image_generation.completed" || payload.stage === "final";
      upsertStreamImage(raw, payload, imageId, isFinal);
      return;
    }

    if (payload.type === "image") {
      const b64 = String(payload.b64_json || "");
      const fileURL = String(payload.file_url || payload.url || "");
      if (!b64 && !fileURL) return;
      imagineState.imageCount += 1;
      const count = document.getElementById("imagineCount");
      if (count) count.textContent = String(imagineState.imageCount);

      const elapsed = Number(payload.elapsed_ms || 0);
      if (elapsed > 0) {
        imagineState.latencySum += elapsed;
        imagineState.latencyCount += 1;
        const avg = Math.round(imagineState.latencySum / imagineState.latencyCount);
        const latency = document.getElementById("imagineLatency");
        if (latency) latency.textContent = `${avg} ms`;
      }
      appendImagineImage(b64, payload, fileURL);
      return;
    }

    if (payload.type === "status") {
      if (payload.status === "running") {
        setImagineStatus(`运行中 (${imagineState.effectiveMode.toUpperCase()})`);
      } else if (payload.status === "stopped") {
        if (imagineState.running) {
          setImagineStatus("已停止");
        }
      }
      return;
    }

    if (payload.type === "error" || payload.error) {
      const message = payload.message || (payload.error && payload.error.message) || "Imagine 运行出错";
      const errorImageId = payload.image_id || payload.imageId;
      if (errorImageId && imagineStreamImageMap.has(errorImageId)) {
        setImagineImageStatus(imagineStreamImageMap.get(errorImageId), "error", "失败");
      }
      const lower = String(message || "").toLowerCase();
      if (lower.includes("no image generated") || lower.includes("429") || lower.includes("rate-limited") || lower.includes("cooling down")) {
        setImagineStatus("等待重试");
        return;
      }
      showToast(String(message || "Imagine 运行出错"), "error");
      setImagineStatus("错误");
    }
  }

  function updateImagineSelectedCount() {
    const countSpan = document.getElementById("selectedCount");
    if (countSpan) {
      countSpan.textContent = String(imagineSelectedImages.size);
    }
    const downloadBtn = document.getElementById("downloadSelectedBtn");
    if (downloadBtn) {
      downloadBtn.disabled = imagineSelectedImages.size === 0;
    }
    const toggleBtn = document.getElementById("toggleSelectAllBtn");
    if (toggleBtn) {
      const items = document.querySelectorAll("#imagineGrid .waterfall-item.is-ready");
      const allSelected = items.length > 0 && imagineSelectedImages.size === items.length;
      toggleBtn.textContent = allSelected ? "取消全选" : "全选";
    }
  }

  function enterImagineSelectionMode() {
    imagineSelectionMode = true;
    imagineSelectedImages.clear();
    const toolbar = document.getElementById("selectionToolbar");
    if (toolbar) toolbar.classList.remove("hidden");
    const items = document.querySelectorAll("#imagineGrid .waterfall-item.is-ready");
    if (items.length === 0) {
      imagineSelectionMode = false;
      if (toolbar) toolbar.classList.add("hidden");
      showToast("暂无可下载图片", "info");
      updateImagineSelectedCount();
      return;
    }
    items.forEach((item) => {
      item.classList.add("selection-mode");
    });
    updateImagineSelectedCount();
  }

  function exitImagineSelectionMode() {
    imagineSelectionMode = false;
    imagineSelectedImages.clear();
    const toolbar = document.getElementById("selectionToolbar");
    if (toolbar) toolbar.classList.add("hidden");
    const items = document.querySelectorAll("#imagineGrid .waterfall-item");
    items.forEach((item) => {
      item.classList.remove("selection-mode", "selected");
    });
    updateImagineSelectedCount();
  }

  function toggleImagineSelectionMode() {
    if (imagineSelectionMode) {
      exitImagineSelectionMode();
    } else {
      enterImagineSelectionMode();
    }
  }

  function toggleImagineItemSelection(item) {
    if (!imagineSelectionMode) return;
    if (item.classList.contains("selected")) {
      item.classList.remove("selected");
      imagineSelectedImages.delete(item);
    } else {
      item.classList.add("selected");
      imagineSelectedImages.add(item);
    }
    updateImagineSelectedCount();
  }

  function toggleImagineSelectAll() {
    const items = document.querySelectorAll("#imagineGrid .waterfall-item.is-ready");
    const allSelected = items.length > 0 && imagineSelectedImages.size === items.length;
    if (allSelected) {
      items.forEach((item) => item.classList.remove("selected"));
      imagineSelectedImages.clear();
    } else {
      items.forEach((item) => {
        item.classList.add("selected");
        imagineSelectedImages.add(item);
      });
    }
    updateImagineSelectedCount();
  }

  async function downloadSelectedImages() {
    if (imagineSelectedImages.size === 0) {
      toggleImagineSelectAll();
      if (imagineSelectedImages.size === 0) {
        showToast("未选择图片", "info");
        return;
      }
    }
    if (typeof JSZip === "undefined") {
      showToast("JSZip 未加载", "error");
      return;
    }
    const downloadBtn = document.getElementById("downloadSelectedBtn");
    if (downloadBtn) {
      downloadBtn.disabled = true;
      downloadBtn.textContent = "打包中...";
    }
    const zip = new JSZip();
    const imgFolder = zip.folder("images");
    let processed = 0;

    try {
      for (const item of imagineSelectedImages) {
        const url = item.dataset.imageUrl || "";
        const prompt = item.dataset.prompt || "image";
        try {
          let blob = null;
          if (url.startsWith("data:")) {
            blob = dataUrlToBlob(url);
          } else if (url) {
            const response = await fetch(url);
            blob = await response.blob();
          }
          if (!blob) {
            throw new Error("empty blob");
          }
          const safeName = String(prompt).substring(0, 30).replace(/[^a-zA-Z0-9\u4e00-\u9fa5]/g, "_") || "image";
          const filename = `${safeName}_${processed + 1}.png`;
          imgFolder.file(filename, blob);
          processed += 1;
          if (downloadBtn) {
            downloadBtn.textContent = `打包中 ${processed}/${imagineSelectedImages.size}`;
          }
        } catch (err) {
          // ignore individual failures
        }
      }

      if (processed === 0) {
        showToast("没有可下载的图片", "error");
        return;
      }

      if (downloadBtn) downloadBtn.textContent = "生成压缩包...";
      const content = await zip.generateAsync({ type: "blob" });
      const link = document.createElement("a");
      link.href = URL.createObjectURL(content);
      link.download = `imagine_${new Date().toISOString().slice(0, 10)}_${Date.now()}.zip`;
      link.click();
      URL.revokeObjectURL(link.href);

      showToast(`已打包 ${processed} 张图片`, "success");
      exitImagineSelectionMode();
    } catch (err) {
      showToast("打包失败", "error");
    } finally {
      if (downloadBtn) {
        downloadBtn.disabled = false;
        downloadBtn.innerHTML = `下载 <span id="selectedCount" class="selected-count">${imagineSelectedImages.size}</span>`;
      }
    }
  }

  async function loadImagineConfig() {
    try {
      const res = await fetch("/v1/public/imagine/config", { cache: "no-store" });
      if (!res.ok) return;
      const data = await res.json();
      const value = parseInt(data && data.final_min_bytes, 10);
      if (Number.isFinite(value) && value >= 0) {
        imagineFinalMinBytes = value;
      }
      const nsfwSelect = document.getElementById("imagineNSFW");
      const modelSelect = document.getElementById("imagineModel");
      const uiState = loadGrokToolsUIState();
      if (nsfwSelect && typeof data.nsfw === "boolean" && !uiState.imagineNSFW) {
        nsfwSelect.value = data.nsfw ? "true" : "false";
      }
      if (modelSelect && typeof uiState.imagineModel === "string" && uiState.imagineModel) {
        modelSelect.value = uiState.imagineModel;
      }
    } catch (err) {
      // ignore
    }
  }

  function closeImagineConnections(sendStop) {
    if (imagineState.fallbackTimer) {
      clearTimeout(imagineState.fallbackTimer);
      imagineState.fallbackTimer = null;
    }

    imagineState.wsSockets.forEach((ws) => {
      if (!ws) return;
      if (sendStop && ws.readyState === 1) {
        try {
          ws.send(JSON.stringify({ type: "stop" }));
        } catch (err) {
          // ignore
        }
      }
      try {
        ws.close(1000, "stop");
      } catch (err) {
        // ignore
      }
    });
    imagineState.wsSockets = [];

    imagineState.sseStreams.forEach((es) => {
      if (!es) return;
      try {
        es.close();
      } catch (err) {
        // ignore
      }
    });
    imagineState.sseStreams = [];
    updateImagineActiveCount();
  }

  async function createImagineTask(prompt, aspectRatio, nsfwEnabled) {
    const model = String(document.getElementById("imagineModel")?.value || "grok-imagine-image-lite").trim() || "grok-imagine-image-lite";
    const res = await fetch("/api/v1/admin/imagine/start", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        prompt,
        aspect_ratio: aspectRatio,
        model,
        nsfw: !!nsfwEnabled,
      }),
    });
    if (handleUnauthorized(res)) {
      throw new Error("unauthorized");
    }
    if (!res.ok) {
      throw new Error(await res.text());
    }
    const data = await res.json();
    return String((data && data.task_id) || "").trim();
  }

  async function stopImagineTasks(taskIDs) {
    if (!Array.isArray(taskIDs) || taskIDs.length === 0) return;
    const res = await fetch("/api/v1/admin/imagine/stop", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ task_ids: taskIDs }),
    });
    if (handleUnauthorized(res)) return;
  }

  function startImagineSSE(taskIDs) {
    imagineState.effectiveMode = "sse";
    setImagineStatus("连接中 (SSE)");
    closeImagineConnections(false);

    taskIDs.forEach((taskID, idx) => {
      const url = `/api/v1/admin/imagine/sse?task_id=${encodeURIComponent(taskID)}&conn=${idx}&t=${Date.now()}`;
      const es = new EventSource(url);
      es.onopen = () => {
        setImagineStatus("运行中 (SSE)");
        updateImagineActiveCount();
      };
      es.onmessage = (event) => {
        try {
          handleImagineMessage(JSON.parse(event.data));
        } catch (err) {
          // ignore bad payload
        }
      };
      es.onerror = () => {
        updateImagineActiveCount();
        const alive = imagineState.sseStreams.filter((s) => s && s.readyState !== 2).length;
        if (alive === 0 && imagineState.running) {
          setImagineStatus("连接异常");
        }
      };
      imagineState.sseStreams.push(es);
    });
  }

  function startImagineWS(taskIDs, prompt, aspectRatio, allowFallback) {
    imagineState.effectiveMode = "ws";
    setImagineStatus("连接中 (WS)");
    closeImagineConnections(false);

    let opened = 0;
    let switched = false;

    if (allowFallback) {
      imagineState.fallbackTimer = setTimeout(() => {
        if (!imagineState.running || opened > 0 || switched) return;
        switched = true;
        showToast("WS 建连失败，自动切换 SSE", "info");
        startImagineSSE(taskIDs);
      }, 1500);
    }

    taskIDs.forEach((taskID) => {
      const protocol = window.location.protocol === "https:" ? "wss" : "ws";
      const url = `${protocol}://${window.location.host}/api/v1/admin/imagine/ws?task_id=${encodeURIComponent(taskID)}`;
      const ws = new WebSocket(url);

      ws.onopen = () => {
        opened += 1;
        updateImagineActiveCount();
        setImagineStatus("运行中 (WS)");
        try {
          ws.send(JSON.stringify({
            type: "start",
            prompt,
            aspect_ratio: aspectRatio,
            model: String(document.getElementById("imagineModel")?.value || "grok-imagine-image-lite").trim() || "grok-imagine-image-lite",
          }));
        } catch (err) {
          // ignore
        }
      };

      ws.onmessage = (event) => {
        try {
          handleImagineMessage(JSON.parse(event.data));
        } catch (err) {
          // ignore bad payload
        }
      };

      ws.onerror = () => {
        if (allowFallback && opened === 0 && !switched) {
          switched = true;
          startImagineSSE(taskIDs);
          return;
        }
        updateImagineActiveCount();
      };

      ws.onclose = () => {
        updateImagineActiveCount();
      };

      imagineState.wsSockets.push(ws);
    });
  }

  async function startImagine() {
    if (imagineState.running) {
      await stopImagine();
      return;
    }
    const promptInput = document.getElementById("imaginePrompt");
    const prompt = String(promptInput?.value || "").trim();
    if (!prompt) {
      showToast("请输入 Prompt", "error");
      return;
    }
    const ratio = String(document.getElementById("imagineRatio")?.value || "2:3");
    const runMode = imagineReadToggle("#imagineRunModeToggle", "imagineRunMode", "single");
    const quality = normalizeImagineQuality(imagineReadToggle("#imagineQualityToggle", "imagineQuality", "lite"));
    const models = imagineQualityModels(quality);
    const model = models[0];
    const nsfw = String(document.getElementById("imagineNSFW")?.value || "true") === "true";

    saveGrokToolsUIState({
      imagineRatio: ratio,
      imagineModel: model,
      imagineRunMode: runMode,
      imagineQuality: quality,
      imagineConcurrent: IMAGINE_BATCH_SIZE,
    });

    imagineState.running = true;
    imagineState.mode = runMode;
    imagineState.effectiveMode = "masonry";
    imagineState.abortController = new AbortController();
    imagineState.taskIDs = [];
    setImagineButtons(true);
    setImagineControlsDisabled(true);
    setImagineStatus("生成中");

    let round = 0;
    try {
      while (imagineState.running) {
        round += 1;
        const batch = createImagineBatch(prompt, ratio, quality, round);
        if (!batch) throw new Error("瀑布流容器不存在");
        setImagineStatus(`生成中 · 第 ${round} 轮`);
        const signal = imagineState.abortController?.signal;
        const results = await runImagineBatchSlots(batch, prompt, ratio, models, nsfw, signal);
        batch.ready = results.filter(Boolean).length;
        batch.failed = results.length - batch.ready;
        updateImagineBatchMeta(batch, true);

        const elapsed = batch.slots.reduce((sum, slot) => sum + (slot.status === "ready" ? 1 : 0), 0);
        const active = document.getElementById("imagineActive");
        if (active) active.textContent = imagineState.running ? "1" : "0";
        if (elapsed > 0) {
          const latency = document.getElementById("imagineLatency");
          if (latency) latency.textContent = "-";
        }

        if (!imagineState.running || runMode !== "continuous") break;
      }
      if (imagineState.running) {
        setImagineStatus("完成");
      }
    } catch (err) {
      if (!imagineState.abortController?.signal?.aborted) {
        setImagineStatus("错误");
        showToast(`启动失败: ${err.message || err}`, "error");
      }
    } finally {
      imagineState.running = false;
      imagineState.abortController = null;
      setImagineButtons(false);
      setImagineControlsDisabled(false);
      updateImagineActiveCount();
      if (document.activeElement !== promptInput) promptInput?.focus();
    }
  }

  async function stopImagine() {
    const taskIDs = imagineState.taskIDs.slice();
    imagineState.running = false;
    setImagineButtons(false);
    setImagineControlsDisabled(false);
    setImagineStatus("停止中");
    if (imagineState.abortController) {
      imagineState.abortController.abort();
      imagineState.abortController = null;
    }
    closeImagineConnections(true);
    imagineState.taskIDs = [];
    try {
      await stopImagineTasks(taskIDs);
    } catch (err) {
      // ignore
    }
    updateImagineActiveCount();
    setImagineStatus("已停止");
  }

  function clearImagineGrid() {
    const grid = document.getElementById("imagineGrid");
    const empty = document.getElementById("imagineEmpty");
    if (grid) grid.innerHTML = "";
    if (empty) {
      empty.hidden = false;
      empty.style.display = "";
      if (grid) grid.appendChild(empty);
    }
    imagineStreamImageMap.clear();
    imagineStreamSequence = 0;
    imagineSelectedImages.clear();
    imagineSelectionMode = false;
    imagineLightboxIndex = -1;
    const toolbar = document.getElementById("selectionToolbar");
    if (toolbar) toolbar.classList.add("hidden");
    resetImagineMetrics();
  }

  function createChatSession() {
    return {
      id: `${Date.now().toString(36)}${Math.random().toString(36).slice(2, 8)}`,
      title: "新会话",
      isDefaultTitle: true,
      createdAt: Date.now(),
      updatedAt: Date.now(),
      messages: [],
      model: chatState.model,
    };
  }

  function trimChatSessionMessages(session) {
    if (!session || !Array.isArray(session.messages)) return 0;
    const overflow = session.messages.length - MAX_CHAT_MESSAGES;
    if (overflow <= 0) return 0;
    session.messages = session.messages.slice(-MAX_CHAT_MESSAGES);
    session.updatedAt = Date.now();
    return overflow;
  }

  function isImageFileName(name) {
    return /\.(png|jpe?g|webp|gif|bmp|svg)$/i.test(String(name || "").trim());
  }

  function readFileAsDataURL(file) {
    return new Promise((resolve, reject) => {
      const reader = new FileReader();
      reader.onload = () => resolve(String(reader.result || ""));
      reader.onerror = () => reject(reader.error || new Error("读取文件失败"));
      reader.readAsDataURL(file);
    });
  }

  function saveChatSessions() {
    try {
      const sessions = chatState.sessions.map((session) => ({
        ...session,
        messages: Array.isArray(session.messages)
          ? session.messages.map((msg) => ({
              ...msg,
              attachment: msg?.attachment
                ? {
                    name: String(msg.attachment.name || ""),
                    type: String(msg.attachment.type || ""),
                  }
                : undefined,
            }))
          : [],
      }));
      localStorage.setItem(chatStorageKey, JSON.stringify({
        activeId: chatState.activeId,
        model: chatState.model,
        sessions,
      }));
    } catch (err) {
      // ignore storage failures
    }
  }

  function loadChatSessions() {
    try {
      const raw = localStorage.getItem(chatStorageKey);
      if (raw) {
        const parsed = JSON.parse(raw);
        if (parsed && Array.isArray(parsed.sessions)) {
          chatState.sessions = parsed.sessions;
          chatState.activeId = String(parsed.activeId || "");
          if (typeof parsed.model === "string" && parsed.model.trim()) {
            chatState.model = parsed.model.trim();
          }
        }
      }
    } catch (err) {
      chatState.sessions = [];
      chatState.activeId = "";
    }
    if (!Array.isArray(chatState.sessions) || chatState.sessions.length === 0) {
      const session = createChatSession();
      chatState.sessions = [session];
      chatState.activeId = session.id;
    }
    if (!chatState.sessions.find((item) => item && item.id === chatState.activeId)) {
      chatState.activeId = chatState.sessions[0].id;
    }
    chatState.sessions.forEach((session) => {
      if (session && typeof session.isDefaultTitle === "undefined") {
        session.isDefaultTitle = !session.title || session.title === "新会话";
      }
      trimChatSessionMessages(session);
    });
  }

  function activeChatSession() {
    return chatState.sessions.find((item) => item && item.id === chatState.activeId) || null;
  }

  function updateChatStatus(text, type) {
    const el = document.getElementById("grokChatStatus");
    if (!el) return;
    el.textContent = String(text || "");
    el.classList.remove("connected", "connecting", "error");
    if (type === "ok") el.classList.add("connected");
    if (type === "connecting") el.classList.add("connecting");
    if (type === "error") el.classList.add("error");
  }

  function escapeHTML(text) {
    const div = document.createElement("div");
    div.textContent = String(text == null ? "" : text);
    return div.innerHTML;
  }

  async function copyToClipboard(text) {
    const value = String(text || "").trim();
    if (!value) {
      showToast("暂无可复制内容", "info");
      return;
    }
    try {
      if (navigator.clipboard && navigator.clipboard.writeText) {
        await navigator.clipboard.writeText(value);
      } else {
        const temp = document.createElement("textarea");
        temp.value = value;
        temp.style.position = "fixed";
        temp.style.opacity = "0";
        document.body.appendChild(temp);
        temp.select();
        document.execCommand("copy");
        document.body.removeChild(temp);
      }
      showToast("已复制到剪贴板", "success");
    } catch (err) {
      showToast("复制失败", "error");
    }
  }

  function setRenderedHTML(el, html) {
    el.innerHTML = html;
  }

  function isSafeLinkURL(url) {
    const value = String(url || "").trim().toLowerCase();
    if (!value) return false;
    return /^(https?:|mailto:|tel:|\/(?!\/)|\.\.?\/|#)/.test(value);
  }

  function isSafeImageURL(url) {
    const value = String(url || "").trim().toLowerCase();
    if (!value) return false;
    return /^(https?:|data:image\/(?:png|jpe?g|gif|webp|bmp|ico);base64,|\/(?!\/)|\.\.?\/)/.test(value);
  }

  function renderBasicMarkdown(rawText) {
    const text = String(rawText || "").replace(/\\n/g, "\n");
    const escaped = escapeHTML(text);
    const codeBlocks = [];
    const fenced = escaped.replace(/```([a-zA-Z0-9_-]+)?\n([\s\S]*?)```/g, (_match, lang, code) => {
      const safeLang = lang ? escapeHTML(lang) : "";
      const html = `<pre class="code-block"><code${safeLang ? ` class="language-${safeLang}"` : ""}>${code}</code></pre>`;
      const token = `@@CODEBLOCK_${codeBlocks.length}@@`;
      codeBlocks.push(html);
      return token;
    });

    const renderInline = (value) => {
      const inlineCodes = [];
      let output = value.replace(/`([^`]+)`/g, (_m, code) => {
        const token = `@@INLINE_${inlineCodes.length}@@`;
        inlineCodes.push(`<code class="inline-code">${code}</code>`);
        return token;
      });
      output = output
        .replace(/\*\*([^*]+)\*\*/g, "<strong>$1</strong>")
        .replace(/\*([^*]+)\*/g, "<em>$1</em>")
        .replace(/~~([^~]+)~~/g, "<del>$1</del>");
      output = output.replace(/!\[([^\]]*)\]\(([^)]+)\)/g, (_m, alt, url) => {
        const safeAlt = escapeHTML(alt || "image");
        if (!isSafeImageURL(url)) return safeAlt;
        return `<img src="${escapeHTML(url || "")}" alt="${safeAlt}" loading="lazy">`;
      });
      output = output.replace(/\[([^\]]+)\]\(([^)]+)\)/g, (_m, label, url) => {
        const safeLabel = escapeHTML(label || "");
        if (!isSafeLinkURL(url)) return safeLabel;
        return `<a href="${escapeHTML(url || "")}" target="_blank" rel="noopener">${safeLabel}</a>`;
      });
      output = output.replace(/(data:image\/[a-zA-Z0-9.+-]+;base64,[A-Za-z0-9+/=]+)/g, (uri) => {
        if (!isSafeImageURL(uri)) return "";
        return `<img src="${escapeHTML(uri)}" alt="image" loading="lazy">`;
      });
      inlineCodes.forEach((html, index) => {
        output = output.replace(new RegExp(`@@INLINE_${index}@@`, "g"), html);
      });
      return output;
    };

    const lines = fenced.split(/\r?\n/);
    const htmlParts = [];
    let inUl = false;
    let inTaskUl = false;
    let inOl = false;
    let inTable = false;
    let paragraphLines = [];

    const closeLists = () => {
      if (inUl) {
        htmlParts.push("</ul>");
        inUl = false;
        inTaskUl = false;
      }
      if (inOl) {
        htmlParts.push("</ol>");
        inOl = false;
      }
    };
    const closeTable = () => {
      if (inTable) {
        htmlParts.push("</tbody></table></div>");
        inTable = false;
      }
    };
    const flushParagraph = () => {
      if (!paragraphLines.length) return;
      htmlParts.push(`<p>${renderInline(paragraphLines.join("<br>"))}</p>`);
      paragraphLines = [];
    };
    const isTableSeparator = (line) => /^\s*\|?(?:\s*:?-+:?\s*\|)+\s*$/.test(line);
    const splitTableRow = (line) => line.trim().replace(/^\|/, "").replace(/\|$/, "").split("|").map((cell) => cell.trim());

    for (let i = 0; i < lines.length; i += 1) {
      const line = lines[i];
      const trimmed = line.trim();
      if (!trimmed) {
        flushParagraph();
        closeLists();
        closeTable();
        continue;
      }

      const codeTokenMatch = trimmed.match(/^@@CODEBLOCK_(\d+)@@$/);
      if (codeTokenMatch) {
        flushParagraph();
        closeLists();
        closeTable();
        htmlParts.push(trimmed);
        continue;
      }

      const headingMatch = trimmed.match(/^(#{1,6})\s+(.*)$/);
      if (headingMatch) {
        flushParagraph();
        closeLists();
        closeTable();
        const level = headingMatch[1].length;
        htmlParts.push(`<h${level}>${renderInline(headingMatch[2])}</h${level}>`);
        continue;
      }

      if (/^(-{3,}|\*{3,}|_{3,})$/.test(trimmed)) {
        flushParagraph();
        closeLists();
        closeTable();
        htmlParts.push("<hr>");
        continue;
      }

      if (/^\s*>/.test(line)) {
        flushParagraph();
        closeLists();
        closeTable();
        const quoteLines = [];
        let j = i;
        for (; j < lines.length; j += 1) {
          const currentLine = lines[j];
          if (!/^\s*>/.test(currentLine)) break;
          quoteLines.push(currentLine.replace(/^\s*>\s?/, ""));
        }
        i = j - 1;
        htmlParts.push(`<blockquote>${renderBasicMarkdown(quoteLines.join("\n"))}</blockquote>`);
        continue;
      }

      if (trimmed.includes("|")) {
        const nextLine = lines[i + 1] || "";
        if (!inTable && isTableSeparator(nextLine.trim())) {
          flushParagraph();
          closeLists();
          const headers = splitTableRow(trimmed);
          htmlParts.push('<div class="table-wrap"><table><thead><tr>');
          headers.forEach((cell) => htmlParts.push(`<th>${renderInline(cell)}</th>`));
          htmlParts.push("</tr></thead><tbody>");
          inTable = true;
          i += 1;
          continue;
        }
        if (inTable && !isTableSeparator(trimmed)) {
          const cells = splitTableRow(trimmed);
          htmlParts.push("<tr>");
          cells.forEach((cell) => htmlParts.push(`<td>${renderInline(cell)}</td>`));
          htmlParts.push("</tr>");
          continue;
        }
      }

      const taskMatch = trimmed.match(/^[-*+•·]\s+\[([ xX])\]\s+(.*)$/);
      if (taskMatch) {
        flushParagraph();
        if (inUl && !inTaskUl) closeLists();
        if (!inUl) {
          closeLists();
          closeTable();
          htmlParts.push('<ul class="task-list">');
          inUl = true;
          inTaskUl = true;
        }
        const checked = taskMatch[1].toLowerCase() === "x";
        htmlParts.push(`<li class="task-item"><input type="checkbox" disabled${checked ? " checked" : ""}>${renderInline(taskMatch[2])}</li>`);
        continue;
      }

      const ulMatch = trimmed.match(/^[-*+•·]\s+(.*)$/);
      if (ulMatch) {
        flushParagraph();
        if (!inUl) {
          closeLists();
          closeTable();
          htmlParts.push("<ul>");
          inUl = true;
          inTaskUl = false;
        }
        htmlParts.push(`<li>${renderInline(ulMatch[1])}</li>`);
        continue;
      }

      const olMatch = trimmed.match(/^\d+[.)、]\s+(.*)$/);
      if (olMatch) {
        flushParagraph();
        if (!inOl) {
          closeLists();
          closeTable();
          htmlParts.push("<ol>");
          inOl = true;
        }
        htmlParts.push(`<li>${renderInline(olMatch[1])}</li>`);
        continue;
      }

      paragraphLines.push(trimmed);
    }

    flushParagraph();
    closeLists();
    closeTable();

    let output = htmlParts.join("");
    codeBlocks.forEach((html, index) => {
      output = output.replace(`@@CODEBLOCK_${index}@@`, html);
    });
    return output;
  }

  function parseThinkSections(raw) {
    const input = String(raw || "");
    const parts = [];
    let cursor = 0;
    while (cursor < input.length) {
      const start = input.indexOf("<think>", cursor);
      if (start === -1) {
        parts.push({ type: "text", value: input.slice(cursor) });
        break;
      }
      if (start > cursor) {
        parts.push({ type: "text", value: input.slice(cursor, start) });
      }
      const thinkStart = start + 7;
      const end = input.indexOf("</think>", thinkStart);
      if (end === -1) {
        parts.push({ type: "think", value: input.slice(thinkStart), open: true });
        cursor = input.length;
      } else {
        parts.push({ type: "think", value: input.slice(thinkStart, end), open: false });
        cursor = end + 8;
      }
    }
    return parts;
  }

  function parseRolloutBlocks(text) {
    const lines = String(text || "").split(/\r?\n/);
    const blocks = [];
    let current = null;
    for (const line of lines) {
      const match = line.match(/^\s*\[([^\]]+)\]\[([^\]]+)\]\s*(.*)$/);
      if (match) {
        if (current) blocks.push(current);
        current = { id: match[1], type: match[2], lines: [] };
        if (match[3]) current.lines.push(match[3]);
        continue;
      }
      if (current) {
        current.lines.push(line);
      }
    }
    if (current) blocks.push(current);
    return blocks;
  }

  function parseAgentSections(text) {
    const lines = String(text || "").split(/\r?\n/);
    const sections = [];
    let current = { title: null, lines: [] };
    let hasAgentHeading = false;
    for (const line of lines) {
      const trimmed = line.trim();
      if (!trimmed) {
        current.lines.push(line);
        continue;
      }
      const agentMatch = trimmed.match(/^(Grok\s+Leader|(?:Grok\s+)?Agent\s*\d+)$/i);
      if (agentMatch) {
        hasAgentHeading = true;
        if (current.lines.length) {
          sections.push(current);
        }
        current = { title: agentMatch[1], lines: [] };
        continue;
      }
      current.lines.push(line);
    }
    if (current.lines.length) {
      sections.push(current);
    }
    if (!hasAgentHeading) {
      return [{ title: null, lines }];
    }
    return sections;
  }

  const toolTypeMap = {
    websearch: { icon: "", label: "网页搜索" },
    searchimage: { icon: "", label: "图片搜索" },
    agentthink: { icon: "", label: "Agent Think" },
  };
  const defaultToolType = { icon: "", label: "工具" };

  function getToolMeta(typeStr) {
    const key = String(typeStr || "").trim().toLowerCase().replace(/\s+/g, "");
    return toolTypeMap[key] || defaultToolType;
  }

  function renderThinkContent(text, openAll) {
    const sections = parseAgentSections(text);
    if (!sections.length) {
      return renderBasicMarkdown(text);
    }
    const renderGroups = (blocks, openAllGroups) => {
      const groups = [];
      const map = new Map();
      for (const block of blocks) {
        const key = block.id;
        let group = map.get(key);
        if (!group) {
          group = { id: key, items: [] };
          map.set(key, group);
          groups.push(group);
        }
        group.items.push(block);
      }
      return groups.map((group) => {
        const items = group.items.map((item) => {
          const body = renderBasicMarkdown(item.lines.join("\n").trim());
          const typeKey = String(item.type || "").trim().toLowerCase().replace(/\s+/g, "");
          const typeAttr = escapeHTML(typeKey);
          const meta = getToolMeta(item.type);
          const iconHtml = meta.icon ? `<span class="think-tool-icon">${meta.icon}</span>` : "";
          const typeLabel = `${iconHtml}${escapeHTML(meta.label)}`;
          return `<div class="think-item-row think-tool-card" data-tool-type="${typeAttr}"><div class="think-item-type" data-type="${typeAttr}">${typeLabel}</div><div class="think-item-body">${body || "<em>空</em>"}</div></div>`;
        }).join("");
        const title = escapeHTML(group.id);
        const openAttr = openAllGroups ? " open" : "";
        return `<details class="think-rollout-group"${openAttr}><summary><span class="think-rollout-title">${title}</span></summary><div class="think-rollout-body">${items}</div></details>`;
      }).join("");
    };

    const agentBlocks = sections.map((section, idx) => {
      const blocks = parseRolloutBlocks(section.lines.join("\n"));
      const inner = blocks.length
        ? renderGroups(blocks, openAll)
        : `<div class="think-rollout-body">${renderBasicMarkdown(section.lines.join("\n").trim())}</div>`;
      if (!section.title) {
        return `<div class="think-agent-items">${inner}</div>`;
      }
      const title = escapeHTML(section.title);
      const openAttr = openAll ? " open" : (idx === 0 ? " open" : "");
      return `<details class="think-agent"${openAttr}><summary>${title}</summary><div class="think-agent-items">${inner}</div></details>`;
    });
    return `<div class="think-agents">${agentBlocks.join("")}</div>`;
  }

  function renderAssistantContent(raw) {
    const parts = parseThinkSections(String(raw || ""));
    return parts.map((part) => {
      if (part.type === "think") {
        const body = renderThinkContent(part.value.trim(), part.open);
        const openAttr = part.open ? " open" : "";
        return `<details class="think-block" data-think="true"${openAttr}><summary class="think-summary">思考过程</summary><div class="think-content">${body || "<em>空</em>"}</div></details>`;
      }
      return renderBasicMarkdown(part.value);
    }).join("");
  }

  function applyImageGrid(root) {
    if (!root) return;
    const isIgnorable = (node) => {
      if (node.nodeType === Node.TEXT_NODE) {
        return !node.textContent.trim();
      }
      return node.nodeType === Node.ELEMENT_NODE && node.tagName === "BR";
    };

    const isImageLink = (node) => {
      if (!node || node.nodeType !== Node.ELEMENT_NODE || node.tagName !== "A") return false;
      const children = Array.from(node.childNodes);
      if (!children.length) return false;
      return children.every((child) => {
        if (child.nodeType === Node.TEXT_NODE) {
          return !child.textContent.trim();
        }
        return child.nodeType === Node.ELEMENT_NODE && child.tagName === "IMG";
      });
    };

    const extractImageItems = (node) => {
      if (!node || node.nodeType !== Node.ELEMENT_NODE) return null;
      if (node.classList && node.classList.contains("img-grid")) return null;
      if (node.tagName === "IMG") {
        return { items: [node], removeNode: null };
      }
      if (isImageLink(node)) {
        return { items: [node], removeNode: null };
      }
      if (node.tagName === "P") {
        const items = [];
        const children = Array.from(node.childNodes);
        if (!children.length) return null;
        for (const child of children) {
          if (child.nodeType === Node.TEXT_NODE) {
            if (!child.textContent.trim()) continue;
            return null;
          }
          if (child.nodeType === Node.ELEMENT_NODE) {
            if (child.tagName === "IMG" || isImageLink(child)) {
              items.push(child);
              continue;
            }
            if (child.tagName === "BR") continue;
            return null;
          }
          return null;
        }
        if (!items.length) return null;
        return { items, removeNode: node };
      }
      return null;
    };

    const wrapImagesInContainer = (container) => {
      const children = Array.from(container.childNodes);
      let group = [];
      let groupStart = null;
      let removeNodes = [];

      const flush = () => {
        if (group.length < 2) {
          group = [];
          groupStart = null;
          removeNodes = [];
          return;
        }
        const wrapper = document.createElement("div");
        wrapper.className = "img-grid";
        const cols = Math.min(4, group.length);
        wrapper.style.setProperty("--cols", String(cols));
        if (groupStart) {
          container.insertBefore(wrapper, groupStart);
        } else {
          container.appendChild(wrapper);
        }
        group.forEach((img) => wrapper.appendChild(img));
        removeNodes.forEach((n) => n.parentNode && n.parentNode.removeChild(n));
        group = [];
        groupStart = null;
        removeNodes = [];
      };

      children.forEach((node) => {
        if (group.length && isIgnorable(node)) {
          removeNodes.push(node);
          return;
        }
        const extracted = extractImageItems(node);
        if (extracted && extracted.items.length) {
          if (!groupStart) groupStart = node;
          group.push(...extracted.items);
          if (extracted.removeNode) {
            removeNodes.push(extracted.removeNode);
          }
          return;
        }
        flush();
      });
      flush();
    };

    const containers = [root, ...root.querySelectorAll(".think-content, .think-item-body, .think-rollout-body, .think-agent-items")];
    containers.forEach((container) => {
      if (!container || container.closest(".img-grid")) return;
      if (!container.querySelector || !container.querySelector("img")) return;
      wrapImagesInContainer(container);
    });
  }

  function enhanceBrokenImages(root) {
    if (!root) return;
    const images = root.querySelectorAll("img");
    images.forEach((img) => {
      if (img.dataset.retryBound) return;
      img.dataset.retryBound = "1";
      img.addEventListener("error", () => {
        if (img.dataset.failed) return;
        img.dataset.failed = "1";
        const wrapper = document.createElement("button");
        wrapper.type = "button";
        wrapper.className = "img-retry";
        wrapper.textContent = "点击重试";
        wrapper.addEventListener("click", () => {
          wrapper.classList.add("loading");
          const original = img.getAttribute("src") || "";
          const cacheBust = original.includes("?") ? "&" : "?";
          img.dataset.failed = "";
          img.src = `${original}${cacheBust}t=${Date.now()}`;
        });
        img.replaceWith(wrapper);
      });
      img.addEventListener("load", () => {
        if (img.dataset.failed) {
          img.dataset.failed = "";
        }
      });
    });
  }

  function updateThinkSummary(root, elapsedSec) {
    if (!root) return;
    const summaries = root.querySelectorAll(".think-summary");
    if (!summaries.length) return;
    let text = "思考过程";
    if (typeof elapsedSec === "number") {
      text = elapsedSec > 0 ? `思考 ${elapsedSec}s` : "思考完成";
    } else {
      text = "思考中";
    }
    summaries.forEach((node) => {
      node.textContent = text;
      const block = node.closest(".think-block");
      if (!block) return;
      if (typeof elapsedSec === "number") {
        block.removeAttribute("data-thinking");
      } else {
        block.setAttribute("data-thinking", "true");
      }
    });
  }

  function renderUserContent(content, attachment) {
    const wrapper = document.createElement("div");
    const text = document.createElement("div");
    text.className = "message-content";
    text.textContent = String(content || "");
    wrapper.appendChild(text);
    if (attachment && attachment.name) {
      const badge = document.createElement("div");
      badge.className = "message-attachment";
      badge.textContent = `附件: ${attachment.name}`;
      wrapper.appendChild(badge);
    }
    return wrapper;
  }

  function appendChatMessage(role, content, attachment) {
    const log = document.getElementById("grokChatLog");
    const empty = document.getElementById("grokChatEmpty");
    if (!log) return null;
    if (empty) empty.style.display = "none";
    const row = document.createElement("div");
    row.className = `message-row ${role}`;
    const bubble = document.createElement("div");
    bubble.className = "message-bubble";
    let contentEl = document.createElement("div");
    contentEl.className = role === "assistant" ? "message-content rendered" : "message-content";
    if (role === "assistant") {
      setRenderedHTML(contentEl, renderAssistantContent(content || ""));
      applyImageGrid(contentEl);
      if (String(content || "").includes("<think>")) {
        updateThinkSummary(contentEl, 0);
      }
      enhanceBrokenImages(contentEl);
      bubble.appendChild(contentEl);
    } else {
      contentEl = renderUserContent(content, attachment);
      bubble.appendChild(contentEl);
    }
    row.appendChild(bubble);
    const actions = document.createElement("div");
    actions.className = "message-actions";
    const copyBtn = document.createElement("button");
    copyBtn.type = "button";
    copyBtn.className = "action-btn";
    copyBtn.textContent = "复制";
    copyBtn.addEventListener("click", () => {
      const text = role === "assistant"
        ? String(bubble.innerText || "").trim()
        : String(content || "");
      copyToClipboard(text);
    });
    actions.appendChild(copyBtn);
    if (role === "user") {
      const editBtn = document.createElement("button");
      editBtn.type = "button";
      editBtn.className = "action-btn";
      editBtn.textContent = "编辑";
      editBtn.addEventListener("click", () => startEditChatMessage(row, content, attachment));
      actions.appendChild(editBtn);
    } else {
      const retryBtn = document.createElement("button");
      retryBtn.type = "button";
      retryBtn.className = "action-btn";
      retryBtn.textContent = "重试";
      retryBtn.addEventListener("click", () => retryAssistantMessage(row));
      const editBtn = document.createElement("button");
      editBtn.type = "button";
      editBtn.className = "action-btn";
      editBtn.textContent = "编辑";
      editBtn.addEventListener("click", () => startEditAssistantMessage(row));
      actions.appendChild(retryBtn);
      actions.appendChild(editBtn);
    }
    row.appendChild(actions);
    log.appendChild(row);
    log.scrollTop = log.scrollHeight;
    return contentEl;
  }

  function startEditChatMessage(row, content, attachment) {
    const session = activeChatSession();
    if (!row || !session) return;
    const messages = Array.isArray(session.messages) ? session.messages : [];
    const targetIndex = messages.findIndex((msg) => msg && msg.role === "user" && String(msg.content || "") === String(content || ""));
    if (targetIndex < 0) return;

    const bubble = row.querySelector(".message-bubble");
    const actions = row.querySelector(".message-actions");
    if (!bubble || !actions) return;
    bubble.innerHTML = "";
    actions.innerHTML = "";

    const textarea = document.createElement("textarea");
    textarea.className = "edit-msg-input";
    textarea.value = String(content || "");
    const actionWrap = document.createElement("div");
    actionWrap.className = "edit-msg-actions";
    const saveBtn = document.createElement("button");
    saveBtn.type = "button";
    saveBtn.className = "btn btn-primary";
    saveBtn.textContent = "保存";
    const cancelBtn = document.createElement("button");
    cancelBtn.type = "button";
    cancelBtn.className = "btn btn-outline";
    cancelBtn.textContent = "取消";

    const cancel = () => rerenderChatThread();
    saveBtn.addEventListener("click", () => {
      const next = String(textarea.value || "").trim();
      if (!next) {
        showToast("消息不能为空", "info");
        return;
      }
      messages[targetIndex].content = next;
      if (attachment) {
        messages[targetIndex].attachment = attachment;
      }
      session.updatedAt = Date.now();
      saveChatSessions();
      renderChatSessions();
      rerenderChatThread();
    });
    cancelBtn.addEventListener("click", cancel);
    textarea.addEventListener("keydown", (event) => {
      if ((event.metaKey || event.ctrlKey) && event.key === "Enter") {
        event.preventDefault();
        saveBtn.click();
      } else if (event.key === "Escape") {
        event.preventDefault();
        cancel();
      }
    });

    bubble.appendChild(textarea);
    actionWrap.appendChild(saveBtn);
    actionWrap.appendChild(cancelBtn);
    bubble.appendChild(actionWrap);
    textarea.focus();
    textarea.select();
  }

  function startEditAssistantMessage(row) {
    const session = activeChatSession();
    if (!row || !session) return;
    const rows = Array.from(document.querySelectorAll("#grokChatLog .message-row"));
    const rowIndex = rows.indexOf(row);
    const messages = Array.isArray(session.messages) ? session.messages : [];
    if (rowIndex < 0 || rowIndex >= messages.length) return;
    const msg = messages[rowIndex];
    if (!msg || msg.role !== "assistant") return;

    const bubble = row.querySelector(".message-bubble");
    const actions = row.querySelector(".message-actions");
    if (!bubble || !actions) return;
    bubble.innerHTML = "";
    actions.classList.add("hidden");

    const textarea = document.createElement("textarea");
    textarea.className = "edit-msg-input";
    textarea.value = String(msg.content || "");
    const actionWrap = document.createElement("div");
    actionWrap.className = "edit-msg-actions";
    const saveBtn = document.createElement("button");
    saveBtn.type = "button";
    saveBtn.className = "btn btn-primary";
    saveBtn.textContent = "保存";
    const cancelBtn = document.createElement("button");
    cancelBtn.type = "button";
    cancelBtn.className = "btn btn-outline";
    cancelBtn.textContent = "取消";
    const cancel = () => rerenderChatThread();
    saveBtn.addEventListener("click", () => {
      const next = String(textarea.value || "").trim();
      if (!next) {
        showToast("消息不能为空", "info");
        return;
      }
      msg.content = next;
      session.updatedAt = Date.now();
      saveChatSessions();
      renderChatSessions();
      rerenderChatThread();
    });
    cancelBtn.addEventListener("click", cancel);
    textarea.addEventListener("keydown", (event) => {
      if ((event.metaKey || event.ctrlKey) && event.key === "Enter") {
        event.preventDefault();
        saveBtn.click();
      } else if (event.key === "Escape") {
        event.preventDefault();
        cancel();
      }
    });
    bubble.appendChild(textarea);
    actionWrap.appendChild(saveBtn);
    actionWrap.appendChild(cancelBtn);
    bubble.appendChild(actionWrap);
    textarea.focus();
    textarea.select();
  }

  async function requestChatCompletion(session, contentEl) {
    let assistantText = "";
    let hasThink = false;
    let thinkStartAt = null;
    let thinkElapsed = null;
    let thinkAutoCollapsed = false;
    chatState.sending = true;
    chatState.abortController = new AbortController();
    setChatSendButtonState(true);
    updateChatStatus("连接中...", "connecting");

    const updateAssistantView = () => {
      if (!contentEl) return;
      let savedThinkStates = null;
      if (hasThink && thinkAutoCollapsed) {
        const blocks = contentEl.querySelectorAll(".think-block[data-think=\"true\"]");
        if (blocks.length) {
          savedThinkStates = Array.from(blocks).map((b) => b.hasAttribute("open"));
        }
      }
      setRenderedHTML(contentEl, renderAssistantContent(assistantText));
      if (hasThink) {
        updateThinkSummary(contentEl, typeof thinkElapsed === "number" ? thinkElapsed : null);
        const blocks = contentEl.querySelectorAll(".think-block[data-think=\"true\"]");
        blocks.forEach((block, index) => {
          if (savedThinkStates && index < savedThinkStates.length) {
            if (savedThinkStates[index]) {
              block.setAttribute("open", "");
            } else {
              block.removeAttribute("open");
            }
          } else if (thinkElapsed === null || thinkElapsed === undefined) {
            block.setAttribute("open", "");
          } else if (!thinkAutoCollapsed) {
            block.removeAttribute("open");
            thinkAutoCollapsed = true;
          }
        });
      }
      applyImageGrid(contentEl);
      enhanceBrokenImages(contentEl);
      if (hasThink) {
        const thinkNodes = contentEl.querySelectorAll(".think-content");
        thinkNodes.forEach((node) => {
          node.scrollTop = node.scrollHeight;
        });
      }
      const log = document.getElementById("grokChatLog");
      if (log) log.scrollTop = log.scrollHeight;
    };

    try {
      const payload = buildChatPayload();
      const res = await fetch("/grok/v1/chat/completions", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(payload),
        signal: chatState.abortController.signal,
      });
      if (handleUnauthorized(res)) return { aborted: false, text: "" };
      if (!res.ok || !res.body) {
        throw new Error(await res.text());
      }
      const reader = res.body.getReader();
      const decoder = new TextDecoder();
      let buffer = "";
      while (true) {
        const { value, done } = await reader.read();
        if (done) break;
        buffer += decoder.decode(value, { stream: true });
        let idx = buffer.indexOf("\n\n");
        while (idx >= 0) {
          const chunk = buffer.slice(0, idx);
          buffer = buffer.slice(idx + 2);
          const lines = chunk.split("\n");
          let data = "";
          lines.forEach((line) => {
            if (line.startsWith("data:")) {
              data += line.slice(5).trimStart();
            }
          });
          if (!data || data === "[DONE]") {
            idx = buffer.indexOf("\n\n");
            continue;
          }
          let payloadChunk = null;
          try {
            payloadChunk = JSON.parse(data);
          } catch (err) {
            idx = buffer.indexOf("\n\n");
            continue;
          }
          const choice = payloadChunk?.choices?.[0];
          const delta = typeof choice?.delta?.content === "string" ? choice.delta.content : "";
          const finalContent = typeof choice?.message?.content === "string" ? choice.message.content : "";
          if (delta) {
            assistantText += delta;
          } else if (finalContent) {
            assistantText = finalContent;
          }
          if (!hasThink && assistantText.includes("<think>")) {
            hasThink = true;
            thinkStartAt = Date.now();
            thinkElapsed = null;
          }
          if (hasThink && thinkStartAt && thinkElapsed === null && assistantText.includes("</think>")) {
            thinkElapsed = Math.max(1, Math.round((Date.now() - thinkStartAt) / 1000));
          }
          updateAssistantView();
          idx = buffer.indexOf("\n\n");
        }
      }
      session.messages.push({ role: "assistant", content: assistantText.trim() });
      session.updatedAt = Date.now();
      const trimmed = trimChatSessionMessages(session);
      if (trimmed > 0) {
        rerenderChatThread();
      }
      saveChatSessions();
      updateChatStatus("完成", "ok");
      return { aborted: false, text: assistantText.trim() };
    } catch (err) {
      if (err && err.name === "AbortError") {
        if (contentEl && !assistantText.trim()) {
          assistantText = "[stopped]";
          updateAssistantView();
        }
        updateChatStatus("已停止", "error");
        return { aborted: true, text: assistantText.trim() };
      }
      if (contentEl) {
        assistantText = `[error] ${err.message || err}`;
        updateAssistantView();
      }
      updateChatStatus(err.message || "发送失败", "error");
      return { aborted: false, text: "" };
    } finally {
      chatState.sending = false;
      chatState.abortController = null;
      setChatSendButtonState(false);
      renderChatSessions();
      saveChatSessions();
    }
  }

  async function retryAssistantMessage(row) {
    if (chatState.sending) return;
    const session = activeChatSession();
    if (!row || !session) return;
    const rows = Array.from(document.querySelectorAll("#grokChatLog .message-row"));
    const rowIndex = rows.indexOf(row);
    const messages = Array.isArray(session.messages) ? session.messages : [];
    if (rowIndex < 0 || rowIndex >= messages.length) return;
    if (messages[rowIndex]?.role !== "assistant") return;

    let lastUserIndex = -1;
    for (let i = rowIndex - 1; i >= 0; i -= 1) {
      if (messages[i]?.role === "user") {
        lastUserIndex = i;
        break;
      }
    }
    if (lastUserIndex < 0) {
      showToast("没有可重试的上文", "info");
      return;
    }

    session.messages = messages.slice(0, lastUserIndex + 1);
    session.updatedAt = Date.now();
    saveChatSessions();
    renderChatSessions();
    rerenderChatThread();
    const contentEl = appendChatMessage("assistant", "");
    await requestChatCompletion(session, contentEl);
  }

  function rerenderChatThread() {
    const log = document.getElementById("grokChatLog");
    const empty = document.getElementById("grokChatEmpty");
    if (!log) return;
    log.innerHTML = "";
    const session = activeChatSession();
    const messages = Array.isArray(session?.messages) ? session.messages : [];
    if (messages.length === 0) {
      if (empty) {
        empty.style.display = "block";
        log.appendChild(empty);
      }
      return;
    }
    messages.forEach((msg) => appendChatMessage(msg.role, msg.content, msg.attachment));
  }

  function renderChatSessions() {
    const list = document.getElementById("grokSessionList");
    if (!list) return;
    list.innerHTML = "";
    chatState.sessions.forEach((session) => {
      const item = document.createElement("button");
      item.type = "button";
      item.className = `session-item${session.id === chatState.activeId ? " active" : ""}`;
      item.dataset.id = session.id;

      const title = document.createElement("span");
      title.className = "session-title";
      title.textContent = session.title || "新会话";
      title.addEventListener("dblclick", (event) => {
        event.stopPropagation();
        startRenameChatSession(session.id, title);
      });

      const meta = document.createElement("span");
      meta.className = "session-meta";
      meta.textContent = relativeTime(session.updatedAt);

      const delBtn = document.createElement("button");
      delBtn.type = "button";
      delBtn.className = "session-delete";
      delBtn.title = "删除";
      delBtn.textContent = "×";
      delBtn.addEventListener("click", (event) => {
        event.stopPropagation();
        deleteChatSession(session.id);
      });

      item.addEventListener("click", () => {
        switchChatSession(session.id);
        if (isMobileChatSidebar()) closeChatSidebar();
      });

      item.appendChild(title);
      item.appendChild(meta);
      item.appendChild(delBtn);
      list.appendChild(item);
    });
  }

  function syncChatModelUI() {
    const label = document.getElementById("grokModelLabel");
    if (label) label.textContent = chatState.model;
    const session = activeChatSession();
    if (session) {
      session.model = chatState.model;
    }
  }

  function renderChatModelDropdown() {
    const dropdown = document.getElementById("grokModelDropdown");
    if (!dropdown) return;
    dropdown.innerHTML = "";
    chatState.models.forEach((model) => {
      const btn = document.createElement("button");
      btn.type = "button";
      btn.className = `model-option${model === chatState.model ? " active" : ""}`;
      btn.textContent = model;
      btn.dataset.model = model;
      dropdown.appendChild(btn);
    });
  }

  function preferredConsoleModel(models) {
    const list = Array.isArray(models) ? models : [];
    for (const model of ["grok-4.3", "grok-4.3-latest", "grok-latest"]) {
      if (list.includes(model)) return model;
    }
    return list[0] || "grok-4.3";
  }

  async function loadChatModels() {
    try {
      const res = await fetch("/grok/v1/models");
      if (handleUnauthorized(res)) return;
      if (!res.ok) return;
      const data = await res.json();
      const models = Array.isArray(data?.data)
        ? data.data
            .map((item) => String(item?.id || "").trim())
            .filter((id) => id && !id.includes("imagine"))
        : [];
      if (models.length === 0) return;
      chatState.models = models;
      if (!models.includes(chatState.model)) {
        chatState.model = preferredConsoleModel(models);
      }
    } catch (err) {
      // ignore model fetch failures and keep fallback list
    }
  }

  function ensureChatTitle(session) {
    if (!session || !Array.isArray(session.messages) || session.messages.length === 0) return;
    if (session.isDefaultTitle === false) return;
    const firstUser = session.messages.find((msg) => msg && msg.role === "user" && String(msg.content || "").trim());
    if (!firstUser) return;
    session.title = String(firstUser.content || "").replace(/\s+/g, " ").trim().slice(0, 20) || "新会话";
    session.isDefaultTitle = false;
  }

  function renameChatSession(id, newTitle) {
    const session = chatState.sessions.find((item) => item && item.id === id);
    if (!session) return;
    const trimmed = String(newTitle || "").trim();
    session.title = trimmed || "新会话";
    session.isDefaultTitle = !trimmed;
    session.updatedAt = Date.now();
    saveChatSessions();
    renderChatSessions();
  }

  function deleteChatSession(id) {
    const idx = chatState.sessions.findIndex((item) => item && item.id === id);
    if (idx < 0) return;
    chatState.sessions.splice(idx, 1);
    if (chatState.sessions.length === 0) {
      const session = createChatSession();
      chatState.sessions = [session];
      chatState.activeId = session.id;
    } else if (chatState.activeId === id) {
      chatState.activeId = chatState.sessions[Math.max(0, idx - 1)].id;
    }
    renderChatSessions();
    rerenderChatThread();
    saveChatSessions();
  }

  function startRenameChatSession(sessionId, titleEl) {
    const session = chatState.sessions.find((item) => item && item.id === sessionId);
    if (!session || !titleEl || !titleEl.parentNode) return;
    const input = document.createElement("input");
    input.type = "text";
    input.className = "session-rename-input";
    input.value = String(session.title || "");
    input.maxLength = 40;
    titleEl.replaceWith(input);
    input.focus();
    input.select();
    const commit = () => renameChatSession(sessionId, input.value);
    input.addEventListener("blur", commit);
    input.addEventListener("keydown", (event) => {
      if (event.key === "Enter") {
        event.preventDefault();
        input.blur();
      }
      if (event.key === "Escape") {
        input.value = session.title || "新会话";
        input.blur();
      }
    });
  }

  function switchChatSession(id) {
    if (!id || id === chatState.activeId) return;
    chatState.activeId = id;
    const session = activeChatSession();
    if (session && session.model) {
      chatState.model = session.model;
    }
    syncChatModelUI();
    renderChatModelDropdown();
    renderChatSessions();
    rerenderChatThread();
    saveChatSessions();
  }

  function newChatSession() {
    const session = createChatSession();
    chatState.sessions.unshift(session);
    chatState.activeId = session.id;
    syncChatModelUI();
    renderChatModelDropdown();
    renderChatSessions();
    rerenderChatThread();
    saveChatSessions();
    updateChatStatus("就绪");
  }

  function buildChatPayload() {
    const session = activeChatSession();
    if (!session) {
      throw new Error("missing chat session");
    }
    const systemPrompt = String(document.getElementById("grokSystemInput")?.value || "").trim();
    const messages = [];
    if (systemPrompt) {
      messages.push({ role: "system", content: systemPrompt });
    }
    session.messages.forEach((msg) => {
      if (msg.role !== "user" || !msg.attachment?.dataUrl) {
        messages.push({ role: msg.role, content: msg.content });
        return;
      }
      const parts = [];
      if (msg.content) {
        parts.push({ type: "text", text: String(msg.content || "") });
      }
      parts.push({
        type: "file",
        file: {
          file_data: msg.attachment.dataUrl,
        },
      });
      messages.push({ role: msg.role, content: parts });
    });
    return {
      model: chatState.model,
      stream: true,
      temperature: Number(document.getElementById("grokTempRange")?.value || 0.8),
      top_p: Number(document.getElementById("grokTopPRange")?.value || 0.95),
      messages,
    };
  }

  async function sendChatMessage() {
    if (chatState.sending) return;
    const input = document.getElementById("grokPromptInput");
    const prompt = String(input?.value || "").trim();
    if (!prompt) return;
    const attachment = chatState.pendingFile ? { ...chatState.pendingFile } : null;
    const session = activeChatSession();
    if (!session) return;
    session.messages.push({ role: "user", content: prompt, attachment });
    session.updatedAt = Date.now();
    trimChatSessionMessages(session);
    ensureChatTitle(session);
    renderChatSessions();
    rerenderChatThread();
    if (input) {
      input.value = "";
      input.style.height = "40px";
    }

    clearPendingChatFile();
    const contentEl = appendChatMessage("assistant", "");
    await requestChatCompletion(session, contentEl);
  }

  function bindChatEvents() {
    const newBtn = document.getElementById("grokChatNewBtn");
    const list = document.getElementById("grokSessionList");
    const sendBtn = document.getElementById("grokSendBtn");
    const input = document.getElementById("grokPromptInput");
    const modelChip = document.getElementById("grokModelChip");
    const modelDropdown = document.getElementById("grokModelDropdown");
    const settingsToggle = document.getElementById("grokSettingsToggle");
    const settingsPanel = document.getElementById("grokSettingsPanel");
    const attachBtn = document.getElementById("grokAttachBtn");
    const fileInput = document.getElementById("grokFileInput");
    const fileRemoveBtn = document.getElementById("grokFileRemoveBtn");
    const sidebarToggle = document.getElementById("grokChatSidebarToggle");
    const sidebarOverlay = document.getElementById("grokChatSidebarOverlay");
    const collapseBtn = document.getElementById("grokChatCollapseBtn");
    const expandBtn = document.getElementById("grokChatExpandBtn");
    const tempRange = document.getElementById("grokTempRange");
    const tempValue = document.getElementById("grokTempValue");
    const topPRange = document.getElementById("grokTopPRange");
    const topPValue = document.getElementById("grokTopPValue");

    if (newBtn) newBtn.addEventListener("click", () => {
      newChatSession();
      if (isMobileChatSidebar()) {
        closeChatSidebar();
      }
    });
    if (sendBtn) {
      sendBtn.addEventListener("click", () => {
        if (chatState.sending && chatState.abortController) {
          chatState.abortController.abort();
          return;
        }
        sendChatMessage().catch(() => {});
      });
    }
    if (attachBtn && fileInput) {
      attachBtn.addEventListener("click", () => fileInput.click());
    }
    if (fileInput) {
      fileInput.addEventListener("change", async () => {
        const file = fileInput.files && fileInput.files[0];
        if (!file) return;
        try {
          const dataUrl = await readFileAsDataURL(file);
          chatState.pendingFile = {
            name: file.name || "upload.bin",
            type: file.type || "",
            dataUrl,
          };
          renderPendingChatFile();
        } catch (err) {
          showToast(err?.message || "读取文件失败", "error");
        } finally {
          fileInput.value = "";
        }
      });
    }
    if (fileRemoveBtn) {
      fileRemoveBtn.addEventListener("click", () => clearPendingChatFile());
    }
    if (input) {
      let composing = false;
      input.addEventListener("compositionstart", () => {
        composing = true;
      });
      input.addEventListener("compositionend", () => {
        composing = false;
      });
      input.addEventListener("input", () => {
        input.style.height = "40px";
        input.style.height = `${Math.min(input.scrollHeight, 160)}px`;
      });
      input.addEventListener("keydown", (event) => {
        if (event.key === "Enter" && !event.shiftKey) {
          if (composing || event.isComposing) return;
          event.preventDefault();
          sendChatMessage().catch(() => {});
        }
      });
    }
    if (sidebarToggle) sidebarToggle.addEventListener("click", () => toggleChatSidebar());
    if (expandBtn) expandBtn.addEventListener("click", () => openChatSidebar());
    if (collapseBtn) collapseBtn.addEventListener("click", () => closeChatSidebar());
    if (sidebarOverlay) sidebarOverlay.addEventListener("click", () => closeChatSidebar());
    window.addEventListener("resize", syncChatSidebarState);
    if (modelChip && modelDropdown) {
      modelChip.addEventListener("click", (event) => {
        event.stopPropagation();
        modelDropdown.classList.toggle("show");
      });
      modelDropdown.addEventListener("click", (event) => {
        const btn = event.target.closest(".model-option");
        if (!btn || !modelDropdown.contains(btn)) return;
        chatState.model = String(btn.dataset.model || chatState.model);
        syncChatModelUI();
        renderChatModelDropdown();
        saveChatSessions();
        modelDropdown.classList.remove("show");
      });
    }
    if (settingsToggle && settingsPanel) {
      settingsToggle.addEventListener("click", (event) => {
        event.stopPropagation();
        settingsPanel.classList.toggle("show");
      });
    }
    document.addEventListener("click", () => {
      modelDropdown?.classList.remove("show");
      settingsPanel?.classList.remove("show");
    });
    if (tempRange && tempValue) {
      tempRange.addEventListener("input", () => {
        tempValue.textContent = String(Number(tempRange.value).toFixed(2)).replace(/\.00$/, "");
        saveGrokToolsUIState({ chatTemperature: Number(tempRange.value) });
      });
    }
    if (topPRange && topPValue) {
      topPRange.addEventListener("input", () => {
        topPValue.textContent = String(Number(topPRange.value).toFixed(2)).replace(/\.00$/, "");
        saveGrokToolsUIState({ chatTopP: Number(topPRange.value) });
      });
    }
    const systemInput = document.getElementById("grokSystemInput");
    if (systemInput) {
      systemInput.addEventListener("input", () => {
        saveGrokToolsUIState({ chatSystemPrompt: String(systemInput.value || "") });
      });
    }
  }

  async function initChat() {
    await loadChatModels();
    loadChatSessions();
    const uiState = loadGrokToolsUIState();
    if (!chatState.models.includes(chatState.model)) {
      chatState.model = preferredConsoleModel(chatState.models);
    }
    const tempRange = document.getElementById("grokTempRange");
    const tempValue = document.getElementById("grokTempValue");
    const topPRange = document.getElementById("grokTopPRange");
    const topPValue = document.getElementById("grokTopPValue");
    const systemInput = document.getElementById("grokSystemInput");
    if (tempRange && typeof uiState.chatTemperature === "number") {
      tempRange.value = String(uiState.chatTemperature);
    }
    if (tempValue && tempRange) {
      tempValue.textContent = String(Number(tempRange.value).toFixed(2)).replace(/\.00$/, "");
    }
    if (topPRange && typeof uiState.chatTopP === "number") {
      topPRange.value = String(uiState.chatTopP);
    }
    if (topPValue && topPRange) {
      topPValue.textContent = String(Number(topPRange.value).toFixed(2)).replace(/\.00$/, "");
    }
    if (systemInput && typeof uiState.chatSystemPrompt === "string") {
      systemInput.value = uiState.chatSystemPrompt;
    }
    syncChatModelUI();
    renderChatModelDropdown();
    renderChatSessions();
    rerenderChatThread();
    renderPendingChatFile();
    setChatSendButtonState(false);
    bindChatEvents();
    closeChatSidebar();
    syncChatSidebarState();
  }

  function renderPendingChatFile() {
    const badge = document.getElementById("grokFileBadge");
    const name = document.getElementById("grokFileName");
    if (!badge || !name) return;
    if (!chatState.pendingFile?.name) {
      badge.classList.add("hidden");
      name.textContent = "";
      return;
    }
    badge.classList.remove("hidden");
    name.textContent = chatState.pendingFile.name;
  }

  function clearPendingChatFile() {
    chatState.pendingFile = null;
    renderPendingChatFile();
  }

  function setVoiceStatus(text, type) {
    const el = document.getElementById("voiceStatus");
    if (!el) return;
    el.textContent = String(text || "");
    el.classList.remove("connected", "connecting", "error");
    if (type === "ok") el.classList.add("connected");
    if (type === "warn" || type === "connecting") el.classList.add("connecting");
    if (type === "error") el.classList.add("error");
  }

  function appendVoiceLog(line) {
    const el = document.getElementById("voiceLogOutput");
    if (!el) return;
    const time = new Date().toLocaleTimeString();
    el.textContent = `[${time}] ${String(line || "")}\n` + el.textContent;
    el.scrollTop = 0;
  }

  function clearVoiceLog() {
    const el = document.getElementById("voiceLogOutput");
    if (el) el.textContent = "";
  }

  function setVoiceButtons(running) {
    const start = document.getElementById("voiceStartBtn");
    const stop = document.getElementById("voiceStopBtn");
    if (start) {
      start.disabled = false;
      start.classList.toggle("hidden", !!running);
    }
    if (stop) {
      stop.disabled = false;
      stop.classList.toggle("hidden", !running);
    }
  }

  function updateVoiceMeta() {
    const voiceSelect = String(document.getElementById("voiceName")?.value || "ara").trim() || "ara";
    const customVoice = String(document.getElementById("voiceCustomID")?.value || "").trim();
    const voice = voiceSelect === "custom" ? (customVoice || "custom") : voiceSelect;
    const personality = String(document.getElementById("voicePersonality")?.value || "assistant").trim() || "assistant";
    const speed = Math.max(0.1, Number(document.getElementById("voiceSpeed")?.value || 1));
    const customBlock = document.getElementById("voiceCustomBlock");
    const statusVoice = document.getElementById("voiceStatusVoice");
    const statusPersonality = document.getElementById("voiceStatusPersonality");
    const statusSpeed = document.getElementById("voiceStatusSpeed");
    const speedValue = document.getElementById("voiceSpeedValue");
    if (customBlock) customBlock.classList.toggle("hidden", voiceSelect !== "custom");
    if (statusVoice) statusVoice.textContent = voice;
    if (statusPersonality) statusPersonality.textContent = personality;
    if (statusSpeed) statusSpeed.textContent = `${speed}x`;
    if (speedValue) speedValue.textContent = speed.toFixed(1);
    updateVoiceRangeProgress();
  }

  function updateVoiceRangeProgress() {
    const speedRange = document.getElementById("voiceSpeed");
    if (!speedRange) return;
    const min = Number(speedRange.min || 0);
    const max = Number(speedRange.max || 100);
    const val = Number(speedRange.value || 0);
    const pct = max === min ? 0 : ((val - min) / (max - min)) * 100;
    speedRange.style.setProperty("--range-progress", `${pct}%`);
  }

  function buildVoiceVisualizerBars() {
    const root = document.getElementById("voiceVisualizer");
    if (!root) return;
    root.innerHTML = "";
    const targetCount = Math.max(36, Math.floor(root.offsetWidth / 7));
    for (let i = 0; i < targetCount; i += 1) {
      const bar = document.createElement("div");
      bar.className = "bar";
      root.appendChild(bar);
    }
  }

  function stopVoiceVisualizer() {
    const root = document.getElementById("voiceVisualizer");
    if (!root) return;
    const bars = root.querySelectorAll(".bar");
    bars.forEach((bar) => {
      bar.style.height = "6px";
    });
  }

  function startVoiceVisualizer() {
    const root = document.getElementById("voiceVisualizer");
    if (!root) return;
    buildVoiceVisualizerBars();
    if (voiceState.visualizerTimer) return;
    voiceState.visualizerTimer = window.setInterval(() => {
      const bars = root.querySelectorAll(".bar");
      const status = document.getElementById("voiceStatus");
      const connected = status && status.classList.contains("connected");
      bars.forEach((bar) => {
        bar.style.height = connected ? `${Math.random() * 32 + 6}px` : "6px";
      });
    }, 150);
  }

  function resetVoiceAudio() {
    const root = document.getElementById("voiceAudioRoot");
    if (root) root.innerHTML = "";
  }

  function syncVoiceOutputMute() {
    const root = document.getElementById("voiceAudioRoot");
    if (root) {
      root.querySelectorAll("audio").forEach((audio) => {
        audio.muted = !!voiceState.outputMuted;
      });
    }
    const btn = document.getElementById("voiceMuteOutputBtn");
    if (btn) btn.textContent = voiceState.outputMuted ? "恢复输出" : "静音输出";
  }

  async function resetVoiceSession(reason, opts) {
    const options = opts || {};
    const skipDisconnect = !!options.skipDisconnect;
    const statusText = options.statusText === undefined ? t("common.notConnected") : options.statusText;
    const statusType = options.statusType || "";
    const logLine = options.logLine === undefined
      ? (reason ? `Voice session stopped: ${reason}` : "Voice session stopped")
      : options.logLine;

    if (!skipDisconnect && voiceState.room && typeof voiceState.room.disconnect === "function") {
      try {
        await voiceState.room.disconnect();
      } catch (err) {
        // ignore disconnect failures during reset
      }
    }
    voiceState.room = null;
    voiceState.running = false;
    voiceState.stopping = false;
    voiceState.reconnecting = false;
    setVoiceButtons(false);
    stopVoiceVisualizer();
    resetVoiceAudio();
    setVoiceStatus(statusText, statusType);
    if (logLine) appendVoiceLog(logLine);
  }

  function resolveLiveKitClient() {
    return window.LivekitClient || window.LiveKitClient || null;
  }

  async function ensureLiveKitClient() {
    const sdk = resolveLiveKitClient();
    if (sdk) return sdk;
    if (!grokLazyState.livekitPromise) {
      grokLazyState.livekitPromise = new Promise((resolve, reject) => {
        const existing = document.querySelector('script[data-livekit-client="1"]');
        if (existing) {
          existing.addEventListener("load", () => resolve(resolveLiveKitClient()), { once: true });
          existing.addEventListener("error", () => reject(new Error("LiveKit SDK 加载失败")), { once: true });
          return;
        }
        const script = document.createElement("script");
        script.src = "https://cdn.jsdelivr.net/npm/livekit-client@2.19.1/dist/livekit-client.umd.min.js";
        script.async = true;
        script.dataset.livekitClient = "1";
        script.onload = () => {
          const loaded = resolveLiveKitClient();
          if (loaded) {
            appendVoiceLog("LiveKit SDK 已按需加载");
            resolve(loaded);
            return;
          }
          reject(new Error("LiveKit SDK 未挂载到 window"));
        };
        script.onerror = () => reject(new Error("LiveKit SDK 加载失败"));
        document.head.appendChild(script);
      }).catch((err) => {
        grokLazyState.livekitPromise = null;
        throw err;
      });
    }
    const loaded = await grokLazyState.livekitPromise;
    if (loaded) return loaded;
    appendVoiceLog("LiveKit SDK 未加载");
    showToast("LiveKit SDK 未加载", "error");
    throw new Error("LiveKit SDK 未加载");
  }

  function ensureVoiceMicSupport() {
    const hasMediaDevices = typeof navigator !== "undefined" && navigator.mediaDevices;
    const hasGetUserMedia = !!(hasMediaDevices && typeof navigator.mediaDevices.getUserMedia === "function");
    if (hasGetUserMedia) return;
    const isLocalhost = typeof window !== "undefined" && ["localhost", "127.0.0.1"].includes(window.location.hostname);
    const secureHint = (typeof window !== "undefined" && window.isSecureContext) || isLocalhost
      ? "当前浏览器未暴露麦克风接口"
      : "当前页面不是 HTTPS 或 localhost，浏览器不会开放麦克风";
    throw new Error(secureHint);
  }

  function formatVoiceMicError(err) {
    const raw = String(err?.message || err || "");
    if (/permission denied|notallowederror/i.test(raw)) {
      return "麦克风权限被拒绝，请在浏览器设置中允许麦克风后重试";
    }
    if (/notfounderror|devicesnotfounderror/i.test(raw)) {
      return "未检测到麦克风设备";
    }
    if (/notreadableerror|trackstarterror/i.test(raw)) {
      return "麦克风被占用或不可用";
    }
    return raw || "麦克风不可用";
  }

  function isVoiceMicLikeError(err) {
    const raw = String(err?.message || err || "");
    return /permission denied|notallowederror|notfounderror|devicesnotfounderror|notreadableerror|trackstarterror|microphone/i.test(raw);
  }

  async function logVoiceMicDiagnostics(err) {
    try {
      const protocol = window.location?.protocol || "";
      const host = window.location?.host || "";
      const secure = !!window.isSecureContext;
      const isLocalhost = typeof window !== "undefined" && ["localhost", "127.0.0.1"].includes(window.location.hostname);
      let policyAllowed = "unknown";
      const policy = document.permissionsPolicy || document.featurePolicy;
      if (policy && typeof policy.allowsFeature === "function") {
        policyAllowed = policy.allowsFeature("microphone") ? "yes" : "no";
      }
      let permState = "unknown";
      try {
        if (navigator.permissions && typeof navigator.permissions.query === "function") {
          const status = await navigator.permissions.query({ name: "microphone" });
          permState = status?.state || "unknown";
        }
      } catch (err2) {
        permState = "unsupported";
      }
      const errName = err?.name || "";
      const errMsg = err?.message || String(err || "");
      appendVoiceLog(
        `Mic diagnostics: secure=${secure} localhost=${isLocalhost} protocol=${protocol} host=${host} policy=${policyAllowed} perm=${permState} error=${errName || errMsg}`
      );
    } catch (err3) {
      // ignore diagnostics failures
    }
  }

  async function fetchVoiceToken() {
    updateVoiceMeta();
    const voiceSelect = String(document.getElementById("voiceName")?.value || "ara").trim() || "ara";
    const customVoice = String(document.getElementById("voiceCustomID")?.value || "").trim();
    const voice = voiceSelect === "custom" ? customVoice : voiceSelect;
    const personality = String(document.getElementById("voicePersonality")?.value || "assistant").trim() || "assistant";
    const speed = Number(document.getElementById("voiceSpeed")?.value || 1);
    if (!voice) {
      throw new Error("请选择声音，或填写自定义 voice_id");
    }

    const res = await fetch("/api/v1/admin/voice/token", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        voice,
        personality,
        speed: speed > 0 ? speed : 1,
      }),
    });
    if (handleUnauthorized(res)) return null;
    if (!res.ok) {
      throw new Error(await res.text());
    }
    const data = await res.json();
    const token = String(data.token || "");
    const livekitURL = String(data.url || "");
    const urlStatus = document.getElementById("voiceStatusURL");
    if (urlStatus) urlStatus.textContent = livekitURL || "-";
    appendVoiceLog("Fetched voice token");
    return { token, url: livekitURL };
  }

  async function stopVoiceSession(reason) {
    if (voiceState.stopping) return;
    voiceState.stopping = true;
    await resetVoiceSession(reason, {
      skipDisconnect: false,
      statusText: t("common.notConnected"),
      statusType: "",
    });
  }

  async function startVoiceSession() {
    if (voiceState.running) {
      showToast(t("voice.alreadyRunning"), "info");
      return;
    }
    const startBtn = document.getElementById("voiceStartBtn");
    if (startBtn) startBtn.disabled = true;
    setVoiceStatus(t("common.connecting"), "connecting");
    updateVoiceMeta();

    const LiveKitSDK = await ensureLiveKitClient();
    const payload = await fetchVoiceToken();
    if (!payload || !payload.token || !payload.url) {
      throw new Error(t("voice.tokenUnavailable"));
    }

    const room = new LiveKitSDK.Room({
      adaptiveStream: true,
      dynacast: true,
      audioCaptureDefaults: {
        autoGainControl: true,
        echoCancellation: true,
        noiseSuppression: true,
      },
    });
    voiceState.room = room;
    voiceState.running = true;
    voiceState.reconnecting = false;
    room.on(LiveKitSDK.RoomEvent.ParticipantConnected, (participant) => {
      appendVoiceLog(`Participant connected: ${participant?.identity || "unknown"}`);
    });
    room.on(LiveKitSDK.RoomEvent.ParticipantDisconnected, (participant) => {
      appendVoiceLog(`Participant disconnected: ${participant?.identity || "unknown"}`);
    });
    room.on(LiveKitSDK.RoomEvent.TrackSubscribed, (track) => {
      if (!track || track.kind !== "audio") return;
      const root = document.getElementById("voiceAudioRoot");
      if (!root) return;
      root.innerHTML = "";
      try {
        const el = track.attach();
        el.autoplay = true;
        el.controls = true;
        el.muted = !!voiceState.outputMuted;
        root.appendChild(el);
        syncVoiceOutputMute();
      } catch (err) {
        appendVoiceLog(`Attach audio failed: ${err.message || err}`);
      }
    });
    room.on(LiveKitSDK.RoomEvent.Reconnecting, () => {
      if (voiceState.room !== room || voiceState.stopping) return;
      voiceState.reconnecting = true;
      appendVoiceLog("Voice reconnecting");
      setVoiceStatus("重连中...", "warn");
    });
    room.on(LiveKitSDK.RoomEvent.Reconnected, () => {
      if (voiceState.room !== room || voiceState.stopping) return;
      voiceState.reconnecting = false;
      appendVoiceLog("Voice reconnected");
      setVoiceStatus("已连接", "ok");
    });
    room.on(LiveKitSDK.RoomEvent.ConnectionStateChanged, (state) => {
      appendVoiceLog(`Connection state: ${String(state || "unknown")}`);
    });
    if (LiveKitSDK.RoomEvent.MediaDevicesError) {
      room.on(LiveKitSDK.RoomEvent.MediaDevicesError, (err) => {
        appendVoiceLog(`Media devices error: ${err?.message || err}`);
      });
    }
    room.on(LiveKitSDK.RoomEvent.Disconnected, (reason) => {
      const text = String(reason || "disconnected");
      if (voiceState.room !== room) return;
      if (voiceState.stopping) {
        appendVoiceLog(`Voice disconnected after stop: ${text}`);
        return;
      }
      appendVoiceLog(`Voice disconnected: ${text}`);
      if (voiceState.reconnecting) {
        setVoiceStatus("重连中...", "warn");
        return;
      }
      resetVoiceSession(reason || "disconnected", {
        skipDisconnect: true,
        statusText: t("common.notConnected"),
        statusType: "",
      }).catch(() => {});
    });

    try {
      await room.connect(payload.url, payload.token);
      appendVoiceLog("Connected to LiveKit signaling server");
      ensureVoiceMicSupport();
      if (!room.localParticipant || typeof room.localParticipant.setMicrophoneEnabled !== "function") {
        throw new Error("LiveKit microphone API is unavailable");
      }
      await room.localParticipant.setMicrophoneEnabled(true);
      setVoiceStatus(t("voice.connected"), "ok");
      setVoiceButtons(true);
      appendVoiceLog("Voice session connected");
      startVoiceVisualizer();
    } catch (err) {
      appendVoiceLog(`Voice connect failed: ${err?.message || err}`);
      await resetVoiceSession("connect failed", {
        skipDisconnect: false,
        statusText: t("voice.connectFailed"),
        statusType: "error",
        logLine: null,
      });
      if (!err?._voiceLogged) {
        const message = isVoiceMicLikeError(err) ? formatVoiceMicError(err) : String(err?.message || err || "连接失败");
        appendVoiceLog(message);
        setVoiceStatus(message || t("voice.connectFailed"), "error");
        throw Object.assign(new Error(message), { _voiceLogged: true });
      }
      setVoiceStatus(err.message || t("voice.connectFailed"), "error");
      throw err;
    }
  }

  function setVideoStatus(text, type) {
    const el = document.getElementById("videoStatus");
    if (!el) return;
    el.textContent = String(text || "");
    el.classList.remove("connected", "connecting", "error");
    if (type === "ok") el.classList.add("connected");
    if (type === "warn" || type === "connecting") el.classList.add("connecting");
    if (type === "error") el.classList.add("error");
  }

  function setVideoButtons(running) {
    const start = document.getElementById("videoStartBtn");
    const stop = document.getElementById("videoStopBtn");
    if (start) {
      start.disabled = false;
      start.classList.toggle("hidden", !!running);
    }
    if (stop) {
      stop.disabled = false;
      stop.classList.toggle("hidden", !running);
    }
  }

  function updateVideoMeta() {
    const aspectValue = document.getElementById("videoAspectValue");
    const lengthValue = document.getElementById("videoLengthValue");
    const resolutionValue = document.getElementById("videoResolutionValue");
    const presetValue = document.getElementById("videoPresetValue");
    const ratioInput = document.getElementById("videoRatio");
    const lengthInput = document.getElementById("videoLength");
    const resolutionInput = document.getElementById("videoResolution");
    const presetInput = document.getElementById("videoPreset");
    if (aspectValue && ratioInput) aspectValue.textContent = ratioInput.value || "-";
    if (lengthValue && lengthInput) lengthValue.textContent = `${lengthInput.value || "-"}s`;
    if (resolutionValue && resolutionInput) resolutionValue.textContent = resolutionInput.value || "-";
    if (presetValue && presetInput) presetValue.textContent = presetInput.value || "-";
  }

  function setVideoProgress(value) {
    const safe = Math.max(0, Math.min(100, Number(value) || 0));
    videoState.lastProgress = safe;
    const fill = document.getElementById("videoProgressFill");
    const text = document.getElementById("videoProgressText");
    if (fill) fill.style.width = `${safe}%`;
    if (text) text.textContent = `${safe}%`;
  }

  function setVideoIndeterminate(active) {
    const bar = document.getElementById("videoProgressBar");
    if (!bar) return;
    bar.classList.toggle("indeterminate", !!active);
  }

  function stopVideoElapsedTimer() {
    if (videoState.elapsedTimer) {
      clearInterval(videoState.elapsedTimer);
      videoState.elapsedTimer = null;
    }
  }

  function startVideoElapsedTimer() {
    stopVideoElapsedTimer();
    const duration = document.getElementById("videoDurationValue");
    videoState.elapsedTimer = window.setInterval(() => {
      if (!videoState.startAt || !duration) return;
      const seconds = Math.max(0, Math.round((Date.now() - videoState.startAt) / 1000));
      duration.textContent = `${seconds}s`;
    }, 1000);
  }

  function resetVideoOutput(keepPreview) {
    const stage = document.getElementById("videoStage");
    const empty = document.getElementById("videoEmpty");
    videoState.contentBuffer = "";
    videoState.progressBuffer = "";
    videoState.collectingContent = false;
    videoState.lastProgress = 0;
    videoState.currentPreviewItem = null;
    setVideoProgress(0);
    setVideoIndeterminate(false);
    if (!keepPreview) {
      if (stage) {
        stage.innerHTML = "";
        stage.classList.add("hidden");
      }
      if (empty) {
        empty.classList.remove("hidden");
      }
      videoState.previewCount = 0;
    }
    const duration = document.getElementById("videoDurationValue");
    if (duration) duration.textContent = "-";
  }

  function normalizeVideoURL(raw) {
    let url = String(raw || "").trim();
    if (!url) return "";
    url = url.replace(/^["'`(<\[]+/, "").replace(/["'`)>\\],.;:]+$/g, "");
    try {
      return new URL(url, window.location.origin).toString();
    } catch (err) {
      return url;
    }
  }

  function videoSizeForRatio(value) {
    switch (String(value || "").trim()) {
      case "16:9":
      case "3:2":
      case "1792x1024":
      case "1280x720":
        return "1792x1024";
      case "9:16":
      case "2:3":
      case "1024x1792":
      case "720x1280":
        return "1024x1792";
      case "1:1":
      case "1024x1024":
        return "1024x1024";
      default:
        return "720x1280";
    }
  }

  function initVideoPreviewSlot() {
    const stage = document.getElementById("videoStage");
    if (!stage) return;
    videoState.previewCount += 1;
    const item = document.createElement("div");
    item.className = "video-item is-pending";
    item.dataset.index = String(videoState.previewCount);

    const header = document.createElement("div");
    header.className = "video-item-bar";

    const title = document.createElement("div");
    title.className = "video-item-title";
    title.textContent = `Video #${videoState.previewCount}`;

    const actions = document.createElement("div");
    actions.className = "video-item-actions";

    const openBtn = document.createElement("a");
    openBtn.className = "btn btn-outline video-open hidden";
    openBtn.target = "_blank";
    openBtn.rel = "noopener";
    openBtn.textContent = "打开";

    const downloadBtn = document.createElement("button");
    downloadBtn.type = "button";
    downloadBtn.className = "btn btn-outline video-download";
    downloadBtn.textContent = "下载";
    downloadBtn.disabled = true;

    actions.appendChild(openBtn);
    actions.appendChild(downloadBtn);
    header.appendChild(title);
    header.appendChild(actions);

    const body = document.createElement("div");
    body.className = "video-item-body";
    body.innerHTML = `<div class="video-item-placeholder">${t("video.generatingPlaceholder")}</div>`;

    const link = document.createElement("div");
    link.className = "video-item-link";

    item.appendChild(header);
    item.appendChild(body);
    item.appendChild(link);
    stage.appendChild(item);
    stage.classList.remove("hidden");
    const empty = document.getElementById("videoEmpty");
    if (empty) empty.classList.add("hidden");
    videoState.currentPreviewItem = item;
  }

  function ensureVideoPreviewSlot() {
    if (!videoState.currentPreviewItem) {
      initVideoPreviewSlot();
    }
    return videoState.currentPreviewItem;
  }

  function updateVideoItemLinks(item, url) {
    if (!item) return;
    const openBtn = item.querySelector(".video-open");
    const downloadBtn = item.querySelector(".video-download");
    const link = item.querySelector(".video-item-link");
    const safeUrl = normalizeVideoURL(url);
    item.dataset.url = safeUrl;
    if (link) {
      link.textContent = safeUrl;
      link.classList.toggle("has-url", !!safeUrl);
    }
    if (openBtn) {
      if (safeUrl) {
        openBtn.href = safeUrl;
        openBtn.classList.remove("hidden");
      } else {
        openBtn.classList.add("hidden");
        openBtn.removeAttribute("href");
      }
    }
    if (downloadBtn) {
      downloadBtn.dataset.url = safeUrl;
      downloadBtn.disabled = !safeUrl;
    }
    if (safeUrl) {
      item.classList.remove("is-pending");
    }
  }

  function extractVideoInfo(buffer) {
    if (!buffer) return null;
    if (buffer.includes("<video")) {
      const matches = buffer.match(/<video[\s\S]*?<\/video>/gi);
      if (matches && matches.length) {
        return { html: matches[matches.length - 1] };
      }
    }
    const mdMatches = buffer.match(/\[video\]\(([^)]+)\)/g);
    if (mdMatches && mdMatches.length) {
      const last = mdMatches[mdMatches.length - 1];
      const urlMatch = last.match(/\[video\]\(([^)]+)\)/);
      if (urlMatch) {
        return { url: normalizeVideoURL(urlMatch[1]) };
      }
    }
    const urlMatches = buffer.match(/(?:https?:\/\/|\/grok\/v1\/files\/video\/|\/v1\/files\/video\/|\/grok\/v1\/videos\/|\/v1\/videos\/)[^\s<)]+/g);
    if (urlMatches && urlMatches.length) {
      return { url: normalizeVideoURL(urlMatches[urlMatches.length - 1]) };
    }
    return null;
  }

  function renderVideoFromHtml(html) {
    const container = ensureVideoPreviewSlot();
    if (!container) return;
    const body = container.querySelector(".video-item-body");
    if (!body) return;
    body.innerHTML = html;
    const videoEl = body.querySelector("video");
    let videoUrl = "";
    if (videoEl) {
      videoEl.controls = true;
      videoEl.preload = "metadata";
      const source = videoEl.querySelector("source");
      if (source && source.getAttribute("src")) {
        videoUrl = source.getAttribute("src");
      } else if (videoEl.getAttribute("src")) {
        videoUrl = videoEl.getAttribute("src");
      }
    }
    updateVideoItemLinks(container, videoUrl);
  }

  function renderVideoFromUrl(url) {
    const container = ensureVideoPreviewSlot();
    if (!container) return;
    const body = container.querySelector(".video-item-body");
    if (!body) return;
    const safeUrl = normalizeVideoURL(url);
    body.innerHTML = `
      <video controls preload="metadata">
        <source src="${safeUrl}" type="video/mp4">
      </video>
    `;
    updateVideoItemLinks(container, safeUrl);
  }

  function handleVideoDelta(text) {
    if (!text) return;
    if (text.includes("<think>") || text.includes("</think>")) {
      return;
    }
    if (/超分辨率|super\s+resolution/i.test(text)) {
      setVideoStatus(t("video.superResolution"), "connecting");
      setVideoIndeterminate(true);
      const progressText = document.getElementById("videoProgressText");
      if (progressText) progressText.textContent = t("video.superResolution");
      return;
    }

    if (!videoState.collectingContent) {
      const maybeVideo = text.includes("<video")
        || text.includes("[video](")
        || text.includes("http://")
        || text.includes("https://")
        || text.includes("/grok/v1/files/video/")
        || text.includes("/v1/files/video/");
      if (maybeVideo) {
        videoState.collectingContent = true;
      }
    }

    if (videoState.collectingContent) {
      videoState.contentBuffer += text;
      const info = extractVideoInfo(videoState.contentBuffer);
      if (info) {
        if (info.html) {
          renderVideoFromHtml(info.html);
        } else if (info.url) {
          renderVideoFromUrl(info.url);
        }
      }
      return;
    }

    videoState.progressBuffer += text;
    const progressText = document.getElementById("videoProgressText");
    const roundMatches = [...videoState.progressBuffer.matchAll(/\[round=(\d+)\/(\d+)\]\s*progress=([0-9]+(?:\.[0-9]+)?)%/g)];
    if (roundMatches.length) {
      const last = roundMatches[roundMatches.length - 1];
      const round = parseInt(last[1], 10);
      const total = parseInt(last[2], 10);
      const value = parseFloat(last[3]);
      setVideoIndeterminate(false);
      setVideoProgress(value);
      if (progressText && Number.isFinite(round) && Number.isFinite(total) && total > 0) {
        progressText.textContent = `${Math.round(value)}% · ${round}/${total}`;
      }
      videoState.progressBuffer = videoState.progressBuffer.slice(Math.max(0, videoState.progressBuffer.length - 300));
      return;
    }

    const genericProgressMatches = [...videoState.progressBuffer.matchAll(/progress=([0-9]+(?:\.[0-9]+)?)%/g)];
    if (genericProgressMatches.length) {
      const last = genericProgressMatches[genericProgressMatches.length - 1];
      const value = parseFloat(last[1]);
      setVideoIndeterminate(false);
      setVideoProgress(value);
      videoState.progressBuffer = videoState.progressBuffer.slice(Math.max(0, videoState.progressBuffer.length - 240));
      return;
    }

    const matches = [...videoState.progressBuffer.matchAll(/进度\s*(\d+)%/g)];
    if (matches.length) {
      const last = matches[matches.length - 1];
      const value = parseInt(last[1], 10);
      setVideoIndeterminate(false);
      setVideoProgress(value);
      videoState.progressBuffer = videoState.progressBuffer.slice(Math.max(0, videoState.progressBuffer.length - 200));
    }
  }

  async function createVideoTask(payload) {
    const res = await fetch("/grok/v1/videos", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(payload),
    });
    if (handleUnauthorized(res)) return "";
    if (!res.ok) {
      throw new Error(await res.text());
    }
    const data = await res.json();
    return String(data.id || data.task_id || "").trim();
  }

  async function stopVideoTask() {
    if (!videoState.taskID) return;
    const res = await fetch("/api/v1/admin/video/stop", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ task_ids: [videoState.taskID] }),
    });
    if (handleUnauthorized(res)) return;
  }

  function closeVideoStream() {
    if (!videoState.stream) return;
    try {
      videoState.stream.close();
    } catch (err) {
      // ignore
    }
    videoState.stream = null;
  }

  function stopVideoPoll() {
    if (videoState.pollTimer) {
      clearTimeout(videoState.pollTimer);
      videoState.pollTimer = null;
    }
  }

  function finishVideoRun(hasError) {
    if (!videoState.running) return;
    closeVideoStream();
    stopVideoPoll();
    videoState.running = false;
    setVideoButtons(false);
    stopVideoElapsedTimer();
    setVideoIndeterminate(false);
    if (!hasError) {
      setVideoStatus(t("common.done"), "ok");
      setVideoProgress(100);
    }
    const duration = document.getElementById("videoDurationValue");
    if (duration && videoState.startAt) {
      const seconds = Math.max(0, Math.round((Date.now() - videoState.startAt) / 1000));
      duration.textContent = `${seconds}s`;
    }
  }

  async function fetchVideoJob(taskID) {
    const res = await fetch(`/grok/v1/videos/${encodeURIComponent(taskID)}?t=${Date.now()}`, { cache: "no-store" });
    if (handleUnauthorized(res)) return null;
    if (!res.ok) {
      throw new Error(await res.text());
    }
    return await res.json();
  }

  function videoContentURL(taskID) {
    return normalizeVideoURL(`/grok/v1/videos/${encodeURIComponent(taskID)}/content`);
  }

  function pollVideoTask(taskID) {
    stopVideoPoll();
    videoState.pollTimer = window.setTimeout(async () => {
      if (!videoState.running || videoState.taskID !== taskID) return;
      try {
        const job = await fetchVideoJob(taskID);
        if (!job) return;
        const progress = Number(job.progress || 0);
        setVideoIndeterminate(false);
        setVideoProgress(progress);
        if (job.status === "completed") {
          renderVideoFromUrl(job.content_url || videoContentURL(taskID));
          finishVideoRun(false);
          return;
        }
        if (job.status === "failed") {
          const message = job.error?.message || "视频生成失败";
          setVideoStatus(String(message), "error");
          finishVideoRun(true);
          return;
        }
        setVideoStatus(t("common.generating"), "ok");
        pollVideoTask(taskID);
      } catch (err) {
        if (!videoState.running) return;
        setVideoStatus(err.message || t("common.connectionError"), "error");
        finishVideoRun(true);
      }
    }, 1500);
  }

  function openVideoSSE(taskID) {
    const url = `/api/v1/admin/video/sse?task_id=${encodeURIComponent(taskID)}&t=${Date.now()}`;
    const es = new EventSource(url);
    videoState.stream = es;
    es.onopen = () => {
      setVideoStatus(t("common.generating"), "ok");
    };
    es.onmessage = (event) => {
      const text = String(event.data || "");
      if (!text) return;
      if (text === "[DONE]") {
        finishVideoRun(false);
        return;
      }
      let payload = null;
      try {
        payload = JSON.parse(text);
      } catch (err) {
        payload = null;
      }
      const errorMsg = payload && (payload.error || payload?.error?.message);
      if (errorMsg) {
        setVideoStatus(String(errorMsg), "error");
        finishVideoRun(true);
        return;
      }
      const choice = payload?.choices?.[0];
      const deltaContent = typeof choice?.delta?.content === "string" ? choice.delta.content : "";
      const messageContent = typeof choice?.message?.content === "string" ? choice.message.content : "";
      const content = deltaContent || messageContent || text;
      handleVideoDelta(content);
      if (choice && choice.finish_reason === "stop") {
        finishVideoRun(false);
      }
    };
    es.onerror = () => {
      if (!videoState.running) return;
      setVideoStatus(t("common.connectionError"), "error");
      finishVideoRun(true);
    };
  }

  async function startVideo() {
    if (videoState.running) {
      showToast(t("video.alreadyRunning"), "info");
      return;
    }
    const prompt = String(document.getElementById("videoPrompt")?.value || "").trim();
    if (!prompt) {
      showToast(t("video.enterPrompt"), "error");
      return;
    }
    const effort = String(document.getElementById("videoEffort")?.value || "").trim();
    const payload = {
      model: "grok-imagine-video",
      prompt,
      size: videoSizeForRatio(document.getElementById("videoRatio")?.value || "3:2"),
      seconds: Number(document.getElementById("videoLength")?.value || 6),
      resolution_name: String(document.getElementById("videoResolution")?.value || "480p"),
      preset: String(document.getElementById("videoPreset")?.value || "custom"),
      input_references: [],
    };
    const imageRef = videoState.fileDataURL || String(document.getElementById("videoImageUrl")?.value || "").trim();
    if (imageRef) payload.input_references = [imageRef];
    if (effort) payload.reasoning_effort = effort;
    updateVideoMeta();
    resetVideoOutput(true);
    initVideoPreviewSlot();
    setVideoButtons(true);
    const startBtn = document.getElementById("videoStartBtn");
    if (startBtn) startBtn.disabled = true;
    setVideoStatus(t("common.connecting"), "connecting");
    videoState.running = true;
    try {
      const taskID = await createVideoTask(payload);
      if (!taskID) {
        throw new Error(t("video.taskEmpty"));
      }
      videoState.taskID = taskID;
      videoState.startAt = Date.now();
      startVideoElapsedTimer();
      setVideoStatus(t("common.generating"), "ok");
      setVideoIndeterminate(true);
      pollVideoTask(taskID);
    } catch (err) {
      videoState.running = false;
      setVideoButtons(false);
      setVideoStatus(err.message || t("video.startFailed"), "error");
      throw err;
    }
  }

  async function stopVideo() {
    videoState.running = false;
    closeVideoStream();
    stopVideoPoll();
    stopVideoElapsedTimer();
    setVideoButtons(false);
    setVideoStatus(t("common.notConnected"));
    setVideoIndeterminate(false);
    try {
      await stopVideoTask();
    } catch (err) {
      // ignore
    }
    videoState.taskID = "";
  }

  function normalizeOnlineAccounts(rawAccounts) {
    const list = Array.isArray(rawAccounts) ? rawAccounts : [];
    const out = [];
    for (const item of list) {
      const token = normalizeOnlineToken(item?.token);
      if (!token) continue;
      out.push({
        ...item,
        token,
        token_masked: String(item?.token_masked || formatTokenMask(token)),
      });
    }
    return out;
  }

  function normalizeOnlineDetails(rawDetails) {
    const list = Array.isArray(rawDetails) ? rawDetails : [];
    const out = [];
    for (const item of list) {
      const token = normalizeOnlineToken(item?.token);
      if (!token) continue;
      out.push({
        ...item,
        token,
        token_masked: String(item?.token_masked || formatTokenMask(token)),
      });
    }
    return out;
  }

  function currentOnlineRows() {
    const rows = [];
    const online = cacheOnlineState.online || {};
    const detailsMap = cacheOnlineState.detailMap;
    if (cacheOnlineState.accounts.length > 0) {
      for (const acc of cacheOnlineState.accounts) {
        const token = normalizeOnlineToken(acc.token);
        if (!token) continue;
        const detail = detailsMap.get(token);
        const isOnlineToken = normalizeOnlineToken(online.token) === token;
        const count = detail ? toNumberOrZero(detail.count) : (isOnlineToken ? toNumberOrZero(online.count) : null);
        const status = String(detail?.status || (isOnlineToken ? online.status : "not_loaded") || "not_loaded");
        const lastClear = detail?.last_asset_clear_at ?? (isOnlineToken ? online.last_asset_clear_at : acc.last_asset_clear_at);
        rows.push({
          token,
          token_masked: String(acc.token_masked || detail?.token_masked || formatTokenMask(token)),
          pool: String(acc.pool || "-"),
          count,
          status,
          last_asset_clear_at: lastClear,
        });
      }
      return rows;
    }
    for (const detail of cacheOnlineState.details) {
      rows.push({
        token: detail.token,
        token_masked: String(detail.token_masked || formatTokenMask(detail.token)),
        pool: "-",
        count: toNumberOrZero(detail.count),
        status: String(detail.status || "not_loaded"),
        last_asset_clear_at: detail.last_asset_clear_at,
      });
    }
    return rows;
  }

  function syncCacheOnlineSelectAll() {
    const selectAll = document.getElementById("cacheOnlineSelectAll");
    const body = document.getElementById("cacheOnlineBody");
    if (!selectAll || !body) return;
    const checkboxes = Array.from(body.querySelectorAll("input.cache-online-check"));
    if (checkboxes.length === 0) {
      selectAll.checked = false;
      selectAll.indeterminate = false;
      return;
    }
    const selected = checkboxes.filter((item) => item.checked).length;
    selectAll.checked = selected > 0 && selected === checkboxes.length;
    selectAll.indeterminate = selected > 0 && selected < checkboxes.length;
  }

  function renderCacheOnlineTable() {
    const body = document.getElementById("cacheOnlineBody");
    if (!body) return;
    const rows = currentOnlineRows();
    if (rows.length === 0) {
      body.innerHTML = `<tr><td colspan="7" style="text-align:center;color:var(--text-secondary);padding:24px;">暂无在线账号</td></tr>`;
      syncCacheOnlineSelectAll();
      return;
    }

    body.innerHTML = rows.map((row) => {
      const checked = cacheOnlineState.selectedTokens.has(row.token) ? "checked" : "";
      const countText = row.count === null ? "-" : String(row.count);
      const statusText = resolveOnlineStatusText(row.status);
      const lastClear = formatDateTime(row.last_asset_clear_at);
      return `
        <tr>
          <td style="text-align:center;">
            <input type="checkbox" class="cache-online-check" data-token="${encodeURIComponent(row.token)}" ${checked} />
          </td>
          <td><code>${row.token_masked || formatTokenMask(row.token)}</code></td>
          <td><span class="tag">${row.pool || "-"}</span></td>
          <td>${countText}</td>
          <td>${statusText}</td>
          <td>${lastClear}</td>
          <td>
            <button class="btn btn-danger-outline cache-online-clear-btn" data-token="${encodeURIComponent(row.token)}" style="padding:4px 8px;">清理</button>
          </td>
        </tr>
      `;
    }).join("");
    syncCacheOnlineSelectAll();
  }

  function applyCacheOnlineData(data) {
    const online = (data && typeof data === "object") ? (data.online || {}) : {};
    const onlineScope = String(data?.online_scope || "none");
    const accounts = normalizeOnlineAccounts(data?.online_accounts);
    const details = normalizeOnlineDetails(data?.online_details);

    cacheOnlineState.accounts = accounts;
    cacheOnlineState.details = details;
    cacheOnlineState.online = online;
    cacheOnlineState.onlineScope = onlineScope;
    cacheOnlineState.accountMap = new Map();
    cacheOnlineState.detailMap = new Map();
    accounts.forEach((item) => cacheOnlineState.accountMap.set(item.token, item));
    details.forEach((item) => cacheOnlineState.detailMap.set(item.token, item));

    const available = new Set();
    accounts.forEach((item) => available.add(item.token));
    details.forEach((item) => available.add(item.token));
    Array.from(cacheOnlineState.selectedTokens).forEach((token) => {
      if (!available.has(token)) {
        cacheOnlineState.selectedTokens.delete(token);
      }
    });

    const onlineCountEl = document.getElementById("cacheOnlineCount");
    const onlineStatusEl = document.getElementById("cacheOnlineStatus");
    const onlineScopeEl = document.getElementById("cacheOnlineScope");
    const onlineLastClearEl = document.getElementById("cacheOnlineLastClear");
    if (onlineCountEl) onlineCountEl.textContent = String(toNumberOrZero(online.count));
    if (onlineStatusEl) onlineStatusEl.textContent = resolveOnlineStatusText(online.status);
    if (onlineScopeEl) onlineScopeEl.textContent = onlineScope;
    if (onlineLastClearEl) onlineLastClearEl.textContent = formatDateTime(online.last_asset_clear_at);

    renderCacheOnlineTable();
  }

  async function loadCacheSummary(options = {}) {
    const params = new URLSearchParams();
    const tokens = Array.isArray(options.tokens) ? options.tokens.map(normalizeOnlineToken).filter(Boolean) : [];
    const scope = String(options.scope || "").trim().toLowerCase();
    const token = normalizeOnlineToken(options.token);
    if (tokens.length > 0) {
      params.set("tokens", tokens.join(","));
    } else if (scope === "all") {
      params.set("scope", "all");
    } else if (token) {
      params.set("token", token);
    }

    const url = params.toString() ? `/api/v1/admin/cache?${params.toString()}` : "/api/v1/admin/cache";
    const res = await fetch(url);
    if (handleUnauthorized(res)) return;
    if (!res.ok) {
      throw new Error(await res.text());
    }
    const data = await res.json();
    const imageText = `${data?.image?.count || 0} / ${formatBytes(data?.image?.bytes || 0)}`;
    const videoText = `${data?.video?.count || 0} / ${formatBytes(data?.video?.bytes || 0)}`;
    const totalText = `${data?.total?.count || 0} / ${formatBytes(data?.total?.bytes || 0)}`;

    const imageEl = document.getElementById("cacheImageSummary");
    const videoEl = document.getElementById("cacheVideoSummary");
    const totalEl = document.getElementById("cacheTotalSummary");
    const baseEl = document.getElementById("cacheBaseDir");
    if (imageEl) imageEl.textContent = imageText;
    if (videoEl) videoEl.textContent = videoText;
    if (totalEl) totalEl.textContent = totalText;
    if (baseEl) baseEl.textContent = String(data.base_dir || "-");

    applyCacheOnlineData(data);
    return data;
  }

  function renderCacheList(items) {
    const body = document.getElementById("cacheListBody");
    if (!body) return;
    const list = Array.isArray(items) ? items : [];
    if (list.length === 0) {
      body.innerHTML = `<tr><td colspan="5" style="text-align:center;color:var(--text-secondary);padding:24px;">暂无缓存数据</td></tr>`;
      return;
    }

    body.innerHTML = list.map((item) => {
      const mediaType = String(item.media_type || "");
      const name = String(item.name || "");
      const url = String(item.view_url || item.url || "");
      const size = formatBytes(item.size_bytes || item.size || 0);
      const updatedAt = formatDateTime(item.mtime_ms || item.updated_at || 0);
      return `
        <tr>
          <td><span class="tag">${mediaType}</span></td>
          <td><a href="${url}" target="_blank" rel="noopener"><code>${name}</code></a></td>
          <td>${size}</td>
          <td>${updatedAt}</td>
          <td>
            <button class="btn btn-danger-outline cache-delete-btn" data-media-type="${encodeURIComponent(mediaType)}" data-name="${encodeURIComponent(name)}" style="padding:4px 8px;">删除</button>
          </td>
        </tr>
      `;
    }).join("");
  }

  function selectedOnlineTokens() {
    return Array.from(cacheOnlineState.selectedTokens);
  }

  async function cancelCacheBatchTask() {
    const taskID = String(cacheBatchState.taskID || "").trim();
    if (!cacheBatchState.running || !taskID) return;
    const res = await fetch(`/api/v1/admin/batch/${encodeURIComponent(taskID)}/cancel`, {
      method: "POST",
    });
    if (handleUnauthorized(res)) return;
    if (!res.ok) {
      throw new Error(await res.text());
    }
    showToast("已发送取消请求", "info");
  }

  async function startOnlineLoadBatch(payload, label) {
    if (cacheBatchState.running) {
      showToast("有任务正在运行，请稍候", "info");
      return;
    }
    const res = await fetch("/api/v1/admin/cache/online/load/async", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(payload || {}),
    });
    if (handleUnauthorized(res)) return;
    let data = {};
    try {
      data = await res.json();
    } catch (err) {
      // ignore
    }
    if (!res.ok || String(data.status || "") !== "success") {
      throw new Error(data.detail || data.error || (await res.text()) || "请求失败");
    }

    const taskID = String(data.task_id || "").trim();
    if (!taskID) {
      throw new Error("创建任务失败：空 task_id");
    }
    const total = toNumberOrZero(data.total);
    beginCacheBatch("load", taskID, total, "在线统计加载中");
    showToast(`开始加载 ${label || "账号"} (${total})`, "info");

    openCacheBatchStream(taskID, {
      onDone: async (msg) => {
        try {
          const result = (msg && typeof msg.result === "object") ? msg.result : null;
          if (result) {
            applyCacheOnlineData(result);
          } else {
            await loadCacheSummary(payload || {});
          }

          let ok = 0;
          let fail = 0;
          const details = Array.isArray(result?.online_details) ? result.online_details : [];
          if (details.length > 0) {
            details.forEach((item) => {
              const status = String(item?.status || "").trim().toLowerCase();
              if (status === "ok") ok++;
              else fail++;
            });
          } else {
            const totalDone = Math.max(toNumberOrZero(msg?.total), toNumberOrZero(data.total));
            ok = totalDone;
          }

          cacheBatchState.processed = Math.max(toNumberOrZero(msg?.total), toNumberOrZero(data.total));
          cacheBatchState.total = cacheBatchState.processed;
          finishCacheBatch("空闲");
          showToast(`在线统计加载完成：成功 ${ok}，失败 ${fail}`, fail > 0 ? "info" : "success");
        } catch (err) {
          finishCacheBatch("失败");
          throw err;
        }
      },
      onCancelled: () => {
        finishCacheBatch("已取消");
        showToast("已终止加载", "info");
      },
      onError: (message) => {
        finishCacheBatch("失败");
        showToast(`加载失败: ${message || "未知错误"}`, "error");
      },
    });
  }

  async function startOnlineClearBatch(tokens) {
    const cleanTokens = Array.isArray(tokens) ? tokens.map(normalizeOnlineToken).filter(Boolean) : [];
    if (cleanTokens.length === 0) {
      showToast("请先选择在线账号", "info");
      return;
    }
    if (cacheBatchState.running) {
      showToast("有任务正在运行，请稍候", "info");
      return;
    }

    const res = await fetch("/api/v1/admin/cache/online/clear/async", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ tokens: cleanTokens }),
    });
    if (handleUnauthorized(res)) return;
    let data = {};
    try {
      data = await res.json();
    } catch (err) {
      // ignore
    }
    if (!res.ok || String(data.status || "") !== "success") {
      throw new Error(data.detail || data.error || (await res.text()) || "请求失败");
    }

    const taskID = String(data.task_id || "").trim();
    if (!taskID) {
      throw new Error("创建任务失败：空 task_id");
    }
    const total = toNumberOrZero(data.total);
    beginCacheBatch("clear", taskID, total, "在线资产清理中");
    showToast(`开始清理 ${cleanTokens.length} 个账号`, "info");

    openCacheBatchStream(taskID, {
      onDone: async (msg) => {
        try {
          const result = (msg && typeof msg.result === "object") ? msg.result : {};
          const summary = (result && typeof result.summary === "object") ? result.summary : {};
          const ok = toNumberOrZero(summary.ok);
          const fail = toNumberOrZero(summary.fail);
          const doneTotal = Math.max(toNumberOrZero(summary.total), toNumberOrZero(msg?.total), cleanTokens.length);
          cacheBatchState.processed = doneTotal;
          cacheBatchState.total = doneTotal;
          finishCacheBatch("空闲");
          showToast(`在线清理完成：成功 ${ok}，失败 ${fail}`, fail > 0 ? "info" : "success");
          await loadCacheSummary({ tokens: cleanTokens });
        } catch (err) {
          finishCacheBatch("失败");
          throw err;
        }
      },
      onCancelled: () => {
        finishCacheBatch("已取消");
        showToast("已终止清理", "info");
      },
      onError: (message) => {
        finishCacheBatch("失败");
        showToast(`清理失败: ${message || "未知错误"}`, "error");
      },
    });
  }

  async function clearOnlineAssets(tokens) {
    const cleanTokens = Array.isArray(tokens) ? tokens.map(normalizeOnlineToken).filter(Boolean) : [];
    if (cleanTokens.length === 0) {
      throw new Error("no tokens selected");
    }
    const body = cleanTokens.length === 1 ? { token: cleanTokens[0] } : { tokens: cleanTokens };
    const res = await fetch("/api/v1/admin/cache/online/clear", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body),
    });
    if (handleUnauthorized(res)) return null;
    if (!res.ok) {
      throw new Error(await res.text());
    }
    return res.json();
  }

  function summarizeOnlineClear(data) {
    if (!data || typeof data !== "object") {
      return { total: 0, success: 0, failed: 0 };
    }
    const result = data.result || {};
    let total = toNumberOrZero(result.total);
    let success = toNumberOrZero(result.success);
    let failed = toNumberOrZero(result.failed);
    if (total > 0 || success > 0 || failed > 0) {
      return { total, success, failed };
    }
    const results = data.results || {};
    Object.values(results).forEach((item) => {
      const sub = item?.result || {};
      total += toNumberOrZero(sub.total);
      success += toNumberOrZero(sub.success);
      failed += toNumberOrZero(sub.failed);
    });
    return { total, success, failed };
  }

  async function loadCacheList() {
    const filter = String(document.getElementById("cacheTypeFilter")?.value || "").trim();
    const fetchByType = async (mediaType) => {
      const url = `/api/v1/admin/cache/list?media_type=${encodeURIComponent(mediaType)}`;
      const res = await fetch(url);
      if (handleUnauthorized(res)) return null;
      if (!res.ok) {
        throw new Error(await res.text());
      }
      const data = await res.json();
      return Array.isArray(data?.items) ? data.items : [];
    };

    if (filter) {
      const items = await fetchByType(filter);
      if (items === null) return;
      renderCacheList(items);
      return;
    }

    const [imageItems, videoItems] = await Promise.all([fetchByType("image"), fetchByType("video")]);
    if (imageItems === null || videoItems === null) return;
    const merged = imageItems.concat(videoItems);
    merged.sort((a, b) => {
      const left = toNumberOrZero(a?.mtime_ms ?? a?.updated_at);
      const right = toNumberOrZero(b?.mtime_ms ?? b?.updated_at);
      return right - left;
    });
    renderCacheList(merged);
  }

  async function refreshCacheView(options = {}) {
    await loadCacheSummary(options);
    await loadCacheList();
  }

  async function loadSelectedOnlineStats() {
    const tokens = selectedOnlineTokens();
    if (tokens.length === 0) {
      showToast("请先选择在线账号", "info");
      return;
    }
    await startOnlineLoadBatch({ tokens }, "选中账号");
  }

  async function loadAllOnlineStats() {
    const allTokens = cacheOnlineState.accounts.map((item) => normalizeOnlineToken(item.token)).filter(Boolean);
    if (allTokens.length === 0) {
      showToast("暂无在线账号", "info");
      return;
    }
    await startOnlineLoadBatch({ scope: "all" }, "全部账号");
  }

  async function clearSelectedOnlineAssets() {
    const tokens = selectedOnlineTokens();
    if (tokens.length === 0) {
      showToast("请先选择在线账号", "info");
      return;
    }
    if (cacheBatchState.running) {
      showToast("有任务正在运行，请稍候", "info");
      return;
    }
    if (!window.confirm(`确认清理选中的 ${tokens.length} 个账号在线资产？`)) return;
    await startOnlineClearBatch(tokens);
  }

  async function clearSingleOnlineAssets(token) {
    if (cacheBatchState.running) {
      showToast("有任务正在运行，请稍候", "info");
      return;
    }
    const cleanToken = normalizeOnlineToken(token);
    if (!cleanToken) return;
    const display = cacheOnlineState.accountMap.get(cleanToken)?.token_masked || formatTokenMask(cleanToken);
    if (!window.confirm(`确认清理账号 ${display} 的在线资产？`)) return;
    const data = await clearOnlineAssets([cleanToken]);
    if (!data) return;
    const summary = summarizeOnlineClear(data);
    showToast(`在线清理完成：成功 ${summary.success}，失败 ${summary.failed}`, "success");
    await loadCacheSummary({ token: cleanToken });
  }

  async function clearCache() {
    const filter = String(document.getElementById("cacheTypeFilter")?.value || "").trim();
    const confirmText = filter ? `确认清空 ${filter} 缓存？` : "确认清空全部缓存？";
    if (!window.confirm(confirmText)) return;

    const requestClear = async (mediaType) => {
      const res = await fetch("/api/v1/admin/cache/clear", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ media_type: mediaType }),
      });
      if (handleUnauthorized(res)) return null;
      if (!res.ok) {
        throw new Error(await res.text());
      }
      return res.json();
    };

    if (filter) {
      const single = await requestClear(filter);
      if (single === null) return;
    } else {
      const result = await Promise.all([requestClear("image"), requestClear("video")]);
      if (result[0] === null || result[1] === null) return;
    }
    showToast("缓存已清空", "success");
    await refreshCacheView();
  }

  async function deleteCacheItem(mediaType, name) {
    const res = await fetch("/api/v1/admin/cache/item/delete", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        media_type: mediaType,
        name,
      }),
    });
    if (handleUnauthorized(res)) return;
    if (!res.ok) {
      throw new Error(await res.text());
    }
    await refreshCacheView();
  }

  async function ensureGrokTabReady(tab) {
    const nextTab = String(tab || "imagine").toLowerCase();
    if (nextTab === "imagine" && !grokLazyState.imagineReady) {
      if (window.GrokImagine && typeof window.GrokImagine.ensureReady === "function") {
        await window.GrokImagine.ensureReady();
        grokLazyState.imagineReady = true;
        return;
      }
      await loadImagineConfig();
      grokLazyState.imagineReady = true;
      return;
    }
    if (nextTab === "cache" && !grokLazyState.cacheReady) {
      await refreshCacheView();
      grokLazyState.cacheReady = true;
    }
  }

  async function switchGrokToolTab(tab) {
    const nextTab = String(tab || "").toLowerCase();
    try {
      await ensureGrokTabReady(nextTab);
    } catch (err) {
      showToast(`加载 ${nextTab} 失败: ${err.message || err}`, "error");
    }
    if (nextTab !== "chat") {
      closeChatSidebar();
    } else if (!isMobileChatSidebar()) {
      openChatSidebar();
    }
    const sections = {
      chat: document.getElementById("grokChatSection"),
      imagine: document.getElementById("grokImagineSection"),
      video: document.getElementById("grokVideoSection"),
      voice: document.getElementById("grokVoiceSection"),
      cache: document.getElementById("grokCacheSection"),
    };
    Object.keys(sections).forEach((key) => {
      const section = sections[key];
      if (!section) return;
      if (key !== nextTab) {
        section.classList.add("section-hidden");
        section.style.display = "none";
        return;
      }
      section.classList.remove("section-hidden");
      section.style.display = "";
    });

    const tabs = document.querySelectorAll("#grokToolsTabs .tab-item");
    tabs.forEach((btn) => {
      const active = String(btn.dataset.tab || "").toLowerCase() === nextTab;
      btn.classList.toggle("active", active);
    });
    saveGrokToolsUIState({ activeToolTab: nextTab });
  }

  window.switchGrokToolTab = switchGrokToolTab;

  function bindEvents() {
    if (!window.GrokImagine) {
    document.querySelectorAll("[data-imagine-mode]").forEach((btn) => {
      btn.addEventListener("click", () => {
        const mode = String(btn.dataset.imagineMode || "auto").toLowerCase();
        applyImagineMode(mode);
      });
    });
    const imagineModeSelect = document.getElementById("imagineMode");
    if (imagineModeSelect) {
      imagineModeSelect.addEventListener("change", () => {
        applyImagineMode(imagineModeSelect.value);
      });
    }
    document.querySelectorAll("[data-imagine-run-mode]").forEach((btn) => {
      btn.addEventListener("click", () => {
        const value = String(btn.dataset.imagineRunMode || "single");
        imagineSetToggle("#imagineRunModeToggle", "imagineRunMode", value);
        saveGrokToolsUIState({ imagineRunMode: value });
      });
    });
    document.querySelectorAll("[data-imagine-quality]").forEach((btn) => {
      btn.addEventListener("click", () => {
        const value = normalizeImagineQuality(btn.dataset.imagineQuality || "lite");
        imagineSetToggle("#imagineQualityToggle", "imagineQuality", value);
        const model = imagineQualityModel(value);
        const modelSelect = document.getElementById("imagineModel");
        if (modelSelect) modelSelect.value = model;
        saveGrokToolsUIState({ imagineQuality: value, imagineModel: model });
      });
    });
    const imaginePrompt = document.getElementById("imaginePrompt");
    if (imaginePrompt) {
      imaginePrompt.addEventListener("keydown", async (event) => {
        if (event.key === "Enter" && !event.shiftKey) {
          event.preventDefault();
          try {
            await startImagine();
          } catch (err) {
            showToast(`启动失败: ${err.message || err}`, "error");
          }
        }
      });
      imaginePrompt.addEventListener("input", resizeImaginePrompt);
      resizeImaginePrompt();
    }
    [
      ["imagineRatio", "imagineRatio"],
      ["imagineModel", "imagineModel"],
      ["imagineConcurrent", "imagineConcurrent"],
      ["imagineNSFW", "imagineNSFW"],
    ].forEach(([id, key]) => {
      const input = document.getElementById(id);
      if (!input) return;
      const sync = () => {
        const raw = input.value;
        saveGrokToolsUIState({ [key]: id === "imagineConcurrent" ? Number(raw || 1) : String(raw || "") });
      };
      input.addEventListener("change", sync);
      input.addEventListener("input", sync);
    });
    const imagineRatioSelect = document.getElementById("imagineRatio");
    if (imagineRatioSelect) {
      imagineRatioSelect.addEventListener("change", syncImagineRatioUI);
      syncImagineRatioUI();
    }
    bindPersistedCheckbox("imagineAutoScroll", "imagineAutoScroll");
    bindPersistedCheckbox("imagineAutoDownload", "imagineAutoDownload");
    bindPersistedCheckbox("imagineAutoFilter", "imagineAutoFilter");
    bindPersistedCheckbox("imagineReverseInsert", "imagineReverseInsert");
    const folderBtn = document.getElementById("imagineSelectFolderBtn");
    const folderPath = document.getElementById("imagineFolderPath");
    const autoDownloadToggle = document.getElementById("imagineAutoDownload");
    const canPickDirectory = typeof window.showDirectoryPicker === "function";
    const syncImagineFolderButtonState = () => {
      if (!folderBtn) return;
      folderBtn.disabled = false;
      folderBtn.style.color = imagineDirectoryHandle ? "#059669" : "";
      if (canPickDirectory) {
        folderBtn.title = autoDownloadToggle && !autoDownloadToggle.checked
          ? "选择保存位置并自动开启自动保存"
          : "选择自动保存目录";
        return;
      }
      folderBtn.title = "当前环境不支持自定义保存目录，将使用浏览器默认下载位置";
    };
    if (folderBtn) {
      folderBtn.addEventListener("click", async () => {
        if (!canPickDirectory) {
          showToast("当前环境不支持自定义保存目录，将使用浏览器默认下载位置", "info");
          return;
        }
        try {
          imagineDirectoryHandle = await window.showDirectoryPicker({ mode: "readwrite" });
          imagineUseFileSystemAPI = true;
          if (autoDownloadToggle && !autoDownloadToggle.checked) {
            autoDownloadToggle.checked = true;
            autoDownloadToggle.dispatchEvent(new Event("change", { bubbles: true }));
          }
          if (folderPath) {
            folderPath.textContent = imagineDirectoryHandle.name;
          }
          syncImagineFolderButtonState();
          showToast(`已选择保存位置: ${imagineDirectoryHandle.name}`, "success");
        } catch (err) {
          if (err && err.name !== "AbortError") {
            showToast("选择保存位置失败", "error");
          }
        }
      });
      syncImagineFolderButtonState();
    }
    if (autoDownloadToggle && folderBtn) {
      autoDownloadToggle.addEventListener("change", syncImagineFolderButtonState);
    }
    const startBtn = document.getElementById("imagineStartBtn");
    const stopBtn = document.getElementById("imagineStopBtn");
    const clearBtn = document.getElementById("imagineClearBtn");
    if (startBtn) startBtn.addEventListener("click", () => startImagine());
    if (stopBtn) stopBtn.addEventListener("click", () => stopImagine());
    if (clearBtn) clearBtn.addEventListener("click", () => clearImagineGrid());
    const batchDownloadBtn = document.getElementById("batchDownloadBtn");
    const toggleSelectAllBtn = document.getElementById("toggleSelectAllBtn");
    const downloadSelectedBtn = document.getElementById("downloadSelectedBtn");
    const exitSelectionBtn = document.getElementById("exitSelectionBtn");
    if (batchDownloadBtn) batchDownloadBtn.addEventListener("click", toggleImagineSelectionMode);
    if (toggleSelectAllBtn) toggleSelectAllBtn.addEventListener("click", toggleImagineSelectAll);
    if (downloadSelectedBtn) downloadSelectedBtn.addEventListener("click", downloadSelectedImages);
    if (exitSelectionBtn) exitSelectionBtn.addEventListener("click", exitImagineSelectionMode);
    const imagineGrid = document.getElementById("imagineGrid");
    if (imagineGrid) {
      imagineGrid.addEventListener("click", (event) => {
        const item = event.target.closest(".waterfall-item");
        if (!item) return;
        if (!item.classList.contains("is-ready")) return;
        if (imagineSelectionMode) {
          toggleImagineItemSelection(item);
          return;
        }
      });
    }
    }

    const videoStartBtn = document.getElementById("videoStartBtn");
    if (videoStartBtn) {
      videoStartBtn.addEventListener("click", async () => {
        try {
          await startVideo();
        } catch (err) {
          showToast(`启动失败: ${err.message || err}`, "error");
        }
      });
    }
    const videoStopBtn = document.getElementById("videoStopBtn");
    if (videoStopBtn) {
      videoStopBtn.addEventListener("click", async () => {
        try {
          await stopVideo();
        } catch (err) {
          showToast(`停止失败: ${err.message || err}`, "error");
        }
      });
    }
    const videoClearBtn = document.getElementById("videoClearBtn");
    if (videoClearBtn) {
      videoClearBtn.addEventListener("click", () => {
        resetVideoOutput();
        setVideoStatus(t("common.notConnected"));
      });
    }
    const videoStage = document.getElementById("videoStage");
    if (videoStage) {
      videoStage.addEventListener("click", async (event) => {
        const target = event.target.closest(".video-download");
        if (!target || !videoStage.contains(target)) return;
        event.preventDefault();
        const item = target.closest(".video-item");
        if (!item) return;
        const url = item.dataset.url || target.dataset.url || "";
        if (!url) return;
        try {
          const response = await fetch(url, { mode: "cors" });
          if (!response.ok) {
            throw new Error("download_failed");
          }
          const blob = await response.blob();
          const blobUrl = URL.createObjectURL(blob);
          const anchor = document.createElement("a");
          anchor.href = blobUrl;
          const index = item.dataset.index || "";
          anchor.download = index ? `grok_video_${index}.mp4` : "grok_video.mp4";
          document.body.appendChild(anchor);
          anchor.click();
          anchor.remove();
          URL.revokeObjectURL(blobUrl);
        } catch (err) {
          showToast(t("video.downloadFailed"), "error");
        }
      });
    }
    const videoSelectImageBtn = document.getElementById("videoSelectImageBtn");
    const videoImageFileInput = document.getElementById("videoImageFileInput");
    const videoClearImageBtn = document.getElementById("videoClearImageBtn");
    const videoImageUrl = document.getElementById("videoImageUrl");
    if (videoSelectImageBtn && videoImageFileInput) {
      videoSelectImageBtn.addEventListener("click", () => videoImageFileInput.click());
      videoImageFileInput.addEventListener("change", () => {
        const file = videoImageFileInput.files && videoImageFileInput.files[0];
        const fileName = document.getElementById("videoImageFileName");
        if (!file) {
          videoState.fileDataURL = "";
          if (fileName) fileName.textContent = "未选择文件";
          return;
        }
        if (videoImageUrl) videoImageUrl.value = "";
        if (fileName) fileName.textContent = file.name;
        const reader = new FileReader();
        reader.onload = () => {
          videoState.fileDataURL = typeof reader.result === "string" ? reader.result : "";
        };
        reader.onerror = () => {
          videoState.fileDataURL = "";
          showToast("读取参考图失败", "error");
        };
        reader.readAsDataURL(file);
      });
    }
    if (videoClearImageBtn) {
      videoClearImageBtn.addEventListener("click", () => {
        videoState.fileDataURL = "";
        if (videoImageFileInput) videoImageFileInput.value = "";
        const fileName = document.getElementById("videoImageFileName");
        if (fileName) fileName.textContent = "未选择文件";
      });
    }
    if (videoImageUrl) {
      videoImageUrl.addEventListener("input", () => {
        if (!videoImageUrl.value.trim()) return;
        videoState.fileDataURL = "";
        if (videoImageFileInput) videoImageFileInput.value = "";
        const fileName = document.getElementById("videoImageFileName");
        if (fileName) fileName.textContent = "未选择文件";
      });
    }
    const videoPrompt = document.getElementById("videoPrompt");
    if (videoPrompt) {
      videoPrompt.addEventListener("keydown", async (event) => {
        if ((event.metaKey || event.ctrlKey) && event.key === "Enter") {
          event.preventDefault();
          try {
            await startVideo();
          } catch (err) {
            showToast(`启动失败: ${err.message || err}`, "error");
          }
        }
      });
    }
    [
      ["videoRatio", "videoRatio"],
      ["videoLength", "videoLength"],
      ["videoResolution", "videoResolution"],
      ["videoPreset", "videoPreset"],
      ["videoEffort", "videoEffort"],
    ].forEach(([id, key]) => {
      const input = document.getElementById(id);
      if (!input) return;
      const sync = () => {
        saveGrokToolsUIState({ [key]: String(input.value || "") });
        if (id !== "videoEffort") updateVideoMeta();
      };
      input.addEventListener("change", sync);
      input.addEventListener("input", sync);
    });

    const voiceStartBtn = document.getElementById("voiceStartBtn");
    if (voiceStartBtn) {
      voiceStartBtn.addEventListener("click", async () => {
        try {
          await startVoiceSession();
        } catch (err) {
          if (!err?._voiceLogged) {
            appendVoiceLog(err.message || "Voice start failed");
          }
          showToast(`Voice 启动失败: ${err.message || err}`, "error");
        }
      });
    }
    const voiceStopBtn = document.getElementById("voiceStopBtn");
    if (voiceStopBtn) {
      voiceStopBtn.addEventListener("click", async () => {
        try {
          await stopVoiceSession();
        } catch (err) {
          showToast(`Voice 停止失败: ${err.message || err}`, "error");
        }
      });
    }
    const voiceClearLogBtn = document.getElementById("voiceClearLogBtn");
    if (voiceClearLogBtn) {
      voiceClearLogBtn.addEventListener("click", () => clearVoiceLog());
    }
    const voiceCopyLogBtn = document.getElementById("voiceCopyLogBtn");
    if (voiceCopyLogBtn) {
      voiceCopyLogBtn.addEventListener("click", () => {
        const content = String(document.getElementById("voiceLogOutput")?.textContent || "").trim();
        if (!content) {
          showToast("暂无可复制日志", "info");
          return;
        }
        copyToClipboard(content);
      });
    }
    const voiceName = document.getElementById("voiceName");
    const voiceCustomID = document.getElementById("voiceCustomID");
    const voicePersonality = document.getElementById("voicePersonality");
    const voiceSpeed = document.getElementById("voiceSpeed");
    const voiceInstruction = document.getElementById("voiceInstruction");
    [voiceName, voiceCustomID, voicePersonality, voiceSpeed, voiceInstruction].forEach((input) => {
      if (!input) return;
      const sync = () => {
        updateVoiceMeta();
        saveGrokToolsUIState({
          voiceName: String(document.getElementById("voiceName")?.value || "ara"),
          voiceCustomID: String(document.getElementById("voiceCustomID")?.value || ""),
          voicePersonality: String(document.getElementById("voicePersonality")?.value || "assistant"),
          voiceSpeed: Number(document.getElementById("voiceSpeed")?.value || 1),
          voiceInstruction: String(document.getElementById("voiceInstruction")?.value || ""),
        });
      };
      input.addEventListener("change", sync);
      input.addEventListener("input", sync);
    });
    const voiceMuteOutputBtn = document.getElementById("voiceMuteOutputBtn");
    if (voiceMuteOutputBtn) {
      voiceMuteOutputBtn.addEventListener("click", () => {
        voiceState.outputMuted = !voiceState.outputMuted;
        syncVoiceOutputMute();
      });
    }

    const cacheRefreshBtn = document.getElementById("cacheRefreshBtn");
    if (cacheRefreshBtn) {
      cacheRefreshBtn.addEventListener("click", async () => {
        try {
          await refreshCacheView();
          showToast("缓存已刷新", "success");
        } catch (err) {
          showToast(`刷新失败: ${err.message || err}`, "error");
        }
      });
    }
    const cacheClearBtn = document.getElementById("cacheClearBtn");
    if (cacheClearBtn) {
      cacheClearBtn.addEventListener("click", async () => {
        try {
          await clearCache();
        } catch (err) {
          showToast(`清空失败: ${err.message || err}`, "error");
        }
      });
    }
    const cacheFilter = document.getElementById("cacheTypeFilter");
    if (cacheFilter) {
      cacheFilter.addEventListener("change", async () => {
        try {
          await loadCacheList();
        } catch (err) {
          showToast(`加载失败: ${err.message || err}`, "error");
        }
      });
    }

    const cacheListBody = document.getElementById("cacheListBody");
    if (cacheListBody) {
      cacheListBody.addEventListener("click", async (event) => {
        const btn = event.target.closest(".cache-delete-btn");
        if (!btn || !cacheListBody.contains(btn)) return;
        const mediaType = decodeURIComponent(btn.dataset.mediaType || "");
        const name = decodeURIComponent(btn.dataset.name || "");
        if (!mediaType || !name) return;
        if (!window.confirm(`确认删除 ${mediaType}/${name} ?`)) return;
        try {
          await deleteCacheItem(mediaType, name);
          showToast("删除成功", "success");
        } catch (err) {
          showToast(`删除失败: ${err.message || err}`, "error");
        }
      });
    }

    const cacheOnlineBody = document.getElementById("cacheOnlineBody");
    if (cacheOnlineBody) {
      cacheOnlineBody.addEventListener("change", (event) => {
        const input = event.target.closest(".cache-online-check");
        if (!input || !cacheOnlineBody.contains(input)) return;
        const token = normalizeOnlineToken(decodeURIComponent(input.dataset.token || ""));
        if (!token) return;
        if (input.checked) {
          cacheOnlineState.selectedTokens.add(token);
        } else {
          cacheOnlineState.selectedTokens.delete(token);
        }
        syncCacheOnlineSelectAll();
      });
      cacheOnlineBody.addEventListener("click", async (event) => {
        const btn = event.target.closest(".cache-online-clear-btn");
        if (!btn || !cacheOnlineBody.contains(btn)) return;
        const token = normalizeOnlineToken(decodeURIComponent(btn.dataset.token || ""));
        if (!token) return;
        try {
          await clearSingleOnlineAssets(token);
        } catch (err) {
          showToast(`在线清理失败: ${err.message || err}`, "error");
        }
      });
    }

    const cacheOnlineSelectAll = document.getElementById("cacheOnlineSelectAll");
    if (cacheOnlineSelectAll) {
      cacheOnlineSelectAll.addEventListener("change", () => {
        const body = document.getElementById("cacheOnlineBody");
        if (!body) return;
        const checked = !!cacheOnlineSelectAll.checked;
        const checkboxes = Array.from(body.querySelectorAll("input.cache-online-check"));
        checkboxes.forEach((item) => {
          const token = normalizeOnlineToken(decodeURIComponent(item.dataset.token || ""));
          if (!token) return;
          item.checked = checked;
          if (checked) {
            cacheOnlineState.selectedTokens.add(token);
          } else {
            cacheOnlineState.selectedTokens.delete(token);
          }
        });
        syncCacheOnlineSelectAll();
      });
    }

    const cacheOnlineLoadSelectedBtn = document.getElementById("cacheOnlineLoadSelectedBtn");
    if (cacheOnlineLoadSelectedBtn) {
      cacheOnlineLoadSelectedBtn.addEventListener("click", async () => {
        try {
          await loadSelectedOnlineStats();
        } catch (err) {
          showToast(`加载失败: ${err.message || err}`, "error");
        }
      });
    }
    const cacheOnlineLoadAllBtn = document.getElementById("cacheOnlineLoadAllBtn");
    if (cacheOnlineLoadAllBtn) {
      cacheOnlineLoadAllBtn.addEventListener("click", async () => {
        try {
          await loadAllOnlineStats();
        } catch (err) {
          showToast(`加载失败: ${err.message || err}`, "error");
        }
      });
    }
    const cacheOnlineClearSelectedBtn = document.getElementById("cacheOnlineClearSelectedBtn");
    if (cacheOnlineClearSelectedBtn) {
      cacheOnlineClearSelectedBtn.addEventListener("click", async () => {
        try {
          await clearSelectedOnlineAssets();
        } catch (err) {
          showToast(`在线清理失败: ${err.message || err}`, "error");
        }
      });
    }
    const cacheOnlineBatchCancelBtn = document.getElementById("cacheOnlineBatchCancelBtn");
    if (cacheOnlineBatchCancelBtn) {
      cacheOnlineBatchCancelBtn.addEventListener("click", async () => {
        try {
          await cancelCacheBatchTask();
        } catch (err) {
          showToast(`取消失败: ${err.message || err}`, "error");
        }
      });
    }
  }

  async function init() {
    await initChat();
    bindEvents();
    window.addEventListener("beforeunload", () => {
      if (window.GrokImagine && typeof window.GrokImagine.stop === "function") {
        window.GrokImagine.stop({ silent: true });
      }
      if (Array.isArray(imagineState.taskIDs) && imagineState.taskIDs.length > 0) {
        closeImagineConnections(true);
        try {
          const payload = JSON.stringify({ task_ids: imagineState.taskIDs });
          navigator.sendBeacon(
            "/api/v1/admin/imagine/stop",
            new Blob([payload], { type: "application/json" }),
          );
        } catch (err) {
          // ignore unload errors
        }
      }
      closeVideoStream();
      stopVideoElapsedTimer();
      if (videoState.taskID) {
        try {
          const payload = JSON.stringify({ task_ids: [videoState.taskID] });
          navigator.sendBeacon(
            "/api/v1/admin/video/stop",
            new Blob([payload], { type: "application/json" }),
          );
        } catch (err) {
          // ignore
        }
      }
      stopVoiceSession().catch(() => {});
      closeCacheBatchStream();
    });
    const uiState = loadGrokToolsUIState();
    await switchGrokToolTab(String(uiState.activeToolTab || "imagine"));
    if (window.GrokImagine && typeof window.GrokImagine.init === "function") {
      window.GrokImagine.init({ uiState, saveState: saveGrokToolsUIState, showToast });
    } else {
    syncImagineModeUI(String(uiState.imagineMode || document.getElementById("imagineMode")?.value || "auto"), false);
    const imagineRatio = document.getElementById("imagineRatio");
    const imagineModel = document.getElementById("imagineModel");
    const imagineConcurrent = document.getElementById("imagineConcurrent");
    const imagineNSFW = document.getElementById("imagineNSFW");
    if (imagineRatio && typeof uiState.imagineRatio === "string" && uiState.imagineRatio) {
      imagineRatio.value = uiState.imagineRatio;
    }
    if (imagineModel && typeof uiState.imagineModel === "string" && uiState.imagineModel) {
      imagineModel.value = uiState.imagineModel;
    }
    const savedQuality = typeof uiState.imagineQuality === "string" ? uiState.imagineQuality : "";
    const quality = savedQuality
      ? normalizeImagineQuality(savedQuality)
      : (imagineModel?.value === "grok-imagine-image-lite"
        ? "basic"
        : (imagineModel?.value === "grok-imagine-image-pro" ? "quality" : "lite"));
    imagineSetToggle("#imagineQualityToggle", "imagineQuality", quality);
    const savedRunMode = typeof uiState.imagineRunMode === "string" ? uiState.imagineRunMode : "single";
    imagineSetToggle("#imagineRunModeToggle", "imagineRunMode", savedRunMode === "continuous" ? "continuous" : "single");
    if (imagineConcurrent && typeof uiState.imagineConcurrent === "number" && Number.isFinite(uiState.imagineConcurrent)) {
      imagineConcurrent.value = String(uiState.imagineConcurrent);
    }
    if (imagineNSFW && typeof uiState.imagineNSFW === "string" && uiState.imagineNSFW) {
      imagineNSFW.value = uiState.imagineNSFW;
    }
    resetImagineMetrics();
    updateImagineActiveCount();
    syncImagineRatioUI();
    resizeImaginePrompt();
    }
    const videoRatio = document.getElementById("videoRatio");
    const videoLength = document.getElementById("videoLength");
    const videoResolution = document.getElementById("videoResolution");
    const videoPreset = document.getElementById("videoPreset");
    const videoEffort = document.getElementById("videoEffort");
    if (videoRatio && typeof uiState.videoRatio === "string") videoRatio.value = uiState.videoRatio;
    if (videoLength && typeof uiState.videoLength === "string") videoLength.value = uiState.videoLength;
    if (videoResolution && typeof uiState.videoResolution === "string") videoResolution.value = uiState.videoResolution;
    if (videoPreset && typeof uiState.videoPreset === "string") videoPreset.value = uiState.videoPreset;
    if (videoEffort && typeof uiState.videoEffort === "string") videoEffort.value = uiState.videoEffort;
    resetVideoOutput();
    updateVideoMeta();
    setVideoButtons(false);
    setVideoStatus(t("common.notConnected"));
    const voiceName = document.getElementById("voiceName");
    const voiceCustomID = document.getElementById("voiceCustomID");
    const voicePersonality = document.getElementById("voicePersonality");
    const voiceSpeed = document.getElementById("voiceSpeed");
    const voiceInstruction = document.getElementById("voiceInstruction");
    if (voiceName && typeof uiState.voiceName === "string" && uiState.voiceName) voiceName.value = uiState.voiceName;
    if (voiceCustomID && typeof uiState.voiceCustomID === "string") voiceCustomID.value = uiState.voiceCustomID;
    if (voicePersonality && typeof uiState.voicePersonality === "string" && uiState.voicePersonality) voicePersonality.value = uiState.voicePersonality;
    if (voiceSpeed && typeof uiState.voiceSpeed === "number" && Number.isFinite(uiState.voiceSpeed)) voiceSpeed.value = String(uiState.voiceSpeed);
    if (voiceInstruction && typeof uiState.voiceInstruction === "string") voiceInstruction.value = uiState.voiceInstruction;
    updateVoiceMeta();
    startVoiceVisualizer();
    stopVoiceVisualizer();
    window.addEventListener("resize", buildVoiceVisualizerBars);
    setVoiceButtons(false);
    setVoiceStatus(t("common.notConnected"));
    syncVoiceOutputMute();
    updateCacheBatchUI();
  }

  document.addEventListener("DOMContentLoaded", init);
})();
