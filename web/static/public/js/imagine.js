(function () {
  const promptEl = document.getElementById("prompt");
  const ratioEl = document.getElementById("ratio");
  const nsfwEl = document.getElementById("nsfw");
  const concurrentEl = document.getElementById("concurrent");
  const modeEl = document.getElementById("mode");
  const startBtn = document.getElementById("startBtn");
  const stopBtn = document.getElementById("stopBtn");
  const clearBtn = document.getElementById("clearBtn");
  const statusEl = document.getElementById("status");
  const gridEl = document.getElementById("imageGrid");

  const state = {
    running: false,
    taskIDs: [],
    wsList: [],
    sseList: [],
    seq: 0,
  };

  function setRunning(running) {
    state.running = running;
    startBtn.disabled = running;
    stopBtn.disabled = !running;
  }

  function closeAll() {
    state.wsList.forEach((ws) => {
      if (!ws) return;
      try {
        if (ws.readyState === WebSocket.OPEN) ws.send(JSON.stringify({ type: "stop" }));
      } catch (err) {}
      try {
        ws.close(1000, "stop");
      } catch (err) {}
    });
    state.wsList = [];
    state.sseList.forEach((es) => {
      if (!es) return;
      try {
        es.close();
      } catch (err) {}
    });
    state.sseList = [];
  }

  function addImage(payload) {
    const src = window.PublicApp.detectImageSource(payload);
    if (!src) return;
    state.seq += 1;

    const card = document.createElement("div");
    card.className = "image-card";

    const img = document.createElement("img");
    img.src = src;
    img.loading = "lazy";
    img.alt = `imagine-${state.seq}`;
    img.addEventListener("click", () => window.open(src, "_blank", "noopener"));

    const meta = document.createElement("div");
    meta.className = "image-meta";
    const left = document.createElement("span");
    left.textContent = `#${payload.sequence || state.seq}`;
    const right = document.createElement("span");
    right.textContent = payload.elapsed_ms ? `${payload.elapsed_ms}ms` : "";
    meta.appendChild(left);
    meta.appendChild(right);

    card.appendChild(img);
    card.appendChild(meta);
    gridEl.prepend(card);
  }

  function handleMessage(data) {
    if (!data || typeof data !== "object") return;
    if (data.type === "image") {
      addImage(data);
      return;
    }
    if (data.type === "image_generation.partial_image" || data.type === "image_generation.completed") {
      addImage(data);
      return;
    }
    if (data.type === "status") {
      if (data.status === "running") {
        window.PublicApp.setStatus(statusEl, "Running", "ok");
      } else if (data.status === "stopped" && state.running) {
        window.PublicApp.setStatus(statusEl, "Stopped");
      }
      return;
    }
    if (data.type === "error") {
      window.PublicApp.setStatus(statusEl, data.message || "Imagine error", "error");
    }
  }

  async function createTask(prompt, aspectRatio, nsfw) {
    const data = await window.PublicApp.requestJSON("/v1/public/imagine/start", {
      method: "POST",
      body: JSON.stringify({
        prompt,
        aspect_ratio: aspectRatio,
        nsfw,
      }),
    });
    const taskID = String(data.task_id || "").trim();
    if (!taskID) throw new Error("Missing task_id");
    return taskID;
  }

  async function stopTasks(taskIDs) {
    if (!taskIDs || taskIDs.length === 0) return;
    try {
      await window.PublicApp.requestJSON("/v1/public/imagine/stop", {
        method: "POST",
        body: JSON.stringify({ task_ids: taskIDs }),
      });
    } catch (err) {}
  }

  function startSSE(taskID) {
    const es = new EventSource(`/v1/public/imagine/sse?task_id=${encodeURIComponent(taskID)}&t=${Date.now()}`);
    es.onmessage = (event) => {
      try {
        handleMessage(JSON.parse(event.data));
      } catch (err) {}
    };
    es.onerror = () => {
      try {
        es.close();
      } catch (err) {}
    };
    state.sseList.push(es);
  }

  function startWS(taskID, prompt, aspectRatio, nsfw, mode) {
    const ws = new WebSocket(
      window.PublicApp.wsBaseURL("/v1/public/imagine/ws", { task_id: taskID, t: Date.now() })
    );
    let openTimer = null;
    let fallbackStarted = false;

    const fallbackToSSE = () => {
      if (fallbackStarted || mode !== "auto") return;
      fallbackStarted = true;
      startSSE(taskID);
    };

    ws.onopen = () => {
      if (openTimer) {
        clearTimeout(openTimer);
        openTimer = null;
      }
      try {
        ws.send(
          JSON.stringify({
            type: "start",
            prompt,
            aspect_ratio: aspectRatio,
            nsfw,
          })
        );
      } catch (err) {}
    };

    ws.onmessage = (event) => {
      try {
        handleMessage(JSON.parse(event.data));
      } catch (err) {}
    };

    ws.onerror = () => {
      fallbackToSSE();
    };
    ws.onclose = () => {
      fallbackToSSE();
    };

    if (mode === "auto") {
      openTimer = window.setTimeout(() => {
        if (ws.readyState !== WebSocket.OPEN) fallbackToSSE();
      }, 1500);
    }

    state.wsList.push(ws);
  }

  async function start() {
    if (state.running) return;
    const prompt = String(promptEl.value || "").trim();
    if (!prompt) {
      window.PublicApp.setStatus(statusEl, "Prompt cannot be empty", "error");
      return;
    }
    const aspectRatio = String(ratioEl.value || "2:3");
    const nsfw = String(nsfwEl.value || "true") === "true";
    const concurrent = Math.max(1, Math.min(3, Number(concurrentEl.value || 1)));
    const mode = String(modeEl.value || "auto").toLowerCase();

    closeAll();
    setRunning(true);
    window.PublicApp.setStatus(statusEl, "Starting...");
    state.taskIDs = [];

    try {
      for (let i = 0; i < concurrent; i += 1) {
        const taskID = await createTask(prompt, aspectRatio, nsfw);
        state.taskIDs.push(taskID);
      }

      if (mode === "sse") {
        state.taskIDs.forEach((taskID) => startSSE(taskID));
      } else {
        state.taskIDs.forEach((taskID) => startWS(taskID, prompt, aspectRatio, nsfw, mode));
      }
      window.PublicApp.setStatus(statusEl, `Running (${mode.toUpperCase()})`, "ok");
    } catch (err) {
      await stopTasks(state.taskIDs);
      closeAll();
      state.taskIDs = [];
      setRunning(false);
      window.PublicApp.setStatus(statusEl, err.message || "Start failed", "error");
    }
  }

  async function stop() {
    if (!state.running) return;
    const taskIDs = state.taskIDs.slice();
    closeAll();
    state.taskIDs = [];
    setRunning(false);
    window.PublicApp.setStatus(statusEl, "Stopping...");
    await stopTasks(taskIDs);
    window.PublicApp.setStatus(statusEl, "Stopped");
  }

  startBtn.addEventListener("click", () => {
    start().catch((err) => window.PublicApp.setStatus(statusEl, err.message || "Start failed", "error"));
  });
  stopBtn.addEventListener("click", () => {
    stop().catch((err) => window.PublicApp.setStatus(statusEl, err.message || "Stop failed", "error"));
  });
  clearBtn.addEventListener("click", () => {
    gridEl.innerHTML = "";
  });

  window.addEventListener("beforeunload", () => {
    closeAll();
  });

  setRunning(false);
})();
