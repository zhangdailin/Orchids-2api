(function () {
  const promptEl = document.getElementById("prompt");
  const ratioEl = document.getElementById("ratio");
  const nsfwEl = document.getElementById("nsfw");
  const concurrentEl = document.getElementById("concurrent");

  const startBtn = document.getElementById("startBtn");
  const stopBtn = document.getElementById("stopBtn");
  const clearBtn = document.getElementById("clearBtn");

  const autoScrollToggle = document.getElementById("autoScrollToggle");
  const autoDownloadToggle = document.getElementById("autoDownloadToggle");
  const autoFilterToggle = document.getElementById("autoFilterToggle");
  const reverseInsertToggle = document.getElementById("reverseInsertToggle");

  const selectFolderBtn = document.getElementById("selectFolderBtn");
  const folderPathEl = document.getElementById("folderPath");

  const batchToggleBtn = document.getElementById("batchToggleBtn");
  const selectAllBtn = document.getElementById("selectAllBtn");
  const downloadSelectedBtn = document.getElementById("downloadSelectedBtn");
  const selectedCountEl = document.getElementById("selectedCount");

  const statusEl = document.getElementById("status");
  const countValueEl = document.getElementById("countValue");
  const activeValueEl = document.getElementById("activeValue");
  const latencyValueEl = document.getElementById("latencyValue");
  const modeValueEl = document.getElementById("modeValue");

  const modeButtons = Array.from(document.querySelectorAll(".mode-btn"));
  const gridEl = document.getElementById("imageGrid");
  const emptyStateEl = document.getElementById("emptyState");

  const lightboxEl = document.getElementById("lightbox");
  const lightboxImgEl = document.getElementById("lightboxImg");
  const lightboxCloseEl = document.getElementById("lightboxClose");
  const lightboxPrevEl = document.getElementById("lightboxPrev");
  const lightboxNextEl = document.getElementById("lightboxNext");

  const MODE_STORAGE_KEY = "orchids_public_imagine_mode";
  const PREF_STORAGE_KEY = "orchids_public_imagine_prefs";

  const state = {
    running: false,
    taskIDs: [],
    wsList: [],
    sseList: [],
    fallbackTimers: [],
    imageMap: new Map(),
    nextImageID: 1,
    imageCount: 0,
    totalLatency: 0,
    latencyCount: 0,
    modePreference: "auto",
    finalMinBytes: 100000,
    downloadedKeys: new Set(),
    lightboxIndex: -1,
    selectionMode: false,
    selectedKeys: new Set(),
    directoryHandle: null,
    useFileSystemAPI: false,
    restarting: false,
  };

  function setStatus(message, level) {
    window.PublicApp.setStatus(statusEl, message, level);
  }

  function setRunning(running) {
    state.running = running;
    startBtn.disabled = running;
    stopBtn.disabled = !running;
  }

  function updateCount() {
    countValueEl.textContent = String(state.imageCount);
  }

  function updateLatency(elapsedMS) {
    const value = Number(elapsedMS);
    if (Number.isFinite(value) && value > 0) {
      state.totalLatency += value;
      state.latencyCount += 1;
    }
    if (state.latencyCount <= 0) {
      latencyValueEl.textContent = "-";
      return;
    }
    latencyValueEl.textContent = `${Math.round(state.totalLatency / state.latencyCount)} ms`;
  }

  function countOpenWS() {
    let count = 0;
    for (const ws of state.wsList) {
      if (ws && ws.readyState === WebSocket.OPEN) {
        count += 1;
      }
    }
    return count;
  }

  function countOpenSSE() {
    let count = 0;
    for (const es of state.sseList) {
      if (es && es.readyState === EventSource.OPEN) {
        count += 1;
      }
    }
    return count;
  }

  function updateModeValue() {
    const wsOpen = countOpenWS();
    const sseOpen = countOpenSSE();
    let runtime = "Idle";
    if (wsOpen > 0 && sseOpen > 0) {
      runtime = "WS+SSE";
    } else if (wsOpen > 0) {
      runtime = "WS";
    } else if (sseOpen > 0) {
      runtime = "SSE";
    }
    modeValueEl.textContent = `${state.modePreference.toUpperCase()} / ${runtime}`;
  }

  function updateActive() {
    const active = countOpenWS() + countOpenSSE();
    activeValueEl.textContent = String(active);
    updateModeValue();
  }

  function sanitizeFileName(name) {
    return String(name || "")
      .replace(/[^a-zA-Z0-9._-]+/g, "_")
      .replace(/_+/g, "_")
      .replace(/^_+|_+$/g, "")
      .slice(0, 80) || "image";
  }

  function inferExtensionFromSource(src) {
    const raw = String(src || "");
    if (raw.startsWith("data:image/png")) return "png";
    if (raw.startsWith("data:image/gif")) return "gif";
    if (raw.startsWith("data:image/webp")) return "webp";
    if (raw.startsWith("data:image/jpeg") || raw.startsWith("data:image/jpg")) return "jpg";
    if (raw.startsWith("data:image")) return "jpg";

    try {
      const url = new URL(raw, window.location.origin);
      const name = url.pathname.split("/").pop() || "";
      const idx = name.lastIndexOf(".");
      if (idx > 0 && idx < name.length - 1) {
        const ext = name.slice(idx + 1).toLowerCase();
        if (ext) return ext;
      }
    } catch (err) {
      // ignore parse failure
    }

    return "jpg";
  }

  function buildFileName(key, src) {
    const ext = inferExtensionFromSource(src);
    const safe = sanitizeFileName(key);
    return `imagine_${Date.now()}_${safe}.${ext}`;
  }

  function dataURLToBlob(dataURL) {
    const raw = String(dataURL || "");
    if (!raw.startsWith("data:")) {
      return null;
    }

    const commaIndex = raw.indexOf(",");
    if (commaIndex < 0) {
      return null;
    }

    const header = raw.slice(0, commaIndex);
    const body = raw.slice(commaIndex + 1);
    const mimeMatch = header.match(/data:([^;]+);base64/i);
    const mime = mimeMatch && mimeMatch[1] ? mimeMatch[1] : "application/octet-stream";

    try {
      const binary = atob(body);
      const bytes = new Uint8Array(binary.length);
      for (let i = 0; i < binary.length; i += 1) {
        bytes[i] = binary.charCodeAt(i);
      }
      return new Blob([bytes], { type: mime });
    } catch (err) {
      return null;
    }
  }

  async function sourceToBlob(src) {
    const raw = String(src || "").trim();
    if (!raw) return null;

    if (raw.startsWith("data:")) {
      return dataURLToBlob(raw);
    }

    const response = await fetch(raw, { mode: "cors" });
    if (!response.ok) {
      throw new Error(`fetch_failed_${response.status}`);
    }
    return await response.blob();
  }

  async function saveBlobToDirectory(blob, fileName) {
    if (!state.directoryHandle) {
      throw new Error("missing_directory_handle");
    }

    const handle = await state.directoryHandle.getFileHandle(fileName, { create: true });
    const writable = await handle.createWritable();
    await writable.write(blob);
    await writable.close();
  }

  function downloadBlob(blob, fileName) {
    const url = URL.createObjectURL(blob);
    const link = document.createElement("a");
    link.href = url;
    link.download = fileName;
    link.rel = "noopener";
    link.style.display = "none";
    document.body.appendChild(link);
    link.click();
    link.remove();
    URL.revokeObjectURL(url);
  }

  function downloadByAnchor(src, fileName) {
    const link = document.createElement("a");
    link.href = src;
    link.download = fileName;
    link.rel = "noopener";
    link.style.display = "none";
    document.body.appendChild(link);
    link.click();
    link.remove();
  }

  async function downloadSource(src, key, preferDirectory) {
    const fileName = buildFileName(key, src);

    if (preferDirectory && state.useFileSystemAPI && state.directoryHandle) {
      const blob = await sourceToBlob(src);
      if (!blob) {
        throw new Error("blob_decode_failed");
      }
      await saveBlobToDirectory(blob, fileName);
      return;
    }

    if (String(src || "").startsWith("data:")) {
      downloadByAnchor(src, fileName);
      return;
    }

    try {
      const blob = await sourceToBlob(src);
      if (blob) {
        downloadBlob(blob, fileName);
        return;
      }
    } catch (err) {
      // fallback to direct anchor
    }

    downloadByAnchor(src, fileName);
  }

  function estimateBase64Bytes(raw) {
    let base64 = String(raw || "").trim();
    if (!base64) return null;

    if (base64.startsWith("http://") || base64.startsWith("https://") || (base64.startsWith("/") && !base64.startsWith("//"))) {
      return null;
    }

    if (base64.startsWith("data:")) {
      const comma = base64.indexOf(",");
      base64 = comma >= 0 ? base64.slice(comma + 1) : "";
    }

    base64 = base64.replace(/\s/g, "");
    if (!base64) return 0;

    let padding = 0;
    if (base64.endsWith("==")) padding = 2;
    else if (base64.endsWith("=")) padding = 1;

    return Math.max(0, Math.floor((base64.length * 3) / 4) - padding);
  }

  function shouldFilterImage(payload, isFinal) {
    if (!autoFilterToggle.checked || !isFinal) {
      return false;
    }
    const source = window.PublicApp.detectImageSource(payload);
    const bytes = estimateBase64Bytes(payload.b64_json || source);
    if (bytes === null) return false;
    return bytes < state.finalMinBytes;
  }

  function updateEmptyState() {
    emptyStateEl.style.display = state.imageCount > 0 ? "none" : "block";
  }

  function updateSelectionUI() {
    const selectedCount = state.selectedKeys.size;
    selectedCountEl.textContent = String(selectedCount);

    if (!state.selectionMode) {
      selectAllBtn.classList.add("hidden");
      downloadSelectedBtn.classList.add("hidden");
      return;
    }

    selectAllBtn.classList.remove("hidden");
    downloadSelectedBtn.classList.remove("hidden");
    downloadSelectedBtn.disabled = selectedCount === 0;

    const total = state.imageMap.size;
    const allSelected = total > 0 && selectedCount === total;
    selectAllBtn.textContent = allSelected ? "Clear All" : "Select All";
  }

  function setSelectionMode(enabled) {
    state.selectionMode = Boolean(enabled);
    gridEl.classList.toggle("selection-mode", state.selectionMode);
    batchToggleBtn.textContent = state.selectionMode ? "Cancel Batch" : "Batch";

    if (!state.selectionMode) {
      state.selectedKeys.clear();
      for (const card of state.imageMap.values()) {
        card.classList.remove("selected");
      }
    }

    updateSelectionUI();
  }

  function toggleCardSelectionByKey(key) {
    if (!state.selectionMode) return;

    const card = state.imageMap.get(key);
    if (!card) return;

    if (state.selectedKeys.has(key)) {
      state.selectedKeys.delete(key);
      card.classList.remove("selected");
    } else {
      state.selectedKeys.add(key);
      card.classList.add("selected");
    }

    updateSelectionUI();
  }

  function selectAllOrClearAll() {
    if (!state.selectionMode) return;

    const allKeys = Array.from(state.imageMap.keys());
    const allSelected = allKeys.length > 0 && state.selectedKeys.size === allKeys.length;

    state.selectedKeys.clear();
    for (const [key, card] of state.imageMap.entries()) {
      if (allSelected) {
        card.classList.remove("selected");
      } else {
        state.selectedKeys.add(key);
        card.classList.add("selected");
      }
    }

    updateSelectionUI();
  }

  async function downloadSelectedImages() {
    if (!state.selectionMode || state.selectedKeys.size === 0) {
      setStatus("Select at least one image", "error");
      return;
    }

    const targets = Array.from(state.selectedKeys)
      .map((key) => ({ key, card: state.imageMap.get(key) }))
      .filter((item) => item.card);

    if (targets.length === 0) {
      setStatus("No selectable image found", "error");
      return;
    }

    downloadSelectedBtn.disabled = true;

    try {
      if (typeof window.JSZip === "undefined") {
        for (const item of targets) {
          const src = String(item.card.dataset.src || "").trim();
          if (!src) continue;
          await downloadSource(src, item.key, false);
        }
        setStatus(`Downloaded ${targets.length} images`, "ok");
        setSelectionMode(false);
        return;
      }

      const zip = new window.JSZip();
      const folder = zip.folder("images");
      let processed = 0;

      for (const item of targets) {
        const src = String(item.card.dataset.src || "").trim();
        if (!src) continue;

        try {
          const blob = await sourceToBlob(src);
          if (!blob) continue;
          const fileName = buildFileName(item.key, src);
          folder.file(fileName, blob);
          processed += 1;
          setStatus(`Zipping ${processed}/${targets.length}...`);
        } catch (err) {
          // skip inaccessible image
        }
      }

      if (processed <= 0) {
        throw new Error("No image could be zipped");
      }

      const zipBlob = await zip.generateAsync({ type: "blob" });
      const outName = `imagine_${new Date().toISOString().slice(0, 10)}_${Date.now()}.zip`;
      downloadBlob(zipBlob, outName);

      setStatus(`Packed ${processed} images`, "ok");
      setSelectionMode(false);
    } catch (err) {
      setStatus(err.message || "Batch download failed", "error");
    } finally {
      downloadSelectedBtn.disabled = false;
      updateSelectionUI();
    }
  }

  function insertCard(card) {
    if (reverseInsertToggle.checked) {
      gridEl.prepend(card);
    } else {
      gridEl.appendChild(card);
    }
  }

  function maybeAutoScroll() {
    if (!autoScrollToggle.checked) return;
    if (reverseInsertToggle.checked) {
      window.scrollTo({ top: 0, behavior: "smooth" });
      return;
    }
    window.scrollTo({ top: document.body.scrollHeight, behavior: "smooth" });
  }

  function removeImageCard(key) {
    const card = state.imageMap.get(key);
    if (!card) return;
    state.imageMap.delete(key);
    state.downloadedKeys.delete(key);
    state.selectedKeys.delete(key);
    card.remove();
    state.imageCount = Math.max(0, state.imageCount - 1);
    updateCount();
    updateEmptyState();
    updateSelectionUI();
  }

  function createImageCard(key) {
    const card = document.createElement("div");
    card.className = "image-card";
    card.dataset.key = key;

    const img = document.createElement("img");
    img.loading = "lazy";
    img.decoding = "async";
    img.alt = `imagine-${key}`;

    const selectChip = document.createElement("span");
    selectChip.className = "select-chip";
    selectChip.textContent = "✓";

    const meta = document.createElement("div");
    meta.className = "image-meta";

    const left = document.createElement("span");
    left.className = "image-index";

    const right = document.createElement("span");
    right.className = "image-latency";

    meta.appendChild(left);
    meta.appendChild(right);

    card.appendChild(img);
    card.appendChild(selectChip);
    card.appendChild(meta);

    card.addEventListener("click", (event) => {
      if (!state.selectionMode) return;
      event.preventDefault();
      toggleCardSelectionByKey(key);
    });

    img.addEventListener("click", (event) => {
      if (state.selectionMode) return;
      event.preventDefault();
      event.stopPropagation();
      openLightboxByCard(card);
    });

    return card;
  }

  function getOrCreateImageKey(payload) {
    const candidate = String(payload.image_id || payload.imageId || "").trim();
    if (candidate) return candidate;

    const seqCandidate = Number(payload.sequence);
    if (Number.isFinite(seqCandidate) && seqCandidate > 0) {
      return `seq-${seqCandidate}`;
    }

    const key = `auto-${state.nextImageID}`;
    state.nextImageID += 1;
    return key;
  }

  function maybeAutoDownload(source, key, isFinal) {
    if (!isFinal || !autoDownloadToggle.checked || state.downloadedKeys.has(key)) {
      return;
    }

    state.downloadedKeys.add(key);
    downloadSource(source, key, true).catch(() => {
      downloadSource(source, key, false).catch(() => {
        // keep silent to avoid breaking stream
      });
    });
  }

  function upsertImage(payload, isFinal) {
    if (!payload || typeof payload !== "object") return;

    const source = window.PublicApp.detectImageSource(payload);
    if (!source) return;

    const key = getOrCreateImageKey(payload);
    const filtered = shouldFilterImage(payload, isFinal);
    if (filtered) {
      removeImageCard(key);
      return;
    }

    let card = state.imageMap.get(key);
    const isNew = !card;

    if (!card) {
      card = createImageCard(key);
      state.imageMap.set(key, card);
      insertCard(card);
      state.imageCount += 1;
      updateCount();
      updateEmptyState();
      updateSelectionUI();
    }

    const img = card.querySelector("img");
    const left = card.querySelector(".image-index");
    const right = card.querySelector(".image-latency");

    img.src = source;
    card.dataset.src = source;
    left.textContent = payload.sequence ? `#${payload.sequence}` : `#${key}`;
    right.textContent = payload.elapsed_ms ? `${payload.elapsed_ms}ms` : "";

    if (payload.elapsed_ms) {
      updateLatency(payload.elapsed_ms);
    }

    if (isNew) {
      maybeAutoScroll();
    }

    maybeAutoDownload(source, key, isFinal);
  }

  function resetStats() {
    state.totalLatency = 0;
    state.latencyCount = 0;
    latencyValueEl.textContent = "-";
    updateActive();
  }

  function clearImages() {
    gridEl.innerHTML = "";
    state.imageMap.clear();
    state.downloadedKeys.clear();
    state.selectedKeys.clear();
    state.nextImageID = 1;
    state.imageCount = 0;
    updateCount();
    resetStats();
    updateEmptyState();
    updateSelectionUI();
  }

  function closeAllConnections() {
    for (const timer of state.fallbackTimers) {
      clearTimeout(timer);
    }
    state.fallbackTimers = [];

    for (const ws of state.wsList) {
      if (!ws) continue;
      try {
        if (ws.readyState === WebSocket.OPEN) {
          ws.send(JSON.stringify({ type: "stop" }));
        }
      } catch (err) {
        // ignore
      }
      try {
        ws.close(1000, "client stop");
      } catch (err) {
        // ignore
      }
    }
    state.wsList = [];

    for (const es of state.sseList) {
      if (!es) continue;
      try {
        es.close();
      } catch (err) {
        // ignore
      }
    }
    state.sseList = [];

    updateActive();
  }

  async function createTask(prompt, aspectRatio, nsfwEnabled) {
    const data = await window.PublicApp.requestJSON("/v1/public/imagine/start", {
      method: "POST",
      body: JSON.stringify({
        prompt,
        aspect_ratio: aspectRatio,
        nsfw: nsfwEnabled,
      }),
    });
    const taskID = String(data.task_id || "").trim();
    if (!taskID) {
      throw new Error("Missing task_id");
    }
    return taskID;
  }

  async function stopTasks(taskIDs) {
    if (!taskIDs || taskIDs.length === 0) return;
    try {
      await window.PublicApp.requestJSON("/v1/public/imagine/stop", {
        method: "POST",
        body: JSON.stringify({ task_ids: taskIDs }),
      });
    } catch (err) {
      // ignore remote stop errors during teardown
    }
  }

  function handleMessage(data) {
    if (!data || typeof data !== "object") return;

    if (data.type === "image") {
      upsertImage(data, true);
      return;
    }

    if (data.type === "image_generation.partial_image") {
      upsertImage(data, false);
      return;
    }

    if (data.type === "image_generation.completed") {
      upsertImage(data, true);
      return;
    }

    if (data.type === "status") {
      if (data.status === "running") {
        setStatus("Running", "ok");
      } else if (data.status === "stopped" && state.running) {
        setStatus("Stopped");
      }
      return;
    }

    if (data.type === "error" || data.error) {
      const message = String(data.message || data.error || "Imagine error");
      setStatus(message, "error");
    }
  }

  function startSSE(taskID) {
    const es = new EventSource(`/v1/public/imagine/sse?task_id=${encodeURIComponent(taskID)}&t=${Date.now()}`);

    es.onopen = () => {
      updateActive();
    };

    es.onmessage = (event) => {
      try {
        handleMessage(JSON.parse(event.data));
      } catch (err) {
        // ignore malformed event payload
      }
    };

    es.onerror = () => {
      try {
        es.close();
      } catch (err) {
        // ignore
      }
      updateActive();
      if (state.running && countOpenWS() + countOpenSSE() === 0) {
        setRunning(false);
        setStatus("Stream closed", "error");
      }
    };

    state.sseList.push(es);
    updateActive();
  }

  function startWS(taskID, prompt, aspectRatio, nsfwEnabled, mode) {
    const ws = new WebSocket(window.PublicApp.wsBaseURL("/v1/public/imagine/ws", { task_id: taskID, t: Date.now() }));

    let opened = false;
    let fallbackDone = false;

    const fallbackToSSE = () => {
      if (fallbackDone || mode !== "auto") return;
      fallbackDone = true;
      startSSE(taskID);
    };

    ws.onopen = () => {
      opened = true;
      updateActive();
      try {
        ws.send(
          JSON.stringify({
            type: "start",
            prompt,
            aspect_ratio: aspectRatio,
            nsfw: nsfwEnabled,
          })
        );
      } catch (err) {
        // ignore
      }
    };

    ws.onmessage = (event) => {
      try {
        handleMessage(JSON.parse(event.data));
      } catch (err) {
        // ignore malformed event payload
      }
    };

    ws.onerror = () => {
      if (!opened) {
        fallbackToSSE();
      }
      updateActive();
      if (mode === "ws" && state.running && countOpenWS() + countOpenSSE() === 0) {
        setRunning(false);
        setStatus("Connection closed", "error");
      }
    };

    ws.onclose = () => {
      if (!opened) {
        fallbackToSSE();
      }
      updateActive();
      if (mode === "ws" && state.running && countOpenWS() + countOpenSSE() === 0) {
        setRunning(false);
        setStatus("Connection closed", "error");
      }
    };

    if (mode === "auto") {
      const timer = window.setTimeout(() => {
        if (ws.readyState !== WebSocket.OPEN) {
          fallbackToSSE();
        }
      }, 1500);
      state.fallbackTimers.push(timer);
    }

    state.wsList.push(ws);
    updateActive();
  }

  function setModePreference(mode, persist) {
    if (!["auto", "ws", "sse"].includes(mode)) {
      return;
    }
    state.modePreference = mode;
    for (const btn of modeButtons) {
      btn.classList.toggle("active", btn.dataset.mode === mode);
    }
    if (persist) {
      try {
        localStorage.setItem(MODE_STORAGE_KEY, mode);
      } catch (err) {
        // ignore persistence failure
      }
    }
    updateModeValue();
  }

  function savePreferences() {
    const prefs = {
      auto_scroll: Boolean(autoScrollToggle.checked),
      auto_download: Boolean(autoDownloadToggle.checked),
      auto_filter: Boolean(autoFilterToggle.checked),
      reverse_insert: Boolean(reverseInsertToggle.checked),
    };
    try {
      localStorage.setItem(PREF_STORAGE_KEY, JSON.stringify(prefs));
    } catch (err) {
      // ignore persistence failure
    }
  }

  function loadPreferences() {
    try {
      const raw = localStorage.getItem(PREF_STORAGE_KEY);
      if (!raw) return;
      const prefs = JSON.parse(raw);
      if (typeof prefs.auto_scroll === "boolean") autoScrollToggle.checked = prefs.auto_scroll;
      if (typeof prefs.auto_download === "boolean") autoDownloadToggle.checked = prefs.auto_download;
      if (typeof prefs.auto_filter === "boolean") autoFilterToggle.checked = prefs.auto_filter;
      if (typeof prefs.reverse_insert === "boolean") reverseInsertToggle.checked = prefs.reverse_insert;
    } catch (err) {
      // ignore invalid preference payload
    }
  }

  async function loadConfigDefaults() {
    try {
      const data = await window.PublicApp.requestJSON("/v1/public/imagine/config", { method: "GET" });
      const minBytes = Number(data.final_min_bytes);
      if (Number.isFinite(minBytes) && minBytes >= 0) {
        state.finalMinBytes = minBytes;
      }
      if (typeof data.nsfw === "boolean") {
        nsfwEl.value = data.nsfw ? "true" : "false";
      }
    } catch (err) {
      // keep defaults when config endpoint is unavailable
    }
  }

  async function start() {
    if (state.running) return;

    const prompt = String(promptEl.value || "").trim();
    if (!prompt) {
      setStatus("Prompt cannot be empty", "error");
      return;
    }

    const aspectRatio = String(ratioEl.value || "2:3");
    const nsfwEnabled = String(nsfwEl.value || "true") === "true";
    const concurrent = Math.max(1, Math.min(3, Number(concurrentEl.value || 1)));

    closeAllConnections();
    setRunning(true);
    setStatus("Starting...");
    state.taskIDs = [];

    try {
      for (let i = 0; i < concurrent; i += 1) {
        const taskID = await createTask(prompt, aspectRatio, nsfwEnabled);
        state.taskIDs.push(taskID);
      }

      if (state.modePreference === "sse") {
        for (const taskID of state.taskIDs) {
          startSSE(taskID);
        }
      } else {
        for (const taskID of state.taskIDs) {
          startWS(taskID, prompt, aspectRatio, nsfwEnabled, state.modePreference);
        }
      }

      setStatus(`Running (${state.modePreference.toUpperCase()})`, "ok");
    } catch (err) {
      await stopTasks(state.taskIDs);
      closeAllConnections();
      state.taskIDs = [];
      setRunning(false);
      setStatus(err.message || "Start failed", "error");
    }
  }

  async function stop() {
    if (!state.running) return;

    const taskIDs = state.taskIDs.slice();
    closeAllConnections();
    state.taskIDs = [];
    setRunning(false);

    setStatus("Stopping...");
    await stopTasks(taskIDs);
    setStatus("Stopped");
  }

  async function restartActiveConnection(reason) {
    if (!state.running || state.restarting) return;
    state.restarting = true;
    try {
      await stop();
      await new Promise((resolve) => window.setTimeout(resolve, 60));
      await start();
      if (reason) {
        setStatus(`Running (${state.modePreference.toUpperCase()}) - ${reason}`, "ok");
      }
    } finally {
      state.restarting = false;
    }
  }

  function getLightboxImages() {
    return Array.from(gridEl.querySelectorAll(".image-card img"));
  }

  function updateLightbox(index) {
    const images = getLightboxImages();
    if (images.length === 0) return;

    const max = images.length - 1;
    const safeIndex = Math.max(0, Math.min(max, index));
    state.lightboxIndex = safeIndex;

    lightboxImgEl.src = images[safeIndex].src;
    lightboxPrevEl.disabled = safeIndex <= 0;
    lightboxNextEl.disabled = safeIndex >= max;
  }

  function openLightboxByCard(card) {
    const images = getLightboxImages();
    if (images.length === 0) return;

    const target = card.querySelector("img");
    const index = images.indexOf(target);
    if (index < 0) return;

    lightboxEl.classList.add("active");
    lightboxEl.setAttribute("aria-hidden", "false");
    updateLightbox(index);
  }

  function closeLightbox() {
    lightboxEl.classList.remove("active");
    lightboxEl.setAttribute("aria-hidden", "true");
    state.lightboxIndex = -1;
    lightboxImgEl.removeAttribute("src");
  }

  function initFolderPicker() {
    if (!("showDirectoryPicker" in window)) {
      selectFolderBtn.disabled = true;
      folderPathEl.textContent = "Folder API is not supported by this browser";
      return;
    }

    selectFolderBtn.disabled = false;
    selectFolderBtn.addEventListener("click", async () => {
      try {
        const handle = await window.showDirectoryPicker({ mode: "readwrite" });
        state.directoryHandle = handle;
        state.useFileSystemAPI = true;
        folderPathEl.textContent = handle && handle.name ? `Selected: ${handle.name}` : "Selected folder";
        setStatus("Save folder selected", "ok");
      } catch (err) {
        if (err && err.name === "AbortError") {
          return;
        }
        state.directoryHandle = null;
        state.useFileSystemAPI = false;
        folderPathEl.textContent = "Browser default download path";
        setStatus("Failed to select folder", "error");
      }
    });
  }

  function bindEvents() {
    startBtn.addEventListener("click", () => {
      start().catch((err) => {
        setStatus(err.message || "Start failed", "error");
      });
    });

    stopBtn.addEventListener("click", () => {
      stop().catch((err) => {
        setStatus(err.message || "Stop failed", "error");
      });
    });

    clearBtn.addEventListener("click", () => {
      clearImages();
      setSelectionMode(false);
    });

    for (const toggle of [autoScrollToggle, autoDownloadToggle, autoFilterToggle, reverseInsertToggle]) {
      toggle.addEventListener("change", savePreferences);
    }

    for (const btn of modeButtons) {
      btn.addEventListener("click", () => {
        const mode = String(btn.dataset.mode || "").toLowerCase();
        if (!mode) return;
        setModePreference(mode, true);
        restartActiveConnection("mode switched").catch((err) => {
          setStatus(err.message || "Restart failed", "error");
        });
      });
    }

    const restartOnChange = () => {
      restartActiveConnection("config updated").catch((err) => {
        setStatus(err.message || "Restart failed", "error");
      });
    };

    ratioEl.addEventListener("change", restartOnChange);
    nsfwEl.addEventListener("change", restartOnChange);
    concurrentEl.addEventListener("change", restartOnChange);

    batchToggleBtn.addEventListener("click", () => {
      setSelectionMode(!state.selectionMode);
    });

    selectAllBtn.addEventListener("click", () => {
      selectAllOrClearAll();
    });

    downloadSelectedBtn.addEventListener("click", () => {
      downloadSelectedImages().catch((err) => {
        setStatus(err.message || "Batch download failed", "error");
      });
    });

    promptEl.addEventListener("keydown", (event) => {
      if ((event.ctrlKey || event.metaKey) && event.key === "Enter") {
        event.preventDefault();
        start().catch((err) => {
          setStatus(err.message || "Start failed", "error");
        });
      }
    });

    lightboxCloseEl.addEventListener("click", (event) => {
      event.preventDefault();
      closeLightbox();
    });

    lightboxPrevEl.addEventListener("click", (event) => {
      event.preventDefault();
      updateLightbox(state.lightboxIndex - 1);
    });

    lightboxNextEl.addEventListener("click", (event) => {
      event.preventDefault();
      updateLightbox(state.lightboxIndex + 1);
    });

    lightboxEl.addEventListener("click", (event) => {
      if (event.target === lightboxEl) {
        closeLightbox();
      }
    });

    document.addEventListener("keydown", (event) => {
      if (!lightboxEl.classList.contains("active")) return;
      if (event.key === "Escape") {
        closeLightbox();
      } else if (event.key === "ArrowLeft") {
        updateLightbox(state.lightboxIndex - 1);
      } else if (event.key === "ArrowRight") {
        updateLightbox(state.lightboxIndex + 1);
      }
    });

    window.addEventListener("beforeunload", () => {
      closeAllConnections();
    });
  }

  function init() {
    setRunning(false);
    updateCount();
    updateLatency();
    updateActive();
    loadPreferences();

    const savedMode = (() => {
      try {
        return localStorage.getItem(MODE_STORAGE_KEY) || "auto";
      } catch (err) {
        return "auto";
      }
    })();
    setModePreference(savedMode, false);

    loadConfigDefaults();
    updateEmptyState();
    setSelectionMode(false);
    initFolderPicker();
    bindEvents();
  }

  init();
})();
