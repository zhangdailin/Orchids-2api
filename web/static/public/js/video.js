(function () {
  const promptEl = document.getElementById("prompt");
  const ratioEl = document.getElementById("ratio");
  const lengthEl = document.getElementById("length");
  const resolutionEl = document.getElementById("resolution");
  const presetEl = document.getElementById("preset");
  const imageURLEl = document.getElementById("imageUrl");
  const effortEl = document.getElementById("effort");
  const startBtn = document.getElementById("startBtn");
  const stopBtn = document.getElementById("stopBtn");
  const statusEl = document.getElementById("status");
  const logEl = document.getElementById("logOutput");
  const videoEl = document.getElementById("videoOutput");
  const linkEl = document.getElementById("videoLink");

  const state = {
    taskID: "",
    stream: null,
    running: false,
  };

  function setRunning(running) {
    state.running = running;
    startBtn.disabled = running;
    stopBtn.disabled = !running;
  }

  function closeStream() {
    if (!state.stream) return;
    try {
      state.stream.close();
    } catch (err) {}
    state.stream = null;
  }

  function appendLog(line) {
    logEl.textContent += `${line}\n`;
    logEl.scrollTop = logEl.scrollHeight;
  }

  function extractVideoURL(text) {
    const candidates = [];
    const raw = String(text || "");
    if (raw) candidates.push(raw);

    try {
      const parsed = JSON.parse(raw);
      const deltaContent = parsed?.choices?.[0]?.delta?.content;
      if (typeof deltaContent === "string" && deltaContent.trim()) {
        candidates.push(deltaContent);
      }
      const messageContent = parsed?.choices?.[0]?.message?.content;
      if (typeof messageContent === "string" && messageContent.trim()) {
        candidates.push(messageContent);
      }
    } catch (err) {}

    const patterns = [
      /https?:\/\/[^\s"'`)\]]+\.(mp4|webm)[^\s"'`)\]]*/i,
      /\/grok\/v1\/files\/video\/[^\s"'`)\]]+/i,
      /\/v1\/files\/video\/[^\s"'`)\]]+/i,
    ];

    for (const candidate of candidates) {
      const normalized = String(candidate || "").replace(/\\\//g, "/");
      for (const pattern of patterns) {
        const m = normalized.match(pattern);
        if (m && m[0]) return m[0].replace(/[),.;]+$/g, "");
      }
    }

    return "";
  }

  function maybeSetVideoURL(text) {
    const url = extractVideoURL(text);
    if (!url) return;
    const src = /^https?:\/\//i.test(url) ? url : `${window.location.origin}${url}`;
    videoEl.src = src;
    linkEl.textContent = url;
    linkEl.href = src;
  }

  async function stopRemote() {
    if (!state.taskID) return;
    try {
      await window.PublicApp.requestJSON("/v1/public/video/stop", {
        method: "POST",
        body: JSON.stringify({ task_ids: [state.taskID] }),
      });
    } catch (err) {}
  }

  function openSSE(taskID) {
    const es = new EventSource(`/v1/public/video/sse?task_id=${encodeURIComponent(taskID)}&t=${Date.now()}`);
    state.stream = es;
    es.onmessage = (event) => {
      const text = String(event.data || "");
      appendLog(text);
      maybeSetVideoURL(text);
      if (text === "[DONE]") {
        window.PublicApp.setStatus(statusEl, "Done", "ok");
        closeStream();
        setRunning(false);
      }
    };
    es.onerror = () => {
      window.PublicApp.setStatus(statusEl, "Stream closed");
      closeStream();
      setRunning(false);
    };
  }

  startBtn.addEventListener("click", async () => {
    if (state.running) return;
    const prompt = String(promptEl.value || "").trim();
    if (!prompt) {
      window.PublicApp.setStatus(statusEl, "Prompt cannot be empty", "error");
      return;
    }

    setRunning(true);
    closeStream();
    state.taskID = "";
    logEl.textContent = "";
    videoEl.removeAttribute("src");
    linkEl.textContent = "";
    linkEl.removeAttribute("href");
    window.PublicApp.setStatus(statusEl, "Starting...");

    try {
      const data = await window.PublicApp.requestJSON("/v1/public/video/start", {
        method: "POST",
        body: JSON.stringify({
          prompt,
          aspect_ratio: String(ratioEl.value || "3:2"),
          video_length: Number(lengthEl.value || 6),
          resolution_name: String(resolutionEl.value || "480p"),
          preset: String(presetEl.value || "normal"),
          image_url: String(imageURLEl.value || "").trim(),
          reasoning_effort: String(effortEl.value || "").trim(),
        }),
      });
      state.taskID = String(data.task_id || "").trim();
      if (!state.taskID) throw new Error("Missing task_id");
      window.PublicApp.setStatus(statusEl, "Streaming...", "ok");
      openSSE(state.taskID);
    } catch (err) {
      setRunning(false);
      window.PublicApp.setStatus(statusEl, err.message || "Start failed", "error");
    }
  });

  stopBtn.addEventListener("click", async () => {
    if (!state.running) return;
    closeStream();
    await stopRemote();
    state.taskID = "";
    setRunning(false);
    window.PublicApp.setStatus(statusEl, "Stopped");
  });

  window.addEventListener("beforeunload", () => {
    closeStream();
  });

  setRunning(false);
})();
