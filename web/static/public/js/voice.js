(function () {
  const voiceEl = document.getElementById("voice");
  const personalityEl = document.getElementById("personality");
  const speedEl = document.getElementById("speed");
  const fetchBtn = document.getElementById("fetchBtn");
  const statusEl = document.getElementById("status");
  const tokenEl = document.getElementById("tokenOutput");
  const urlEl = document.getElementById("urlOutput");

  fetchBtn.addEventListener("click", async () => {
    const voice = String(voiceEl.value || "ara").trim() || "ara";
    const personality = String(personalityEl.value || "assistant").trim() || "assistant";
    const speed = Math.max(0.1, Number(speedEl.value || 1));
    window.PublicApp.setStatus(statusEl, "Fetching...");
    try {
      const params = new URLSearchParams({
        voice,
        personality,
        speed: String(speed),
      });
      const data = await window.PublicApp.requestJSON(`/v1/public/voice/token?${params.toString()}`, {
        method: "GET",
      });
      tokenEl.value = String(data.token || "");
      urlEl.value = String(data.url || "");
      window.PublicApp.setStatus(statusEl, "Success", "ok");
    } catch (err) {
      tokenEl.value = "";
      urlEl.value = "";
      window.PublicApp.setStatus(statusEl, err.message || "Fetch failed", "error");
    }
  });
})();
