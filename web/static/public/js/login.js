(function () {
  const keyInput = document.getElementById("publicKey");
  const saveBtn = document.getElementById("saveKeyBtn");
  const verifyBtn = document.getElementById("verifyBtn");
  const statusEl = document.getElementById("status");

  function load() {
    keyInput.value = window.PublicApp.getPublicKey();
    window.PublicApp.setStatus(statusEl, "Ready");
  }

  saveBtn.addEventListener("click", () => {
    window.PublicApp.setPublicKey(keyInput.value);
    window.PublicApp.setStatus(statusEl, "Public key saved locally", "ok");
  });

  verifyBtn.addEventListener("click", async () => {
    window.PublicApp.setPublicKey(keyInput.value);
    window.PublicApp.setStatus(statusEl, "Verifying...");
    try {
      await window.PublicApp.verifyPublicKey();
      window.PublicApp.setStatus(statusEl, "Verify success", "ok");
    } catch (err) {
      window.PublicApp.setStatus(statusEl, err.message || "Verify failed", "error");
    }
  });

  load();
})();
