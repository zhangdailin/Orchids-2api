// Cline (api.cline.bot) official WorkOS device-authorization login.
//
// The channel is OAuth-only: the console starts a WorkOS device transaction,
// the server exchanges it at the Cline API, and only the resulting account is
// stored. There is no manual credential field anywhere in this flow, and no
// token ever passes through this file.
globalThis.ClineLogin = (() => {
  // Server error codes map to operator-actionable messages. The server never
  // echoes credentials, so these strings are safe to surface verbatim.
  const ERROR_MESSAGES = {
    upstream_unreachable: '服务器无法访问 WorkOS 授权端点（api.workos.com）。请检查服务器的出网/代理设置后重试。',
    upstream_rejected: 'WorkOS 拒绝了本次登录会话，请稍后重试。',
    origin_mismatch: '登录请求必须来自本管理页面（同源）。若通过反向代理访问，请确认主机名一致后重试。',
    insecure_origin: '请使用 HTTPS 打开管理页面后再登录（localhost 除外）。',
    store_unavailable: '服务端账号存储不可用，请检查 Redis 配置后重试。',
    too_many_logins: '待处理的 Cline 登录过多，请先完成或取消其中一个。',
    unsupported_media_type: '请求格式不被接受，请刷新页面后重试。',
    transaction_failed: '服务端创建登录事务失败，请重试。',
  };

  const driver = globalThis.DeviceAuthLogin.create({
    storageKey: 'cline_login_v1',
    basePath: '/api/cline/login',
    popupName: 'cline-login',
    statusId: 'clineLoginStatus',
    buttonId: 'clineLoginButton',
    linkId: 'clineLoginLink',
    linkTextId: 'clineLoginLinkText',
    label: 'Cline',
    errorMessages: ERROR_MESSAGES,
    statusPrefix: '正在申请 Cline 登录会话…',
    openingStatus: '请在 WorkOS 官方页面完成登录与授权，本窗口会自动接管。',
    authorizedStatus: '正在确认 Cline 授权结果…',
    popupBlockedMessage: 'Cline 登录弹窗被拦截，请允许本站弹窗后重试。',
    timeoutMessage: 'Cline 授权已超时，请重新发起登录。',
  });

  return { start: driver.start, stop: driver.stop };
})();
