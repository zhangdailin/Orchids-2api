(() => {
  const BATCH_SIZE = 2;
  const PARALLELISM = 1;
  const STORAGE_KEY = "grok_tools_ui_v1";
  const QUALITY_MODELS = {
    basic: "",
    lite: "grok-imagine-image-lite",
    quality: "grok-imagine-image-pro",
  };
  const QUALITY_ROUTES = {
    basic: "app_chat",
    lite: "ws",
    quality: "ws",
  };

  const state = {
    initialized: false,
    ready: false,
    running: false,
    abortController: null,
    selectionMode: false,
    selected: new Set(),
    imageCount: 0,
    finalMinBytes: 100000,
    saveState: null,
    showToast: null,
  };

  function $(id) {
    return document.getElementById(id);
  }

  function toast(message, type = "info") {
    if (typeof state.showToast === "function") state.showToast(message, type);
    else if (typeof window.showToast === "function") window.showToast(message, type);
  }

  function loadState() {
    try {
      const parsed = JSON.parse(localStorage.getItem(STORAGE_KEY) || "{}");
      return parsed && typeof parsed === "object" ? parsed : {};
    } catch (err) {
      return {};
    }
  }

  function saveState(patch) {
    if (typeof state.saveState === "function") {
      state.saveState(patch);
      return;
    }
    try {
      const current = loadState();
      localStorage.setItem(STORAGE_KEY, JSON.stringify({ ...current, ...patch }));
    } catch (err) {
      // ignore storage failures
    }
  }

  function readToggle(selector, attr, fallback) {
    const active = document.querySelector(`${selector} .is-active`);
    return String(active?.dataset?.[attr] || fallback || "").trim();
  }

  function setToggle(selector, attr, value) {
    const dataAttr = attr.replace(/[A-Z]/g, (m) => `-${m.toLowerCase()}`);
    document.querySelectorAll(`${selector} [data-${dataAttr}]`).forEach((btn) => {
      const active = String(btn.dataset[attr] || "") === String(value || "");
      btn.classList.toggle("is-active", active);
      btn.setAttribute("aria-pressed", active ? "true" : "false");
    });
  }

  function qualityModel(quality) {
    return QUALITY_MODELS[quality] || QUALITY_MODELS.lite;
  }

  function normalizeQuality(value) {
    const raw = String(value || "").toLowerCase();
    if (raw === "basic" || raw === "quality") return raw;
    return "lite";
  }

  function qualityRoute(quality) {
    return QUALITY_ROUTES[normalizeQuality(quality)] || "ws";
  }

  function syncQualityModel() {
    const quality = normalizeQuality(readToggle("#imagineQualityToggle", "imagineQuality", "lite"));
    const model = qualityModel(quality);
    const modelSelect = $("imagineModel");
    if (modelSelect) modelSelect.value = model;
    return { quality, model, route: qualityRoute(quality) };
  }

  function aspectRatioCss(value) {
    const ratio = String(value || "1:1").trim();
    if (!ratio.includes(":")) return "1 / 1";
    const [w, h] = ratio.split(":");
    return `${Math.max(1, Number(w) || 1)} / ${Math.max(1, Number(h) || 1)}`;
  }

  function sizeForRatio(value) {
    switch (String(value || "").trim()) {
      case "16:9":
      case "3:2":
        return "1792x1024";
      case "9:16":
      case "2:3":
        return "1024x1792";
      default:
        return "1024x1024";
    }
  }

  function inferMime(raw) {
    const value = String(raw || "");
    if (value.startsWith("iVBOR")) return "image/png";
    if (value.startsWith("R0lGOD")) return "image/gif";
    if (value.startsWith("UklGR")) return "image/webp";
    return "image/jpeg";
  }

  function imageSrc(raw) {
    const value = String(raw || "");
    if (!value) return "";
    if (/^(https?:|data:|\/)/i.test(value)) return value;
    return `data:${inferMime(value)};base64,${value}`;
  }

  function authHeaders() {
    const token = localStorage.getItem("admin_token") || sessionStorage.getItem("admin_token") || "";
    return token ? { "X-Admin-Token": token } : {};
  }

  function resizePrompt() {
    const input = $("imaginePrompt");
    if (!input) return;
    input.style.height = "52px";
    input.style.height = `${Math.min(Math.max(input.scrollHeight, 52), 160)}px`;
    input.style.overflowY = input.scrollHeight > 160 ? "auto" : "hidden";
  }

  function syncRatioUI() {
    const wrap = $("imagineRatioWrap");
    if (wrap) wrap.dataset.ratio = String($("imagineRatio")?.value || "2:3");
  }

  function setStatus(text) {
    const el = $("imagineStatus");
    if (el) el.textContent = text;
  }

  function setButtons(running) {
    const start = $("imagineStartBtn");
    const stop = $("imagineStopBtn");
    if (start) {
      start.disabled = false;
      start.title = running ? "停止" : "生成";
      start.setAttribute("aria-label", running ? "停止" : "生成");
      start.classList.toggle("is-running", running);
      start.innerHTML = running
        ? '<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M10 7v10"></path><path d="M14 7v10"></path></svg>'
        : '<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M12 5v14"></path><path d="m6 11 6-6 6 6"></path></svg>';
    }
    if (stop) stop.classList.toggle("hidden", !running);
    document.querySelectorAll("#imagineRunModeToggle button, #imagineQualityToggle button").forEach((btn) => {
      btn.disabled = running;
    });
    ["imaginePrompt", "imagineRatio", "imagineNSFW"].forEach((id) => {
      const el = $(id);
      if (el) el.disabled = running;
    });
  }

  function setEmptyState() {
    const grid = $("imagineGrid");
    const empty = $("imagineEmpty");
    if (!grid || !empty) return;
    const hasBatch = grid.querySelector(".imagine-masonry-batch") !== null;
    empty.hidden = hasBatch;
    empty.style.display = hasBatch ? "none" : "";
  }

  function createBatch(prompt, ratio, quality, round) {
    const grid = $("imagineGrid");
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
    [
      ["is-round", `第 ${round} 轮`],
      ["is-param", ratio],
      ["is-param", quality === "basic" ? "Basic" : quality === "quality" ? "Quality" : "Lite"],
      ["is-count", `0/${BATCH_SIZE}`],
      ["is-state", "正在生成"],
    ].forEach(([cls, text]) => {
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
    slotGrid.style.setProperty("--tile-aspect", aspectRatioCss(ratio));

    const slots = Array.from({ length: BATCH_SIZE }, (_, index) => {
      const tile = document.createElement("article");
      tile.className = "imagine-masonry-tile waterfall-item is-pending";
      tile.dataset.prompt = prompt;
      const checkbox = document.createElement("div");
      checkbox.className = "image-checkbox";
      const badge = document.createElement("div");
      badge.className = "imagine-masonry-tile-badge";
      badge.textContent = String(index + 1);
      const body = document.createElement("div");
      body.className = "imagine-masonry-tile-body";
      body.innerHTML = '<div class="imagine-masonry-tile-progress"><div class="imagine-masonry-tile-progress-value">0%</div><div class="imagine-masonry-tile-progress-track"><div class="imagine-masonry-tile-progress-fill"></div></div></div>';
      tile.appendChild(checkbox);
      tile.appendChild(badge);
      tile.appendChild(body);
      slotGrid.appendChild(tile);
      return { tile, body, status: "pending", progress: 0, url: "" };
    });

    batch.appendChild(head);
    batch.appendChild(slotGrid);
    grid.prepend(batch);
    setEmptyState();
    return {
      countEl: meta.querySelector(".is-count"),
      stateEl: meta.querySelector(".is-state"),
      slots,
      ready: 0,
      failed: 0,
    };
  }

  function updateBatch(batch, final) {
    if (!batch) return;
    if (batch.countEl) batch.countEl.textContent = `${batch.ready}/${BATCH_SIZE}`;
    if (!batch.stateEl) return;
    if (!final) return;
    if (batch.ready >= BATCH_SIZE) {
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

  function setSlotProgress(slot, value) {
    if (!slot || slot.status !== "pending") return;
    const progress = Math.max(0, Math.min(99, Number(value) || 0));
    slot.progress = progress;
    const text = slot.tile.querySelector(".imagine-masonry-tile-progress-value");
    const fill = slot.tile.querySelector(".imagine-masonry-tile-progress-fill");
    if (text) text.textContent = `${progress}%`;
    if (fill) fill.style.width = `${progress}%`;
  }

  function failSlot(slot, label) {
    if (!slot || slot.status !== "pending") return;
    slot.status = "failed";
    slot.tile.classList.remove("is-pending", "is-ready");
    slot.tile.classList.add("is-filtered");
    const message = document.createElement("div");
    message.className = "imagine-masonry-tile-label";
    message.textContent = label || "失败";
    slot.body.replaceChildren(message);
  }

  function finishSlot(slot, raw, meta) {
    const src = imageSrc(raw);
    if (!slot || slot.status !== "pending" || !src) {
      failSlot(slot, "失败");
      return;
    }
    slot.status = "ready";
    slot.url = src;
    slot.tile.classList.remove("is-pending", "is-filtered");
    slot.tile.classList.add("is-ready");
    slot.tile.dataset.imageUrl = src;
    slot.tile.dataset.prompt = String(meta?.prompt || "");
    if (state.selectionMode) slot.tile.classList.add("selection-mode");

    const link = document.createElement("div");
    link.className = "imagine-masonry-tile-link";
    const img = document.createElement("img");
    img.loading = "lazy";
    img.decoding = "async";
    img.alt = "image";
    img.src = src;
    link.appendChild(img);
    slot.body.replaceChildren(link);
    state.imageCount += 1;
    const count = $("imagineCount");
    if (count) count.textContent = String(state.imageCount);
  }

  function extractImageValue(data) {
    const item = Array.isArray(data?.data) ? data.data[0] : null;
    return String(item?.url || item?.b64_json || item?.base64 || item?.b64 || "");
  }

  async function createTask(prompt, ratio, model, route, nsfw, signal) {
    const body = {
      prompt,
      aspect_ratio: ratio,
      route,
      nsfw,
    };
    if (String(model || "").trim()) body.model = model;
    const res = await fetch("/api/v1/admin/imagine/start", {
      method: "POST",
      headers: { "Content-Type": "application/json", ...authHeaders() },
      signal,
      body: JSON.stringify(body),
    });
    if (res.status === 401) {
      window.location.href = "/admin/login.html?next=" + encodeURIComponent("/admin/?tab=grok-tools");
      throw new Error("unauthorized");
    }
    if (!res.ok) throw new Error(await res.text());
    const data = await res.json();
    const taskID = String(data?.task_id || "").trim();
    if (!taskID) throw new Error("imagine task not created");
    return taskID;
  }

  async function stopTasks(taskIDs) {
    const ids = Array.isArray(taskIDs) ? taskIDs.filter(Boolean) : [];
    if (!ids.length) return;
    try {
      await fetch("/api/v1/admin/imagine/stop", {
        method: "POST",
        headers: { "Content-Type": "application/json", ...authHeaders() },
        body: JSON.stringify({ task_ids: ids }),
      });
    } catch (err) {
      // Best-effort cleanup only.
    }
  }

  function imageFromImagineEvent(payload) {
    if (!payload || typeof payload !== "object") return "";
    if (payload.type === "image") {
      return String(payload.file_url || payload.url || payload.b64_json || "");
    }
    if (payload.type === "image_generation.completed") {
      return String(payload.url || payload.image || payload.b64_json || "");
    }
    return "";
  }

  function isRetryableImagineError(message) {
    const lower = String(message || "").toLowerCase();
    return lower.includes("rate_limit_exceeded") ||
      lower.includes("rate limit") ||
      lower.includes("cooling down") ||
      lower.includes("429") ||
      lower.includes("no image generated");
  }

  async function requestImage(prompt, ratio, model, route, nsfw, signal) {
    const taskID = await createTask(prompt, ratio, model, route, nsfw, signal);
    return await new Promise((resolve, reject) => {
      let settled = false;
      let lastError = "";
      let cleanup = () => {
        try {
          es.close();
        } catch (err) {
          // ignore
        }
        stopTasks([taskID]);
      };
      const finish = (fn, value) => {
        if (settled) return;
        settled = true;
        cleanup();
        fn(value);
      };
      const url = `/api/v1/admin/imagine/sse?task_id=${encodeURIComponent(taskID)}&t=${Date.now()}`;
      const es = new EventSource(url);
      const abort = () => finish(reject, new DOMException("aborted", "AbortError"));
      if (signal) {
        if (signal.aborted) {
          abort();
          return;
        }
        signal.addEventListener("abort", abort, { once: true });
      }
      const timeout = window.setTimeout(() => {
        finish(reject, new Error(lastError || "image generation timed out"));
      }, 150000);
      const originalCleanup = cleanup;
      cleanup = () => {
        window.clearTimeout(timeout);
        if (signal) signal.removeEventListener("abort", abort);
        originalCleanup();
      };
      es.onmessage = (event) => {
        try {
          const payload = JSON.parse(event.data);
          const image = imageFromImagineEvent(payload);
          if (image) {
            finish(resolve, image);
            return;
          }
          if (payload?.type === "error" || payload?.error) {
            const message = String(payload.message || payload.error?.message || payload.error || "");
            lastError = message || lastError;
            if (!isRetryableImagineError(message)) {
              finish(reject, new Error(message || "image generation failed"));
            }
          }
        } catch (err) {
          // ignore malformed SSE frames
        }
      };
      es.onerror = () => {
        if (settled) return;
        lastError = lastError || "image stream disconnected";
      };
    });
  }

  async function runSlot(batch, slot, prompt, ratio, model, route, nsfw, signal) {
    let tick = 0;
    const timer = window.setInterval(() => {
      tick += 1;
      setSlotProgress(slot, Math.min(92, 8 + tick * 7));
    }, 900);
    try {
      const image = await requestImage(prompt, ratio, model, route, nsfw, signal);
      finishSlot(slot, image, { prompt });
      batch.ready += 1;
      updateBatch(batch, false);
      return true;
    } catch (err) {
      failSlot(slot, signal?.aborted ? "已停止" : "失败");
      batch.failed += 1;
      updateBatch(batch, false);
      return false;
    } finally {
      window.clearInterval(timer);
    }
  }

  async function runBatch(batch, prompt, ratio, model, route, nsfw, signal) {
    const results = new Array(batch.slots.length).fill(false);
    let cursor = 0;
    const workers = Array.from({ length: Math.min(PARALLELISM, batch.slots.length) }, async () => {
      while (cursor < batch.slots.length) {
        if (signal?.aborted) break;
        const index = cursor;
        cursor += 1;
        results[index] = await runSlot(batch, batch.slots[index], prompt, ratio, model, route, nsfw, signal);
      }
    });
    await Promise.all(workers);
    return results;
  }

  async function start() {
    if (state.running) {
      await stop();
      return;
    }
    const promptInput = $("imaginePrompt");
    const prompt = String(promptInput?.value || "").trim();
    if (!prompt) {
      toast("请输入 Prompt", "error");
      return;
    }
    const ratio = String($("imagineRatio")?.value || "2:3");
    const runMode = readToggle("#imagineRunModeToggle", "imagineRunMode", "single") === "continuous" ? "continuous" : "single";
    const { quality, model, route } = syncQualityModel();
    const nsfw = String($("imagineNSFW")?.value || "true") === "true";

    saveState({
      imagineRatio: ratio,
      imagineModel: model,
      imagineRoute: route,
      imagineRunMode: runMode,
      imagineQuality: quality,
      imagineConcurrent: BATCH_SIZE,
    });

    state.running = true;
    state.abortController = new AbortController();
    setButtons(true);
    setStatus("生成中");

    let round = 0;
    try {
      while (state.running) {
        round += 1;
        const batch = createBatch(prompt, ratio, quality, round);
        if (!batch) throw new Error("瀑布流容器不存在");
        setStatus(`生成中 · 第 ${round} 轮`);
        await runBatch(batch, prompt, ratio, model, route, nsfw, state.abortController.signal);
        updateBatch(batch, true);
        if (!state.running || runMode !== "continuous") break;
      }
      if (state.running) setStatus("完成");
    } catch (err) {
      if (!state.abortController?.signal?.aborted) {
        setStatus("错误");
        toast(`启动失败: ${err.message || err}`, "error");
      }
    } finally {
      state.running = false;
      state.abortController = null;
      setButtons(false);
      if (document.activeElement !== promptInput) promptInput?.focus();
    }
  }

  async function stop() {
    state.running = false;
    if (state.abortController) {
      state.abortController.abort();
      state.abortController = null;
    }
    setButtons(false);
    setStatus("已停止");
  }

  function clearGrid() {
    const grid = $("imagineGrid");
    const empty = $("imagineEmpty");
    if (grid) grid.innerHTML = "";
    if (empty && grid) {
      empty.hidden = false;
      empty.style.display = "";
      grid.appendChild(empty);
    }
    state.selected.clear();
    state.selectionMode = false;
    state.imageCount = 0;
    const toolbar = $("selectionToolbar");
    if (toolbar) toolbar.classList.add("hidden");
    const count = $("imagineCount");
    if (count) count.textContent = "0";
    setStatus("未连接");
  }

  function dataUrlToBlob(dataUrl) {
    const parts = String(dataUrl || "").split(",");
    if (parts.length < 2) return null;
    const match = parts[0].match(/data:(.*?);base64/);
    const mime = match ? match[1] : "application/octet-stream";
    try {
      const bytes = atob(parts.slice(1).join(","));
      const arr = new Uint8Array(bytes.length);
      for (let i = 0; i < bytes.length; i += 1) arr[i] = bytes.charCodeAt(i);
      return new Blob([arr], { type: mime });
    } catch (err) {
      return null;
    }
  }

  function updateSelectedCount() {
    const count = $("selectedCount");
    if (count) count.textContent = String(state.selected.size);
    const btn = $("downloadSelectedBtn");
    if (btn) btn.disabled = state.selected.size === 0;
    const toggle = $("toggleSelectAllBtn");
    if (toggle) {
      const items = document.querySelectorAll("#imagineGrid .waterfall-item.is-ready");
      toggle.textContent = items.length > 0 && state.selected.size === items.length ? "取消全选" : "全选";
    }
  }

  function enterSelection() {
    state.selectionMode = true;
    state.selected.clear();
    const toolbar = $("selectionToolbar");
    if (toolbar) toolbar.classList.remove("hidden");
    const items = document.querySelectorAll("#imagineGrid .waterfall-item.is-ready");
    if (items.length === 0) {
      state.selectionMode = false;
      if (toolbar) toolbar.classList.add("hidden");
      toast("暂无可下载图片", "info");
      updateSelectedCount();
      return;
    }
    items.forEach((item) => item.classList.add("selection-mode"));
    updateSelectedCount();
  }

  function exitSelection() {
    state.selectionMode = false;
    state.selected.clear();
    const toolbar = $("selectionToolbar");
    if (toolbar) toolbar.classList.add("hidden");
    document.querySelectorAll("#imagineGrid .waterfall-item").forEach((item) => {
      item.classList.remove("selection-mode", "selected");
    });
    updateSelectedCount();
  }

  function toggleSelectionMode() {
    if (state.selectionMode) exitSelection();
    else enterSelection();
  }

  function toggleItem(item) {
    if (!state.selectionMode || !item) return;
    if (item.classList.contains("selected")) {
      item.classList.remove("selected");
      state.selected.delete(item);
    } else {
      item.classList.add("selected");
      state.selected.add(item);
    }
    updateSelectedCount();
  }

  function toggleSelectAll() {
    const items = Array.from(document.querySelectorAll("#imagineGrid .waterfall-item.is-ready"));
    const allSelected = items.length > 0 && state.selected.size === items.length;
    state.selected.clear();
    items.forEach((item) => {
      item.classList.toggle("selected", !allSelected);
      if (!allSelected) state.selected.add(item);
    });
    updateSelectedCount();
  }

  async function downloadSelected() {
    if (state.selected.size === 0) {
      toggleSelectAll();
      if (state.selected.size === 0) {
        toast("未选择图片", "info");
        return;
      }
    }
    if (typeof JSZip === "undefined") {
      toast("JSZip 未加载", "error");
      return;
    }
    const btn = $("downloadSelectedBtn");
    if (btn) {
      btn.disabled = true;
      btn.textContent = "打包中...";
    }
    const zip = new JSZip();
    const folder = zip.folder("images");
    let processed = 0;
    try {
      for (const item of state.selected) {
        const url = item.dataset.imageUrl || "";
        let blob = null;
        if (url.startsWith("data:")) blob = dataUrlToBlob(url);
        else if (url) {
          const response = await fetch(url);
          blob = await response.blob();
        }
        if (!blob) continue;
        processed += 1;
        folder.file(`image_${processed}.png`, blob);
        if (btn) btn.textContent = `打包中 ${processed}/${state.selected.size}`;
      }
      if (processed === 0) {
        toast("没有可下载的图片", "error");
        return;
      }
      const content = await zip.generateAsync({ type: "blob" });
      const link = document.createElement("a");
      link.href = URL.createObjectURL(content);
      link.download = `imagine_${new Date().toISOString().slice(0, 10)}_${Date.now()}.zip`;
      link.click();
      URL.revokeObjectURL(link.href);
      toast(`已打包 ${processed} 张图片`, "success");
      exitSelection();
    } catch (err) {
      toast("打包失败", "error");
    } finally {
      if (btn) {
        btn.disabled = state.selected.size === 0;
        btn.innerHTML = `下载 <span id="selectedCount" class="selected-count">${state.selected.size}</span>`;
      }
    }
  }

  async function ensureReady() {
    if (state.ready) return;
    try {
      const res = await fetch("/v1/public/imagine/config", { cache: "no-store" });
      if (res.ok) {
        const data = await res.json();
        const value = parseInt(data?.final_min_bytes, 10);
        if (Number.isFinite(value) && value >= 0) state.finalMinBytes = value;
        const nsfw = $("imagineNSFW");
        const ui = loadState();
        if (nsfw && typeof data?.nsfw === "boolean" && !ui.imagineNSFW) {
          nsfw.value = data.nsfw ? "true" : "false";
        }
      }
    } catch (err) {
      // config is optional for the page
    }
    state.ready = true;
  }

  function bindEvents() {
    document.querySelectorAll("[data-imagine-run-mode]").forEach((btn) => {
      btn.addEventListener("click", () => {
        const value = String(btn.dataset.imagineRunMode || "single");
        setToggle("#imagineRunModeToggle", "imagineRunMode", value);
        saveState({ imagineRunMode: value });
      });
    });
    document.querySelectorAll("[data-imagine-quality]").forEach((btn) => {
      btn.addEventListener("click", () => {
        const value = normalizeQuality(btn.dataset.imagineQuality);
        setToggle("#imagineQualityToggle", "imagineQuality", value);
        const model = qualityModel(value);
        const modelSelect = $("imagineModel");
        if (modelSelect) modelSelect.value = model;
        saveState({ imagineQuality: value, imagineModel: model, imagineRoute: qualityRoute(value) });
      });
    });
    const prompt = $("imaginePrompt");
    if (prompt) {
      prompt.addEventListener("keydown", async (event) => {
        if (event.key === "Enter" && !event.shiftKey) {
          event.preventDefault();
          await start();
        }
      });
      prompt.addEventListener("input", resizePrompt);
    }
    const ratio = $("imagineRatio");
    if (ratio) {
      ratio.addEventListener("change", () => {
        syncRatioUI();
        saveState({ imagineRatio: String(ratio.value || "") });
      });
    }
    const nsfw = $("imagineNSFW");
    if (nsfw) nsfw.addEventListener("change", () => saveState({ imagineNSFW: String(nsfw.value || "") }));
    $("imagineStartBtn")?.addEventListener("click", () => start());
    $("imagineStopBtn")?.addEventListener("click", () => stop());
    $("imagineClearBtn")?.addEventListener("click", () => clearGrid());
    $("batchDownloadBtn")?.addEventListener("click", toggleSelectionMode);
    $("toggleSelectAllBtn")?.addEventListener("click", toggleSelectAll);
    $("downloadSelectedBtn")?.addEventListener("click", downloadSelected);
    $("exitSelectionBtn")?.addEventListener("click", exitSelection);
    $("imagineGrid")?.addEventListener("click", (event) => {
      const item = event.target.closest(".waterfall-item");
      if (!item || !item.classList.contains("is-ready")) return;
      toggleItem(item);
    });
  }

  function init(options = {}) {
    if (state.initialized) return;
    state.initialized = true;
    state.saveState = options.saveState || null;
    state.showToast = options.showToast || null;
    const ui = options.uiState || loadState();
    if ($("imagineRatio") && typeof ui.imagineRatio === "string" && ui.imagineRatio) $("imagineRatio").value = ui.imagineRatio;
    const quality = normalizeQuality(ui.imagineQuality || (ui.imagineRoute === "app_chat" ? "basic" : ui.imagineModel === QUALITY_MODELS.quality ? "quality" : "lite"));
    setToggle("#imagineQualityToggle", "imagineQuality", quality);
    setToggle("#imagineRunModeToggle", "imagineRunMode", ui.imagineRunMode === "continuous" ? "continuous" : "single");
    const modelSelect = $("imagineModel");
    if (modelSelect) modelSelect.value = qualityModel(quality);
    bindEvents();
    syncRatioUI();
    resizePrompt();
    setButtons(false);
    setStatus("未连接");
  }

  window.GrokImagine = {
    init,
    ensureReady,
    start,
    stop,
  };
})();
