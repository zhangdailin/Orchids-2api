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

  // Server error codes map to operator-actionable messages. The server never
  // echoes credentials, so these strings are safe to surface verbatim.
  const ERROR_MESSAGES = {
    upstream_unreachable: '服务器无法访问 www.workbuddy.ai。请检查服务器的出网/代理设置后重试。',
    upstream_rejected: 'www.workbuddy.ai 拒绝了本次登录会话，请稍后重试。',
    origin_mismatch: '登录请求必须来自本管理页面（同源）。若通过反向代理访问，请确认主机名一致后重试。',
    insecure_origin: '请使用 HTTPS 打开管理页面后再登录（localhost 除外）。',
    store_unavailable: '服务端账号存储不可用，请检查 Redis 配置后重试。',
    too_many_logins: '待处理的 WorkBuddy 登录过多，请先完成或取消其中一个。',
    unsupported_media_type: '请求格式不被接受，请刷新页面后重试。',
    transaction_failed: '服务端创建登录事务失败，请重试。',
  };

  function messageForError(payload, fallback) {
    const code = String((payload && payload.code) || '').trim();
    if (code && ERROR_MESSAGES[code]) return ERROR_MESSAGES[code];
    const detail = String((payload && (payload.error || payload.message)) || '').trim();
    if (detail) return `${fallback}（${detail}）`;
    return fallback;
  }

  async function readErrorPayload(response) {
    try {
      const text = await response.text();
      if (!text) return null;
      try {
        return JSON.parse(text);
      } catch (_) {
        return { error: text.slice(0, 200) };
      }
    } catch (_) {
      return null;
    }
  }

  function begin(enabled) {
    // The fetch is issued synchronously inside the click gesture so the popup
    // below is still treated as user-initiated by the browser.
    return fetch('/api/workbuddy/login', {
      method: 'POST',
      credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ enabled: enabled !== false }),
    });
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

    // Reserve the popup window during the click gesture; it is navigated once
    // the server returns the official login URL.
    try {
      login.popup = window.open('about:blank', 'workbuddy-login', 'popup,width=560,height=760');
    } catch (_) {
      login.popup = null;
    }
    if (!login.popup) {
      fail('WorkBuddy 登录弹窗被拦截，请允许本站弹窗后重试。');
      return;
    }

    const openPopup = (authURL) => {
      try {
        login.popup.location.replace(authURL);
        return true;
      } catch (_) {
        try {
          login.popup.location.href = authURL;
          return true;
        } catch (_) {
          return false;
        }
      }
    };

    (async () => {
      try {
        // Re-attach to a session that outlived a page refresh before starting a
        // new one; the server-side transaction is still pollable.
        let session = readStoredLogin();
        login.resumed = Boolean(session);
        if (!session) {
          const response = await begin(document.getElementById('enabled')?.checked !== false);
          if (!response.ok) {
            const payload = await readErrorPayload(response);
            if (active !== login) return;
            fail(messageForError(payload, `发起 WorkBuddy 登录失败（HTTP ${response.status}）`));
            return;
          }
          session = await response.json();
        }
        if (active !== login) return;
        const loginId = String(session?.id || session?.loginId || '').trim();
        const authURL = String(session?.verification_uri_complete || '').trim();
        if (!loginId) throw new Error('invalid_session');
        login.loginId = loginId;
        login.expiresAt = Date.parse(String(session.expires_at || '')) || Date.now() + 15 * 60 * 1000;
        storeLogin(login);
        if (authURL) {
          if (!openPopup(authURL)) {
            fail('无法打开 WorkBuddy 官方登录页面，请允许弹窗后重试。');
            return;
          }
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
      } catch (err) {
        if (active !== login) return;
        const offline = err instanceof TypeError;
        fail(offline
          ? '无法连接本服务的登录接口，请确认服务正在运行且页面未被反向代理拦截。'
          : '发起 WorkBuddy 登录时发生异常，请刷新页面后重试。');
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
