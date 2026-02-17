(function () {
  const voiceEl = document.getElementById("voice");
  const personalityEl = document.getElementById("personality");
  const speedEl = document.getElementById("speed");

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
    localTracks: [],
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
    const voice = String(voiceEl.value || "-");
    const personality = String(personalityEl.value || "-");
    const speed = Math.max(0.1, Number(speedEl.value || 1));

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
    const voice = String(voiceEl.value || "ara").trim() || "ara";
    const personality = String(personalityEl.value || "assistant").trim() || "assistant";
    const speed = Math.max(0.1, Number(speedEl.value || 1));

    const params = new URLSearchParams({
      voice,
      personality,
      speed: String(speed),
    });

    const data = await window.PublicApp.requestJSON(`/v1/public/voice/token?${params.toString()}`, {
      method: "GET",
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
      });

      const roomEvent = lk.RoomEvent || {};
      const trackObj = lk.Track || {};
      const audioKind = trackObj.Kind && trackObj.Kind.Audio ? trackObj.Kind.Audio : "audio";
      const evParticipantConnected = roomEvent.ParticipantConnected || "participantConnected";
      const evParticipantDisconnected = roomEvent.ParticipantDisconnected || "participantDisconnected";
      const evTrackSubscribed = roomEvent.TrackSubscribed || "trackSubscribed";
      const evDisconnected = roomEvent.Disconnected || "disconnected";

      room.on(evParticipantConnected, (participant) => {
        appendLog(`Participant connected: ${participant.identity || "unknown"}`);
      });

      room.on(evParticipantDisconnected, (participant) => {
        appendLog(`Participant disconnected: ${participant.identity || "unknown"}`);
      });

      room.on(evTrackSubscribed, (track) => {
        if (track.kind !== audioKind && track.kind !== "audio") {
          return;
        }
        const element = track.attach();
        element.autoplay = true;
        audioRootEl.appendChild(element);
        appendLog("Subscribed audio track");
      });

      room.on(evDisconnected, () => {
        appendLog("Room disconnected");
        if (state.room === room) {
          state.room = null;
          state.localTracks = [];
          setRunning(false);
          setStatus("Disconnected");
          stopVisualizer();
        }
      });

      appendLog("Connecting to LiveKit...");
      await room.connect(payload.url, payload.token);
      appendLog("Connected to LiveKit");

      const createLocalTracks = lk.createLocalTracks;
      if (typeof createLocalTracks !== "function") {
        throw new Error("LiveKit createLocalTracks is unavailable");
      }

      appendLog("Enabling microphone...");
      const tracks = await createLocalTracks({ audio: true, video: false });
      state.localTracks = tracks;
      let meterTrack = null;
      for (const localTrack of tracks) {
        await room.localParticipant.publishTrack(localTrack);
        if (!meterTrack && localTrack && localTrack.kind === "audio" && localTrack.mediaStreamTrack) {
          meterTrack = localTrack.mediaStreamTrack;
        }
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
      state.localTracks = [];
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

    for (const track of state.localTracks) {
      if (!track) continue;
      try {
        if (typeof track.stop === "function") {
          track.stop();
        } else if (track.mediaStreamTrack && typeof track.mediaStreamTrack.stop === "function") {
          track.mediaStreamTrack.stop();
        }
      } catch (err) {
        // ignore
      }
    }
    state.localTracks = [];

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
    personalityEl.addEventListener("change", updateMeta);
    speedEl.addEventListener("change", updateMeta);

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
