(function () {
  const promptEl = document.getElementById("prompt");
  const ratioEl = document.getElementById("ratio");
  const lengthEl = document.getElementById("length");
  const resolutionEl = document.getElementById("resolution");
  const presetEl = document.getElementById("preset");
  const imageURLEl = document.getElementById("imageUrl");
  const effortEl = document.getElementById("effort");

  const imageFileInputEl = document.getElementById("imageFileInput");
  const imageFileNameEl = document.getElementById("imageFileName");
  const selectImageFileBtn = document.getElementById("selectImageFileBtn");
  const clearImageFileBtn = document.getElementById("clearImageFileBtn");

  const startBtn = document.getElementById("startBtn");
  const stopBtn = document.getElementById("stopBtn");
  const clearBtn = document.getElementById("clearBtn");

  const statusEl = document.getElementById("status");
  const progressBarEl = document.getElementById("progressBar");
  const progressFillEl = document.getElementById("progressFill");
  const progressTextEl = document.getElementById("progressText");
  const durationValueEl = document.getElementById("durationValue");

  const aspectValueEl = document.getElementById("aspectValue");
  const lengthValueEl = document.getElementById("lengthValue");
  const resolutionValueEl = document.getElementById("resolutionValue");
  const presetValueEl = document.getElementById("presetValue");

  const videoEmptyEl = document.getElementById("videoEmpty");
  const videoStageEl = document.getElementById("videoStage");
  const logEl = document.getElementById("logOutput");

  const state = {
    taskID: "",
    stream: null,
    running: false,
    fileDataURL: "",
    startAt: 0,
    elapsedTimer: null,
    progressBuffer: "",
    contentBuffer: "",
    previewCount: 0,
    currentPreviewItem: null,
  };

  function setStatus(message, level) {
    window.PublicApp.setStatus(statusEl, message, level);
  }

  function setRunning(running) {
    state.running = running;
    startBtn.disabled = running;
    stopBtn.disabled = !running;
  }

  function updateMeta() {
    aspectValueEl.textContent = String(ratioEl.value || "-");
    lengthValueEl.textContent = `${String(lengthEl.value || "-")}s`;
    resolutionValueEl.textContent = String(resolutionEl.value || "-");
    presetValueEl.textContent = String(presetEl.value || "-");
  }

  function setProgress(value) {
    const safe = Math.max(0, Math.min(100, Number(value) || 0));
    progressFillEl.style.width = `${safe}%`;
    progressTextEl.textContent = `${safe}%`;
  }

  function setIndeterminate(active) {
    progressBarEl.classList.toggle("indeterminate", Boolean(active));
  }

  function appendLog(line) {
    logEl.textContent += `${line}\n`;
    logEl.scrollTop = logEl.scrollHeight;
  }

  function clearLog() {
    logEl.textContent = "";
  }

  function closeStream() {
    if (!state.stream) return;
    try {
      state.stream.close();
    } catch (err) {
      // ignore
    }
    state.stream = null;
  }

  function stopElapsedTimer() {
    if (!state.elapsedTimer) return;
    clearInterval(state.elapsedTimer);
    state.elapsedTimer = null;
  }

  function startElapsedTimer() {
    stopElapsedTimer();
    state.elapsedTimer = window.setInterval(() => {
      if (!state.startAt) return;
      const seconds = Math.max(0, Math.round((Date.now() - state.startAt) / 1000));
      durationValueEl.textContent = `Elapsed ${seconds}s`;
    }, 1000);
  }

  function resetOutput(clearPreview) {
    state.progressBuffer = "";
    state.contentBuffer = "";
    setProgress(0);
    setIndeterminate(false);
    durationValueEl.textContent = "Elapsed -";
    state.currentPreviewItem = null;
    if (clearPreview) {
      videoStageEl.innerHTML = "";
      state.previewCount = 0;
      videoEmptyEl.style.display = "block";
    }
  }

  function clearFileSelection() {
    state.fileDataURL = "";
    imageFileInputEl.value = "";
    imageFileNameEl.textContent = "No file selected";
  }

  function buildVideoURL(raw) {
    const value = String(raw || "").trim();
    if (!value) return "";
    if (/^https?:\/\//i.test(value)) return value;
    if (value.startsWith("//")) {
      return `${window.location.protocol}${value}`;
    }
    return `${window.location.origin}${value.startsWith("/") ? "" : "/"}${value}`;
  }

  function extractVideoURL(text) {
    const raw = String(text || "");
    if (!raw) return "";

    const candidates = [raw];

    const sourcePatterns = [
      /<source[^>]*src=["']([^"']+)["']/gi,
      /<video[^>]*src=["']([^"']+)["']/gi,
      /\[video\]\(([^)]+)\)/gi,
      /(https?:\/\/[^\s"'`)<]+\.(mp4|webm)[^\s"'`)<]*)/gi,
      /(\/grok\/v1\/files\/video\/[^\s"'`)<]+)/gi,
      /(\/v1\/files\/video\/[^\s"'`)<]+)/gi,
    ];

    for (const candidate of candidates) {
      const normalized = candidate.replace(/\\\//g, "/");
      for (const pattern of sourcePatterns) {
        pattern.lastIndex = 0;
        let match = null;
        let lastHit = "";
        while ((match = pattern.exec(normalized))) {
          lastHit = String(match[1] || match[0] || "");
        }
        if (lastHit) {
          return lastHit.replace(/[),.;]+$/g, "");
        }
      }
    }

    return "";
  }

  function extractProgress(text) {
    const raw = String(text || "");
    if (!raw) return null;

    let match = raw.match(/(?:progress|进度)\s*[:：]?\s*(\d{1,3})\s*%/i);
    if (match && match[1]) {
      const value = Number(match[1]);
      if (Number.isFinite(value)) return Math.max(0, Math.min(100, value));
    }

    match = raw.match(/(\d{1,3})\s*%/);
    if (match && match[1]) {
      const value = Number(match[1]);
      if (Number.isFinite(value)) return Math.max(0, Math.min(100, value));
    }

    return null;
  }

  function createPreviewItem() {
    state.previewCount += 1;

    const item = document.createElement("div");
    item.className = "video-item";
    item.dataset.index = String(state.previewCount);

    const bar = document.createElement("div");
    bar.className = "video-item-bar";

    const title = document.createElement("div");
    title.className = "video-item-title";
    title.textContent = `Video ${state.previewCount}`;

    const actions = document.createElement("div");
    actions.className = "video-item-actions";

    const openBtn = document.createElement("a");
    openBtn.className = "btn secondary small video-open hidden";
    openBtn.target = "_blank";
    openBtn.rel = "noopener";
    openBtn.textContent = "Open";

    const downloadBtn = document.createElement("button");
    downloadBtn.className = "btn secondary small video-download";
    downloadBtn.type = "button";
    downloadBtn.textContent = "Download";
    downloadBtn.disabled = true;

    actions.appendChild(openBtn);
    actions.appendChild(downloadBtn);
    bar.appendChild(title);
    bar.appendChild(actions);

    const body = document.createElement("div");
    body.className = "video-item-body";
    body.innerHTML = '<div class="video-item-placeholder">Generating...</div>';

    const link = document.createElement("div");
    link.className = "video-item-link";

    item.appendChild(bar);
    item.appendChild(body);
    item.appendChild(link);

    videoStageEl.prepend(item);
    videoEmptyEl.style.display = "none";
    return item;
  }

  function ensurePreviewItem() {
    if (!state.currentPreviewItem) {
      state.currentPreviewItem = createPreviewItem();
    }
    return state.currentPreviewItem;
  }

  function updatePreviewItemURL(item, url) {
    const resolved = buildVideoURL(url);
    item.dataset.url = resolved;

    const openBtn = item.querySelector(".video-open");
    const downloadBtn = item.querySelector(".video-download");
    const link = item.querySelector(".video-item-link");

    if (resolved) {
      openBtn.classList.remove("hidden");
      openBtn.href = resolved;
      downloadBtn.disabled = false;
      downloadBtn.dataset.url = resolved;
      link.textContent = resolved;
      link.classList.add("has-url");
    } else {
      openBtn.classList.add("hidden");
      openBtn.removeAttribute("href");
      downloadBtn.disabled = true;
      delete downloadBtn.dataset.url;
      link.textContent = "";
      link.classList.remove("has-url");
    }
  }

  function renderVideo(url) {
    const item = ensurePreviewItem();
    const body = item.querySelector(".video-item-body");
    const resolved = buildVideoURL(url);
    if (!resolved) return;

    body.innerHTML = "";
    const video = document.createElement("video");
    video.controls = true;
    video.preload = "metadata";
    video.src = resolved;
    body.appendChild(video);

    updatePreviewItemURL(item, resolved);
  }

  function handleDeltaText(text) {
    const raw = String(text || "");
    if (!raw) return;

    const sanitized = raw.replace(/<think>[\s\S]*?<\/think>/gi, "");

    if (/超分辨率|upscal/i.test(sanitized)) {
      setStatus("Upscaling...", "ok");
      setIndeterminate(true);
      progressTextEl.textContent = "Upscaling...";
    }

    const progress = extractProgress(sanitized);
    if (progress !== null) {
      setIndeterminate(false);
      setProgress(progress);
    }

    state.contentBuffer += sanitized;

    const directURL = extractVideoURL(sanitized);
    if (directURL) {
      renderVideo(directURL);
      setIndeterminate(false);
      setProgress(100);
      return;
    }

    const bufferedURL = extractVideoURL(state.contentBuffer);
    if (bufferedURL) {
      renderVideo(bufferedURL);
      setIndeterminate(false);
      setProgress(100);
    }
  }

  async function createTask(payload) {
    const data = await window.PublicApp.requestJSON("/v1/public/video/start", {
      method: "POST",
      body: JSON.stringify(payload),
    });

    const taskID = String(data.task_id || "").trim();
    if (!taskID) {
      throw new Error("Missing task_id");
    }
    return taskID;
  }

  async function stopRemote() {
    if (!state.taskID) return;
    try {
      await window.PublicApp.requestJSON("/v1/public/video/stop", {
        method: "POST",
        body: JSON.stringify({ task_ids: [state.taskID] }),
      });
    } catch (err) {
      // ignore remote stop errors during teardown
    }
  }

  function buildSSEURL(taskID) {
    const params = new URLSearchParams({
      task_id: String(taskID || ""),
      t: String(Date.now()),
    });
    const publicKey = String(window.PublicApp.getPublicKey() || "").trim();
    if (publicKey) {
      params.set("public_key", publicKey);
    }
    return `/v1/public/video/sse?${params.toString()}`;
  }

  function finish(hasError) {
    if (!state.running) return;

    closeStream();
    stopElapsedTimer();
    setRunning(false);

    if (hasError) {
      setStatus("Failed", "error");
      return;
    }

    setIndeterminate(false);
    setProgress(100);
    setStatus("Done", "ok");

    if (state.startAt) {
      const seconds = Math.max(0, Math.round((Date.now() - state.startAt) / 1000));
      durationValueEl.textContent = `Elapsed ${seconds}s`;
    }
  }

  function openSSE(taskID) {
    const es = new EventSource(buildSSEURL(taskID));
    state.stream = es;

    es.onmessage = (event) => {
      const text = String(event.data || "");
      if (!text) return;

      appendLog(text);

      if (text === "[DONE]") {
        finish(false);
        return;
      }

      let payload = null;
      try {
        payload = JSON.parse(text);
      } catch (err) {
        handleDeltaText(text);
        return;
      }

      const errorMessage = payload && (payload.error || payload?.error?.message);
      if (errorMessage) {
        setStatus(String(errorMessage), "error");
        finish(true);
        return;
      }

      const choice = payload?.choices?.[0];
      const deltaContent = choice?.delta?.content;
      const messageContent = choice?.message?.content;

      if (typeof deltaContent === "string" && deltaContent.trim()) {
        handleDeltaText(deltaContent);
      } else if (typeof messageContent === "string" && messageContent.trim()) {
        handleDeltaText(messageContent);
      }

      if (choice && choice.finish_reason === "stop") {
        finish(false);
      }
    };

    es.onerror = () => {
      if (!state.running) return;
      setStatus("Stream closed", "error");
      finish(true);
    };
  }

  async function start() {
    if (state.running) return;

    const prompt = String(promptEl.value || "").trim();
    if (!prompt) {
      setStatus("Prompt cannot be empty", "error");
      return;
    }

    const textImageURL = String(imageURLEl.value || "").trim();
    if (state.fileDataURL && textImageURL) {
      setStatus("Use either URL/Data URI or upload file, not both", "error");
      return;
    }

    const payload = {
      prompt,
      aspect_ratio: String(ratioEl.value || "3:2"),
      video_length: Number(lengthEl.value || 6),
      resolution_name: String(resolutionEl.value || "480p"),
      preset: String(presetEl.value || "normal"),
      image_url: state.fileDataURL || textImageURL,
      reasoning_effort: String(effortEl.value || "").trim(),
    };

    closeStream();
    clearLog();
    updateMeta();
    resetOutput(false);
    ensurePreviewItem();

    state.taskID = "";
    state.startAt = 0;

    setRunning(true);
    setStatus("Starting...");
    setIndeterminate(true);

    try {
      const taskID = await createTask(payload);
      state.taskID = taskID;
      state.startAt = Date.now();
      startElapsedTimer();
      setStatus("Streaming...", "ok");
      openSSE(taskID);
    } catch (err) {
      stopElapsedTimer();
      setRunning(false);
      setIndeterminate(false);
      setStatus(err.message || "Start failed", "error");
    }
  }

  async function stop() {
    if (!state.running) return;

    closeStream();
    stopElapsedTimer();
    await stopRemote();

    state.taskID = "";
    setRunning(false);
    setIndeterminate(false);
    setStatus("Stopped");
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
      resetOutput(true);
      clearLog();
      setStatus("");
    });

    ratioEl.addEventListener("change", updateMeta);
    lengthEl.addEventListener("change", updateMeta);
    resolutionEl.addEventListener("change", updateMeta);
    presetEl.addEventListener("change", updateMeta);

    selectImageFileBtn.addEventListener("click", () => {
      imageFileInputEl.click();
    });

    clearImageFileBtn.addEventListener("click", () => {
      clearFileSelection();
    });

    imageFileInputEl.addEventListener("change", () => {
      const file = imageFileInputEl.files && imageFileInputEl.files[0];
      if (!file) {
        clearFileSelection();
        return;
      }
      if (imageURLEl.value.trim()) {
        imageURLEl.value = "";
      }
      imageFileNameEl.textContent = file.name;

      const reader = new FileReader();
      reader.onload = () => {
        if (typeof reader.result === "string") {
          state.fileDataURL = reader.result;
          return;
        }
        state.fileDataURL = "";
        setStatus("Failed to read selected file", "error");
      };
      reader.onerror = () => {
        state.fileDataURL = "";
        setStatus("Failed to read selected file", "error");
      };
      reader.readAsDataURL(file);
    });

    imageURLEl.addEventListener("input", () => {
      if (!imageURLEl.value.trim()) return;
      if (state.fileDataURL) {
        clearFileSelection();
      }
    });

    videoStageEl.addEventListener("click", async (event) => {
      const target = event.target;
      if (!(target instanceof HTMLElement)) return;
      if (!target.classList.contains("video-download")) return;

      const rawURL = String(target.dataset.url || "").trim();
      if (!rawURL) return;

      try {
        const response = await fetch(rawURL, { mode: "cors" });
        if (!response.ok) {
          throw new Error("download_failed");
        }
        const blob = await response.blob();
        const blobURL = URL.createObjectURL(blob);

        const item = target.closest(".video-item");
        const index = item ? String(item.dataset.index || "") : "";
        const fileName = index ? `grok_video_${index}.mp4` : `grok_video_${Date.now()}.mp4`;

        const link = document.createElement("a");
        link.href = blobURL;
        link.download = fileName;
        document.body.appendChild(link);
        link.click();
        link.remove();
        URL.revokeObjectURL(blobURL);
      } catch (err) {
        setStatus("Download failed", "error");
      }
    });

    promptEl.addEventListener("keydown", (event) => {
      if ((event.ctrlKey || event.metaKey) && event.key === "Enter") {
        event.preventDefault();
        start().catch((err) => {
          setStatus(err.message || "Start failed", "error");
        });
      }
    });

    window.addEventListener("beforeunload", () => {
      closeStream();
    });
  }

  function init() {
    setRunning(false);
    updateMeta();
    resetOutput(true);
    clearLog();
    bindEvents();
  }

  init();
})();
