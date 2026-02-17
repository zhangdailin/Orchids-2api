(function () {
  const STORAGE_KEY = "orchids_public_key";

  function getPublicKey() {
    return String(localStorage.getItem(STORAGE_KEY) || "").trim();
  }

  function setPublicKey(value) {
    localStorage.setItem(STORAGE_KEY, String(value || "").trim());
  }

  function authHeaders(extra) {
    const headers = Object.assign({}, extra || {});
    const key = getPublicKey();
    if (key) headers.Authorization = `Bearer ${key}`;
    return headers;
  }

  async function responseText(res) {
    const text = await res.text();
    if (!text) return `HTTP ${res.status}`;
    try {
      const data = JSON.parse(text);
      if (data && typeof data.detail === "string" && data.detail) return data.detail;
      if (data && typeof data.error === "string" && data.error) return data.error;
      return text;
    } catch (err) {
      return text;
    }
  }

  async function requestJSON(url, options) {
    const opts = Object.assign({}, options || {});
    const headers = authHeaders(opts.headers || {});
    opts.headers = headers;
    if (opts.body && !headers["Content-Type"] && !(opts.body instanceof FormData)) {
      headers["Content-Type"] = "application/json";
    }
    const res = await fetch(url, opts);
    if (!res.ok) {
      throw new Error(await responseText(res));
    }
    const text = await res.text();
    if (!text) return {};
    return JSON.parse(text);
  }

  async function verifyPublicKey() {
    return requestJSON("/v1/public/verify", { method: "GET" });
  }

  function setStatus(el, message, level) {
    if (!el) return;
    el.className = "status";
    if (level) el.classList.add(level);
    el.textContent = String(message || "");
  }

  function wsBaseURL(path, params) {
    const protocol = window.location.protocol === "https:" ? "wss:" : "ws:";
    const url = new URL(`${protocol}//${window.location.host}${path}`);
    Object.entries(params || {}).forEach(([k, v]) => {
      if (v !== undefined && v !== null && String(v) !== "") {
        url.searchParams.set(k, String(v));
      }
    });
    return url.toString();
  }

  function detectImageSource(payload) {
    if (!payload || typeof payload !== "object") return "";
    const fromURL = String(payload.file_url || payload.url || "").trim();
    if (fromURL) return fromURL;
    const b64 = String(payload.b64_json || "").trim();
    if (!b64) return "";
    if (b64.startsWith("data:")) return b64;
    let mime = "image/jpeg";
    if (b64.startsWith("iVBOR")) mime = "image/png";
    else if (b64.startsWith("R0lGOD")) mime = "image/gif";
    return `data:${mime};base64,${b64}`;
  }

  window.PublicApp = {
    getPublicKey,
    setPublicKey,
    authHeaders,
    requestJSON,
    verifyPublicKey,
    responseText,
    setStatus,
    wsBaseURL,
    detectImageSource,
  };
})();
