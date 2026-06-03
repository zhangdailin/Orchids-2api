(function () {
  const voiceEl = document.getElementById("voice");
  const customVoiceEl = document.getElementById("customVoice");
  const customVoiceFieldEl = document.getElementById("customVoiceField");
  const personalityEl = document.getElementById("personality");
  const speedEl = document.getElementById("speed");
  const instructionEl = document.getElementById("instruction");

  const startBtn = document.getElementById("startBtn");
  const stopBtn = document.getElementById("stopBtn");
  const fetchBtn = document.getElementById("fetchBtn");
  const clearLogBtn = document.getElementById("clearLogBtn");
  const copyLogBtn = document.getElementById("copyLogBtn");

  const statusEl = document.getElementById("status");
  const tokenEl = document.getElementById("tokenOutput");
  const urlEl = document.getElementById("urlOutput");
  const logEl = document.getElementById("logOutput");
  const audioRootEl = document.getElementById("audioRoot");
  const visualizerEl = document.getElementById("visualizer");

  const statusVoiceEl = document.getElementById("statusVoice");
  const statusPersonalityEl = document.getElementById("statusPersonality");
  const statusSpeedEl = document.getElementById("statusSpeed");
  const statusURLEl = document.getElementById("statusURL");

  const state = {
    running: false,
    room: null,
    visualizerTimer: null,
    visualizerBars: [],
    visualizerRaf: 0,
    audioContext: null,
    analyser: null,
    analyserSource: null,
    analyserData: null,
  };

  function setStatus(message, level) {
    window.PublicApp.setStatus(statusEl, message, level);
  }

  function setRunning(running) {
    state.running = running;
    startBtn.disabled = running;
    stopBtn.disabled = !running;
  }

  function appendLog(line, isError) {
    const time = new Date().toLocaleTimeString();
    const text = `[${time}] ${String(line || "")}`;
    logEl.textContent += `${text}\n`;
    if (isError) {
      setStatus(String(line || "Error"), "error");
    }
    logEl.scrollTop = logEl.scrollHeight;
  }

  function clearLogs() {
    logEl.textContent = "";
  }

  async function copyLogs() {
    const text = String(logEl.textContent || "");
    if (!text.trim()) {
      setStatus("No logs to copy", "error");
      return;
    }

    if (navigator.clipboard && typeof navigator.clipboard.writeText === "function") {
      await navigator.clipboard.writeText(text);
      setStatus("Logs copied", "ok");
      return;
    }

    const input = document.createElement("textarea");
    input.value = text;
    document.body.appendChild(input);
    input.select();
    document.execCommand("copy");
    input.remove();
    setStatus("Logs copied", "ok");
  }

  function updateMeta() {
    const voiceSelect = String(voiceEl.value || "-");
    const customVoice = String(customVoiceEl?.value || "").trim();
    const voice = voiceSelect === "custom" ? (customVoice || "custom") : voiceSelect;
    const personality = String(personalityEl.value || "-");
    const speed = Math.max(0.1, Number(speedEl.value || 1));

    if (customVoiceFieldEl) customVoiceFieldEl.classList.toggle("hidden", voiceSelect !== "custom");
    statusVoiceEl.textContent = voice;
    statusPersonalityEl.textContent = personality;
    statusSpeedEl.textContent = `${speed}x`;
  }

  function resetAudio() {
    audioRootEl.innerHTML = "";
  }

  function initVisualizer() {
    if (!visualizerEl) return;
    visualizerEl.innerHTML = "";
    state.visualizerBars = [];
    for (let i = 0; i < 18; i += 1) {
      const bar = document.createElement("span");
      bar.className = "bar";
      bar.style.height = "8%";
      bar.dataset.level = "8";
      visualizerEl.appendChild(bar);
      state.visualizerBars.push(bar);
    }
  }

  function resetVisualizerBars() {
    for (const bar of state.visualizerBars) {
      bar.style.height = "8%";
      bar.dataset.level = "8";
    }
  }

  function stopFallbackVisualizer() {
    if (state.visualizerTimer) {
      clearInterval(state.visualizerTimer);
      state.visualizerTimer = null;
    }
  }

  function startFallbackVisualizer() {
    if (!visualizerEl) return;
    if (state.visualizerBars.length === 0) {
      initVisualizer();
    }
    stopFallbackVisualizer();
    state.visualizerTimer = window.setInterval(() => {
      for (const bar of state.visualizerBars) {
        const prev = Number(bar.dataset.level || "8");
        const next = 10 + Math.floor(Math.random() * 30);
        const mixed = Math.round(prev * 0.6 + next * 0.4);
        bar.style.height = `${mixed}%`;
        bar.dataset.level = String(mixed);
      }
    }, 160);
  }

  function stopAudioAnalyser() {
    if (state.visualizerRaf) {
      cancelAnimationFrame(state.visualizerRaf);
      state.visualizerRaf = 0;
    }

    if (state.analyserSource) {
      try {
        state.analyserSource.disconnect();
      } catch (err) {
        // ignore
      }
      state.analyserSource = null;
    }

    if (state.analyser) {
      try {
        state.analyser.disconnect();
      } catch (err) {
        // ignore
      }
      state.analyser = null;
    }

    state.analyserData = null;

    if (state.audioContext) {
      const ctx = state.audioContext;
      state.audioContext = null;
      try {
        ctx.close();
      } catch (err) {
        // ignore
      }
    }
  }

  async function startAudioAnalyser(mediaTrack) {
    if (!mediaTrack || typeof MediaStream === "undefined") {
      throw new Error("missing_media_track");
    }

    const AudioContextCtor = window.AudioContext || window.webkitAudioContext;
    if (!AudioContextCtor) {
      throw new Error("audio_context_unavailable");
    }

    stopAudioAnalyser();
    stopFallbackVisualizer();

    const stream = new MediaStream([mediaTrack]);
    const audioContext = new AudioContextCtor();
    if (audioContext.state === "suspended") {
      await audioContext.resume();
    }

    const source = audioContext.createMediaStreamSource(stream);
    const analyser = audioContext.createAnalyser();
    analyser.fftSize = 256;
    analyser.smoothingTimeConstant = 0.82;
    source.connect(analyser);

    state.audioContext = audioContext;
    state.analyserSource = source;
    state.analyser = analyser;
    state.analyserData = new Uint8Array(analyser.frequencyBinCount);

    const render = () => {
      if (!state.analyser || !state.analyserData) {
        return;
      }

      state.analyser.getByteFrequencyData(state.analyserData);
      const data = state.analyserData;
      const bars = state.visualizerBars;
      const binStep = Math.max(1, Math.floor(data.length / Math.max(1, bars.length)));

      for (let i = 0; i < bars.length; i += 1) {
        const start = i * binStep;
        const end = Math.min(data.length, start + binStep);
        let sum = 0;
        let count = 0;
        for (let j = start; j < end; j += 1) {
          sum += data[j];
          count += 1;
        }
        const avg = count > 0 ? sum / count : 0;
        const normalized = avg / 255;
        const target = Math.max(8, Math.min(100, Math.round(8 + normalized * 92)));
        const prev = Number(bars[i].dataset.level || "8");
        const smooth = target >= prev ? Math.round(prev * 0.35 + target * 0.65) : Math.round(prev * 0.78 + target * 0.22);
        bars[i].style.height = `${smooth}%`;
        bars[i].dataset.level = String(smooth);
      }

      state.visualizerRaf = requestAnimationFrame(render);
    };

    state.visualizerRaf = requestAnimationFrame(render);
  }

  function stopVisualizer() {
    stopFallbackVisualizer();
    stopAudioAnalyser();
    resetVisualizerBars();
  }

  async function startVisualizer(track) {
    if (!visualizerEl) return;
    if (state.visualizerBars.length === 0) {
      initVisualizer();
    }

    if (track) {
      try {
        await startAudioAnalyser(track);
        return;
      } catch (err) {
        // fallback to lightweight animation if analyser setup fails
      }
    }

    startFallbackVisualizer();
  }

  function getLiveKitClient() {
    const lk = window.LiveKitClient || window.LivekitClient || window.livekitClient;
    if (!lk) {
      throw new Error("LiveKit SDK not loaded");
    }
    return lk;
  }

  function ensureMicrophoneSupport() {
    const hasMediaDevices = typeof navigator !== "undefined" && navigator.mediaDevices;
    const hasGetUserMedia = hasMediaDevices && typeof navigator.mediaDevices.getUserMedia === "function";
    if (!hasGetUserMedia) {
      throw new Error("Microphone is not supported in this environment");
    }
  }

  async function fetchTokenPayload() {
    const voiceSelect = String(voiceEl.value || "ara").trim() || "ara";
    const customVoice = String(customVoiceEl?.value || "").trim();
    const voice = voiceSelect === "custom" ? customVoice : voiceSelect;
    const personality = String(personalityEl.value || "assistant").trim() || "assistant";
    const speed = Math.max(0.1, Number(speedEl.value || 1));
    if (!voice) {
      throw new Error("Custom voice_id is required");
    }

    const data = await window.PublicApp.requestJSON("/v1/public/voice/token", {
      method: "POST",
      body: JSON.stringify({ voice, personality, speed }),
    });

    const token = String(data.token || "").trim();
    const url = String(data.url || "").trim();
    if (!token || !url) {
      throw new Error("Invalid voice token response");
    }

    tokenEl.value = token;
    urlEl.value = url;
    statusURLEl.textContent = url;

    return { token, url, voice, personality, speed };
  }

  async function fetchTokenOnly() {
    updateMeta();
    setStatus("Fetching token...");
    try {
      await fetchTokenPayload();
      appendLog("Fetched token successfully");
      setStatus("Token ready", "ok");
    } catch (err) {
      appendLog(err.message || "Fetch token failed", true);
    }
  }

  async function startSession() {
    if (state.running) {
      return;
    }

    updateMeta();
    setRunning(true);
    setStatus("Starting session...");

    let room = null;
    try {
      ensureMicrophoneSupport();
      const lk = getLiveKitClient();
      const payload = await fetchTokenPayload();

      appendLog(`Token ready for voice=${payload.voice}, personality=${payload.personality}, speed=${payload.speed}`);

      room = new lk.Room({
        adaptiveStream: true,
        dynacast: true,
        audioCaptureDefaults: {
          autoGainControl: true,
          echoCancellation: true,
          noiseSuppression: true,
        },
      });

      room.on(lk.RoomEvent.ParticipantConnected, (participant) => {
        appendLog(`Participant connected: ${participant.identity || "unknown"}`);
      });

      room.on(lk.RoomEvent.ParticipantDisconnected, (participant) => {
        appendLog(`Participant disconnected: ${participant.identity || "unknown"}`);
      });

      room.on(lk.RoomEvent.TrackSubscribed, (track) => {
        if (!track || track.kind !== "audio") return;
        const element = track.attach();
        element.autoplay = true;
        audioRootEl.appendChild(element);
        appendLog("Subscribed audio track");
      });

      room.on(lk.RoomEvent.Disconnected, () => {
        appendLog("Room disconnected");
        if (state.room === room) {
          state.room = null;
          setRunning(false);
          setStatus("Disconnected");
          stopVisualizer();
        }
      });

      appendLog("Connecting to LiveKit...");
      await room.connect(payload.url, payload.token);
      appendLog("Connected to LiveKit");

      appendLog("Enabling microphone...");
      let meterTrack = null;
      if (!room.localParticipant || typeof room.localParticipant.setMicrophoneEnabled !== "function") {
        throw new Error("LiveKit microphone API is unavailable");
      }
      await room.localParticipant.setMicrophoneEnabled(true);
      const publications = room.localParticipant.audioTrackPublications;
      if (publications && typeof publications.forEach === "function") {
        publications.forEach((pub) => {
          const track = pub && (pub.track || pub.audioTrack);
          if (!meterTrack && track && track.mediaStreamTrack) {
            meterTrack = track.mediaStreamTrack;
          }
        });
      }
      await startVisualizer(meterTrack);

      appendLog("Voice session started");
      state.room = room;
      setStatus("Session running", "ok");
    } catch (err) {
      if (room) {
        try {
          room.disconnect();
        } catch (disconnectErr) {
          // ignore
        }
      }
      state.room = null;
      setRunning(false);
      stopVisualizer();
      appendLog(err.message || "Start session failed", true);
    }
  }

  async function stopSession() {
    if (!state.running && !state.room) {
      return;
    }

    const room = state.room;
    state.room = null;

    if (room) {
      try {
        room.disconnect();
      } catch (err) {
        // ignore
      }
    }

    resetAudio();
    setRunning(false);
    setStatus("Stopped");
    stopVisualizer();
    appendLog("Session stopped");
  }

  function bindEvents() {
    startBtn.addEventListener("click", () => {
      startSession().catch((err) => {
        appendLog(err.message || "Start session failed", true);
      });
    });

    stopBtn.addEventListener("click", () => {
      stopSession().catch((err) => {
        appendLog(err.message || "Stop session failed", true);
      });
    });

    fetchBtn.addEventListener("click", () => {
      fetchTokenOnly().catch((err) => {
        appendLog(err.message || "Fetch token failed", true);
      });
    });

    clearLogBtn.addEventListener("click", () => {
      clearLogs();
    });

    copyLogBtn.addEventListener("click", () => {
      copyLogs().catch((err) => {
        appendLog(err.message || "Copy log failed", true);
      });
    });

    voiceEl.addEventListener("change", updateMeta);
    if (customVoiceEl) {
      customVoiceEl.addEventListener("input", updateMeta);
    }
    personalityEl.addEventListener("change", updateMeta);
    speedEl.addEventListener("change", updateMeta);
    if (instructionEl) instructionEl.addEventListener("input", updateMeta);

    window.addEventListener("beforeunload", () => {
      if (!state.room) return;
      try {
        state.room.disconnect();
      } catch (err) {
        // ignore
      }
      stopVisualizer();
    });
  }

  function init() {
    setRunning(false);
    updateMeta();
    statusURLEl.textContent = "-";
    initVisualizer();
    bindEvents();
  }

  init();
})();
