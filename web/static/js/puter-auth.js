// Implements Puter's documented sign-in popup protocol, without loading a
// third-party SDK into the administrator page or persisting tokens in storage.
globalThis.PuterWebLogin = (() => {
  const origin = 'https://puter.com';
  let active = null;
  const status = (text) => {
    const node = document.getElementById('puterWebLoginStatus');
    if (node) { node.hidden = !text; node.textContent = text; }
  };
  function stop() {
    if (!active) return;
    const login = active;
    active = null;
    window.removeEventListener('message', login.listener);
    clearInterval(login.timer);
    login.controller.abort();
    if (login.popup && !login.popup.closed) login.popup.close();
    const button = document.getElementById('puterWebLoginButton');
    if (button) button.disabled = false;
    status('');
  }
  function start() {
    if (active) return;
    if (!window.isSecureContext || window.crossOriginIsolated) {
      status('请使用 HTTPS（本地可用 localhost），并允许跨源登录弹窗。');
      return;
    }
    const login = {
      id: crypto.randomUUID(), popup: null, timer: null, accepted: false,
      controller: new AbortController(), expires: Date.now() + 5 * 60 * 1000,
      enabled: document.getElementById('enabled').checked,
    };
    const fail = (message) => { if (active === login) { stop(); status(message); } };
    login.listener = async (event) => {
      if (active !== login || event.origin !== origin || !login.popup || event.source !== login.popup) return;
      const data = event.data;
      if (!data || typeof data !== 'object') return;
      if (data.msg === 'requestOrigin') {
        login.popup.postMessage({ msg: 'originResponse' }, origin);
        return;
      }
      if (data.msg !== 'puter.token' || String(data.msg_id) !== login.id || login.accepted) return;
      if (data.success !== true || typeof data.token !== 'string' || !data.token || data.token.length > 16384) {
        fail('Puter 授权未完成，请重新登录。');
        return;
      }
      login.accepted = true;
      status('授权已返回，正在验证账号和额度接口…');
      try {
        const response = await fetch('/api/puter/web-login', {
          method: 'POST', credentials: 'same-origin', signal: login.controller.signal,
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ token: data.token, enabled: login.enabled }),
        });
        if (active !== login) return;
        if (!response.ok) throw new Error('login_failed');
        const result = await response.json();
        if (result.status !== 'complete' || !result.account_id) throw new Error('invalid_response');
        stop();
        closeModal();
        loadAccounts();
        showToast('Puter 官方登录完成，账号已保存', 'success');
      } catch (_) {
        fail('登录验证或保存失败；请检查管理会话、服务器到 Puter 的网络及账号授权，然后重试。');
      }
    };
    active = login;
    window.addEventListener('message', login.listener);
    const button = document.getElementById('puterWebLoginButton');
    if (button) button.disabled = true;
    status('请在 Puter 官方弹窗中登录并确认授权。');
    try {
      // Open synchronously inside the click gesture, before any async work.
      login.popup = window.open(`${origin}/action/sign-in?embedded_in_popup=true&request_auth=true&msg_id=${encodeURIComponent(login.id)}`,
        `puter-login-${login.id}`, 'popup,width=600,height=700');
      if (!login.popup) { fail('登录弹窗被拦截，请允许弹窗后重试。'); return; }
      login.timer = setInterval(() => {
        if (Date.now() >= login.expires) { fail('Puter 登录已超时，请重试。'); return; }
        if (!login.accepted && login.popup.closed) {
          // Allow the official popup's final queued postMessage to arrive.
          login.closedAt ||= Date.now();
          if (Date.now() - login.closedAt >= 1000) fail('Puter 登录窗口已关闭，尚未完成授权。');
        }
      }, 250);
    } catch (_) { fail('无法打开 Puter 官方登录窗口，请重试。'); }
  }
  window.addEventListener('pagehide', stop);
  return { start, stop };
})();
