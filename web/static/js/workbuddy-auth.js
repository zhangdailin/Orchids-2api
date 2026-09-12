// WorkBuddy international (www.workbuddy.ai) official browser login.
//
// The server starts the OAuth transaction and polls the upstream for the token
// pair; this module only opens the official login page in a popup and reports
// progress. Passwords and tokens never pass through this file.
globalThis.WorkBuddyLogin = (() => {
  const STORAGE_KEY = 'workbuddy_login_v1';
  let active = null;

  const statusNode = () => document.getElementById('workbuddyLoginStatus');
  const buttonNode = () => document.getElementById('workbuddyLoginButton');

  function status(text, kind = 'info') {
    const node = statusNode();
    if (!node) return;
    node.hidden = !text;
    node.classList.toggle('is-error', kind === 'error');
    node.classList.toggle('is-active', kind === 'info' && Boolean(text));
    node.textContent = text || '';
  }

  function setButton(disabled) {
    const button = buttonNode();
    if (button) button.disabled = disabled;
  }

  function clearStoredLogin() {
    try {
      window.localStorage.removeItem(STORAGE_KEY);
    } catch (_) {
      /* storage may be unavailable; the in-page timer still guards the flow */
    }
  }

  function storeLogin(login) {
    try {
      window.localStorage.setItem(STORAGE_KEY, JSON.stringify({
        loginId: login.loginId,
        expiresAt: login.expiresAt,
      }));
    } catch (_) {
      /* ignore */
    }
  }

  function readStoredLogin() {
    try {
      const raw = window.localStorage.getItem(STORAGE_KEY);
      if (!raw) return null;
      const parsed = JSON.parse(raw);
      if (!parsed || typeof parsed.loginId !== 'string' || !parsed.loginId) return null;
      if (typeof parsed.expiresAt === 'number' && Date.now() >= parsed.expiresAt) return null;
      return parsed;
    } catch (_) {
      return null;
    }
  }

  function abortActive(silent) {
    if (!active) return;
    const login = active;
    active = null;
    clearInterval(login.timer);
    if (login.controller) login.controller.abort();
    if (login.popup && !login.popup.closed) login.popup.close();
    if (!silent && login.loginId) {
      fetch(`/api/workbuddy/login/${encodeURIComponent(login.loginId)}`, {
        method: 'DELETE',
        credentials: 'same-origin',
      }).catch(() => {});
    }
    clearStoredLogin();
    setButton(false);
  }

  function finish(message, kind) {
    abortActive(true);
    status(message, kind || 'info');
    if (kind === 'error') {
      showToast(message, 'error');
      return;
    }
    showToast(message, 'success');
    closeModal();
    loadAccounts();
  }

  function fail(message) {
    if (active) {
      const loginId = active.loginId;
      active.loginId = '';
      if (loginId) {
        fetch(`/api/workbuddy/login/${encodeURIComponent(loginId)}`, {
          method: 'DELETE',
          credentials: 'same-origin',
        }).catch(() => {});
      }
    }
    finish(message, 'error');
  }

  async function poll(login) {
    if (active !== login || !login.loginId) return;
    try {
      const response = await fetch(`/api/workbuddy/login/${encodeURIComponent(login.loginId)}`, {
        credentials: 'same-origin',
        signal: login.controller.signal,
      });
      if (active !== login) return;
      if (response.status === 404) {
        fail('WorkBuddy 登录会话已失效，请重新发起登录。');
        return;
      }
      if (!response.ok) return;
      const result = await response.json();
      if (active !== login) return;
      switch (String(result.status || '')) {
        case 'complete':
          finish(result.message || 'WorkBuddy 官方登录完成，账号已保存');
          return;
        case 'failed':
          fail(result.message || 'WorkBuddy 授权失败，请重新发起登录。');
          return;
        case 'expired':
          fail(result.message || 'WorkBuddy 授权已超时，请重新发起登录。');
          return;
        default:
          break;
      }
      if (login.popup && login.popup.closed && !login.resumed) {
        login.closedAt ||= Date.now();
        if (Date.now() - login.closedAt >= 3000) {
          fail('WorkBuddy 登录窗口已关闭，尚未完成授权。');
        }
      }
    } catch (err) {
      if (err && err.name === 'AbortError') return;
      // Transient network errors are retried on the next tick.
    }
  }

  async function begin(enabled) {
    if (active) return;
    const response = await fetch('/api/workbuddy/login', {
      method: 'POST',
      credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ enabled: enabled !== false }),
    });
    if (!response.ok) throw new Error('start_failed');
    return response.json();
  }

  function start() {
    if (active) return;
    if (!window.isSecureContext) {
      status('请使用 HTTPS（本地可用 localhost）打开管理页面后再登录。', 'error');
      return;
    }
    setButton(true);
    status('正在申请 WorkBuddy 登录会话…');

    const login = {
      loginId: '',
      expiresAt: 0,
      popup: null,
      timer: null,
      controller: new AbortController(),
      closedAt: 0,
    };
    active = login;

    const openPopup = (authURL) => {
      try {
        login.popup = window.open(authURL, 'workbuddy-login', 'popup,width=560,height=760');
      } catch (_) {
        login.popup = null;
      }
      if (!login.popup) {
        fail('WorkBuddy 登录弹窗被拦截，请允许弹窗后重试。');
        return false;
      }
      return true;
    };

    (async () => {
      try {
        // Re-attach to a session that outlived a page refresh before starting a
        // new one; the server-side transaction is still pollable.
        let session = readStoredLogin();
        login.resumed = Boolean(session);
        if (!session) {
          session = await begin(document.getElementById('enabled')?.checked !== false);
        }
        if (active !== login) return;
        const loginId = String(session?.id || session?.loginId || '').trim();
        const authURL = String(session?.verification_uri_complete || '').trim();
        if (!loginId) throw new Error('invalid_session');
        login.loginId = loginId;
        login.expiresAt = Date.parse(String(session.expires_at || '')) || Date.now() + 15 * 60 * 1000;
        storeLogin(login);
        if (authURL) {
          if (!openPopup(authURL)) return;
          status('请在 WorkBuddy 官方页面完成登录与授权，本窗口会自动接管。');
        } else {
          // A resumed transaction cannot re-open the original tab, but the
          // browser step may already be finished.
          status('正在确认 WorkBuddy 授权结果…');
        }
        login.timer = setInterval(() => {
          if (active !== login) return;
          if (Date.now() >= login.expiresAt) {
            fail('WorkBuddy 授权已超时，请重新发起登录。');
            return;
          }
          poll(login);
        }, 2000);
        poll(login);
      } catch (_) {
        if (active !== login) return;
        fail('无法发起 WorkBuddy 登录；请检查服务器到 www.workbuddy.ai 的网络连通性后重试。');
      }
    })();
  }

  function stop() {
    abortActive(false);
    status('');
  }

  window.addEventListener('pagehide', () => {
    // Deliberately keep the server-side session: a refresh must be able to
    // re-attach, and the transaction expires on its own.
    if (!active) return;
    clearInterval(active.timer);
    active = null;
    setButton(false);
  });

  return { start, stop };
})();
