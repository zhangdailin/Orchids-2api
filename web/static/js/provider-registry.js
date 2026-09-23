(() => {
  "use strict";

  // One UI registry for every provider-facing page. Provider keys are the API
  // account/channel values; labels are presentation only. Unknown providers are
  // appended by callers instead of being hidden.
  const providers = Object.freeze([
    Object.freeze({ key: "warp", label: "Warp" }),
    Object.freeze({ key: "puter", label: "Puter" }),
    Object.freeze({ key: "workbuddy", label: "WorkBuddy" }),
    Object.freeze({ key: "qoder", label: "Qoder" }),
    Object.freeze({ key: "cline", label: "Cline" }),
    Object.freeze({ key: "grok", label: "Grok" }),
  ]);
  const byKey = Object.freeze(Object.fromEntries(providers.map((item) => [item.key, item])));

  window.OrchidsProviderRegistry = Object.freeze({
    providers,
    keys: Object.freeze(providers.map((item) => item.key)),
    channels: Object.freeze(providers.map((item) => item.label)),
    label(value) {
      const raw = String(value || "").trim();
      return byKey[raw.toLowerCase()]?.label || raw;
    },
  });
})();
