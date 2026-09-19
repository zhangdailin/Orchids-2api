# 出口 / 反爬 / 身份对比

对比范围：Orchids-2api（下称 A）Grok 通道出口层 `internal/grok/` + `internal/grok/egress/` + `internal/api/`，对 chenyme/grok2api（下称 B，HEAD 906b9493，B 路径以 `B:backend/` 为根）。
所有结论均来自源码逐行阅读，引用行号与原码；无代码证据的猜测不作发现。

## 结论摘要

- A 的 Console（DPoP）通道**完全不使用出口代理池**：`doConsoleDPoPRequestWithHeaders` 用进程级 `c.httpClient`（全局 `ProxyURL`）发请求，`EgressManager` 里的 `console` scope、clearance、UA、节点健康全部对该通道失效；B 的每个 Console 请求都经 `AcquireCredential(ScopeConsole)` + `lease.DoDeferredForbidden`（P1）。
- A 的出口亲和是**常量**（`grok-default` / `cli-default`），因此所有账号共用同一出口节点、同一指纹、同一份 grok.com clearance；B 用 `EgressIdentity` / `sso_token` 摘要做 per-credential 身份，并支持按账号隔离连接池（P1）。
- A 把浏览器 UA（含 FlareSolverr 解出的 UA）与 **grok.com 的 cf_clearance 合并进 Build/CLI 请求**（`cli-chat-proxy.grok.com`）；B 明确对 Build scope 不注入 UA、不使用浏览器 clearance（P1，跨源凭据外泄 + 指纹自相矛盾）。
- A 的 `x-statsig-id` 是**本地伪造的固定 JS 报错串**（`x1:TypeError: …` base64），没有签名服务、没有 metaContent、没有 TTL/失效；B 走签名服务 + `grok-site-verification` meta + 1h 缓存 + 反爬后失效重签（P1）。
- A 对**流内反爬拒答**（`streamErrors` / `code=7` / "anti-bot"）没有任何识别，只当普通流错误处理；B 识别后失效签名、同请求重试一次、并给出口节点记 403 反馈（P1）。
- A 的 Web chat 从不走 mgw WebSocket（`fromLegacyPayload` 恒错 → 永远退回 REST），且 mgw 在出口启用时直接拒绝；B 的 Web chat 主路径就是经 lease 的 gateway WS，带 403 clearance 失效与代理池重试（P1）。
- TLS 指纹：A 全 HTTPS 强制走自建 `http2.Transport` + `utls.HelloChrome_Auto`（库常量，与 UA 版本无关，且非 h2 直接报错）；B 用 tls-client 的 `ClientProfile`，按 UA 里的 Chrome 大版本查 `MappedTLSClients` 并做最近邻回退（P1）。
- A 在出口节点校验失败时把**带账号密码的代理 URL 原文写进日志**（`slog.Warn(..., "url", n.URL)`），B 的代理校验错误一律不含 URL（P0）。
- A 的 clearance 缓存键只含 `节点名|亲和`，不含代理 URL / FlareSolverr URL / target；配置热更新会整体重建 Client，丢失全部 clearance/健康/粘性状态；B 的 cache key 含 solver+target+proxy 指纹、带版本与分布式锁、持久化到 DB（P1/P2）。
- 其余为 P2/P3：Cookie 白名单少收 `_cfuvid`/`cf_chl_*`、请求头集（Content-Type/Origin/Baggage/Accept-Encoding/client hints）与 B 不一致、CLI trace 头缺失、`x-cluster` 适用范围过宽、403 失效策略、客户端缓存永不淘汰、节点健康指标统计反了。

## 发现

### A7-1 [P0] 出口节点校验失败把带凭据的代理 URL 原文写进日志

- 本项目：`internal/grok/egress/node.go:95` — `slog.Warn("egress node skipped: invalid proxy URL", "node", strings.TrimSpace(n.Name), "url", strings.TrimSpace(n.URL), "error", err)`（`n.URL` 即 `config.EgressNodeConfig.URL`，见 `internal/config/config.go:158` `URL string`，其解析路径 `internal/grok/egress/node.go:36-54` 允许 `user:password@host`）。
- 本项目（同类第二处）：`internal/grok/handler_voice_ws.go:203` — `proxyURL, parseErr := util.ParseProxyURL(lease.ProxyURL)` / `return nil, nil, func() {}, fmt.Errorf("parse Console voice egress proxy: %w", parseErr)`；`url.Parse` 的错误文本会原样引号回显输入 URL。
- grok2api：`B:backend/internal/application/egress/service.go:1341` — `if len(value) > maxProxyURLBytes || strings.IndexFunc(value, func(character rune) bool { return character < 0x20 || character == 0x7f }) >= 0 { return "", errors.New("代理地址过长或包含控制字符") }`（所有失败分支只返回固定文案，从不回显 URL）；`B:backend/internal/infra/egress/manager.go:615` — `attributes = append(attributes, "error", sanitizeFlareSolverrMessage(err.Error()))`。
- 差异/错误：A 唯一一处节点级日志直接打印 `n.URL`，而 A 自己在 `internal/grok/egress/flaresolverr.go:148` 已经实现了 `sanitizeFlareSolverrMessage`（含 `proxyCredentialPattern`），但没有用于这里；B 的出口校验错误文本本身不含 URL，探针/求解错误还再经一次脱敏。
- 影响：任何一次节点配置写错（端口非法、scheme 不支持）都会把 `http://user:pass@host:port` 明文落到运行日志/日志采集系统，代理账号即泄漏；同一代理通常也是账号级资源，泄漏等于出口池被盗用。
- 修复：改为 `"url", sanitizeFlareSolverrMessage(n.URL)`（或只打印 host:port + sha256 前 8 位）；`handler_voice_ws.go:203` 的错误改用 `%w` 包装前先剥离 User；把 `sanitizeFlareSolverrMessage` 提升为 `egress` 包导出并在所有 URL/错误日志点统一调用。

### A7-2 [P1] `x-statsig-id` 为本地伪造串，无签名/缓存/失效

- 本项目：`internal/grok/client.go:102` — `func (c *Client) statsigID() string { … if configured := …; isBrowserStatsigID(configured) { return configured } } return buildStatsigID()`；`internal/grok/util.go:161` — `func buildStatsigID() string { seed := randomHex(1) … return base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("x1:TypeError: Cannot read properties of null (reading 'children[\\'%s\\']')", suffix))) }`。
- 本项目（唯一注入点，无任何缓存/失效）：`internal/grok/client.go:292` — `h.Set("x-statsig-id", c.statsigID())`；`internal/grok/client.go:314` — `h.Set("x-statsig-id", c.statsigID())`；全仓无 `grok-site-verification` / `metaContent` / 签名服务调用（`grep -rEn 'grok-site-verification|metaContent|statsig' internal/` 仅命中 config 字段与 `internal/grok/{client,util}.go`）。
- grok2api：`B:backend/internal/infra/provider/web/statsig.go:73` — `func (s *statsigSigner) Sign(ctx context.Context, baseURL, signerURL, token string, lease *infraegress.Lease, method, target string) (string, string, error)`（cache → singleflight → `freshSignature` → 失败回退 stale）；`B:backend/internal/infra/provider/web/statsig.go:421` — `func (a *Adapter) applySignedStatsig(ctx, request, token, lease) { … value, source, err := a.statsig.Sign(ctx, cfg.BaseURL, cfg.StatsigSignerURL, token, lease, request.Method, request.URL.String()); request.Header.Set("x-statsig-id", value) }`，`TTL` 见 `:27 statsigCacheTTL = time.Hour`，meta 抓取见 `:274 fetchStatsigMetaContent`（`grok-site-verification`）。
- 差异/错误：B 的 statsig 值由外部签名器基于首页 metaContent 生成（`validStatsigID` 校验 base64 长度 70，`:409`），并按 URL+method 缓存 1h、反爬时 `Invalidate` 重签；A 每次随机拼一个 base64 的 `x1:TypeError…`，既与账号/页面无关，也不会在反爬后更换。
- 影响：反爬判定与 statsig 绑定在同一会话指纹上；A 送出的恒定伪值一旦被上游规则命中，只能靠出口换 IP/换 clearance 恢复，且没有任何"重新签名"恢复通道，表现为 chat 持续 403/流内拒答。
- 修复：移植 `statsigSigner`（含 `validStatsigID`、TTL、singleflight、stale 回退），在 `appChatHeaders`/`consoleHeaders` 注入处改用签名器；把 `buildStatsigID` 限制为 `StatsigMode=local` 的显式兼容开关。

### A7-3 [P1] Console（DPoP）请求完全绕过出口代理池

- 本项目：`internal/grok/dpop.go:323` — `client := *c.httpClient` / `client.Timeout = c.cfg.GrokRequestTimeout(ProviderConsole)` / `resp, err := doUpstreamHTTP(req, client.Do, …)`；`c.httpClient` 由 `internal/grok/client.go:1536` `util.GetSharedBrowserHTTPClientWithHeaderTimeout(proxyKey, timeout, 0, proxyFunc)` 构造，`proxyKey` 来自全局配置（`internal/grok/client.go:1531-1534`），与 `egress.Manager` 无关；token 交换同样直连：`internal/grok/dpop.go:215` — `resp, err := c.httpClient.Do(req)`。
- 本项目（同一 Console 逻辑的两套出口）：`internal/grok/handler_voice_ws.go:195` — `lease, acquireErr := c.egress.Acquire(ctx, "console", dpopCacheKey(token))`（语音 WS 走 lease），而 `internal/grok/console.go:127` — `return h.webClient().doConsoleDPoPRequest(ctx, token, http.MethodPost, h.consoleURL("responses"), body)` 走全局 client。
- grok2api：`B:backend/internal/infra/provider/console/dpop.go:216` — `applyBrowserHeaders(request, ssoToken, lease)` / `localBefore := time.Now().UTC()` / `response, err := lease.DoDeferredForbidden(request)`；`:342` — `response, err := lease.DoDeferredForbidden(request)`。
- 差异/错误：A 的 `egressScopeForURL`（`internal/grok/client.go:749-758`）能为 `console.x.ai` 映射出 `console` scope，但 Console 代码路径根本不调用 `Acquire`；B 的 Console 每个请求（含 `/v1/dpop/token`）都绑定 lease。
- 影响：即使运维为 console scope 配了专用出口与 FlareSolverr clearance，A 的 Console 流量仍从全局代理/直连出去；出口身份、UA、clearance 与其它通道不一致，upstream 侧看到同一账号在不同 IP/指纹间跳变 → 更容易触发风控。
- 修复：把 `doConsoleDPoPRequestWithHeaders` 与 `fetchDPoPSession` 改为 `lease := c.egress.Acquire(ctx, "console", dpopCacheKey(token))`（未启用出口时回落全局 client），并复用 `mergeCFCookies`/`FeedbackOutcome`。

### A7-4 [P1] Build/CLI 请求被注入浏览器 UA 与 grok.com clearance

- 本项目：`internal/grok/cli.go:441` — `if lease.UserAgent != "" { req.Header.Set("User-Agent", lease.UserAgent) }` / `if lease.CFCookies != "" { mergeCFCookies(req.Header, lease.CFCookies) }`（覆盖 `cliHeaders` 里 `internal/grok/cli.go:89 h.Set("User-Agent", c.userAgent())` 的 `grok-shell/…` 身份）。
- 本项目（出口层无 scope 门禁）：`internal/grok/egress/manager.go:236` — `if cfg.Mode == "flaresolverr" && m.solver != nil { solved, err := m.solver.Solve(ctx, cfg, node.URL) … }`，而 `internal/grok/egress/manager.go:297-306 clearanceConfig()` 固定 `TargetURL: "https://grok.com"`；`Acquire` 对任何 scope 都调用 `resolveFingerprint`（`:112`）。
- grok2api：`B:backend/internal/infra/egress/manager.go:1129` — `userAgent := ""` / `if scope != domain.ScopeBuild { userAgent = strings.TrimSpace(selected.UserAgent) }`；`B:backend/internal/infra/egress/manager.go:1171` — `func usesBrowserClearance(scope domain.Scope) bool { return scope != domain.ScopeBuild && scope != domain.ScopeConsoleAsset }`；`B:backend/internal/infra/provider/cli/egress.go:49` — `if lease.UserAgent != "" { request.Header.Set("User-Agent", lease.UserAgent) }`（Build 的 lease UA 恒空，CLI 身份不被覆盖）。
- 差异/错误：A 在 flaresolverr 模式下为 `cli` scope 也去 grok.com 求解，把解出的浏览器 UA 与 `cf_clearance/__cf_bm` 合并进 `cli-chat-proxy.grok.com` 的请求；B 对 Build scope 既不注入 UA 也不合并 clearance cookie。
- 影响：① `cf_clearance` 是 host 作用域的凭据，跨源发送给 CLI 网关属于凭据外泄；② UA 声明成 Chrome 而其余 CLI 头（`x-grok-client-identifier: grok-shell`、`x-grok-client-version`）声明成 CLI，指纹自相矛盾，正是 CLI 网关侧风控最容易命中的组合。
- 修复：`Manager.Acquire` 增加 scope 门禁，`cli`/`build` scope 不进入 `resolveFingerprint`；`CLIClient.doCLIRequest` 删除 UA/CF cookie 注入（或仅当显式配置 `grok_cli_use_browser_identity` 时启用）。

### A7-5 [P1] 出口亲和是常量，所有账号共用一个出口与一份 clearance

- 本项目：`internal/grok/client.go:763` — `func (c *Client) egressAffinity(reqURL string) string { host := strings.ToLower(strings.TrimSpace(reqURL)); if strings.Contains(host, "rate-limits") { return "rate-limits" }; return "grok-default" }`；`internal/grok/cli.go:436` — `lease, err := c.egress.Acquire(ctx, "cli", "cli-default")`；指纹由此确定：`internal/grok/egress/manager.go:212` — `return strings.ToLower(strings.TrimSpace(node.Name)) + "|" + strings.ToLower(strings.TrimSpace(affinity))`。
- grok2api：`B:backend/internal/infra/egress/manager.go:461` — `identity := strings.TrimSpace(credential.EgressIdentity)` / `identity = "sso_" + security.HashToken(token)[:32]` / `ctx = WithAccountIdentity(ctx, identity)`；`B:backend/internal/infra/egress/manager.go:400` — `func isolationAccountIdentity(ctx context.Context, scope domain.Scope, affinity string) string { identity := accountFromContext(ctx); if identity != "" { return identity }; … }`；粘性代理与 clearance key 都取自该身份：`B:backend/internal/infra/egress/manager.go:1099-1107`、`:1196-1206 clearanceCacheKey`。
- 差异/错误：A 的 web/CLI 亲和常量 → `pickNode` 的 sticky 键 `scope:app_chat:grok-default` 一旦落定，**所有账号**长期压在同一节点、同一 UA、同一份 `cf_clearance`；B 的亲和来自凭据身份，且 `AcquireCredential` 还会把 SSO/Web/Console 两套映射收敛到同一 `sso_<hash>`。
- 影响：多账号共用出口 IP 与 clearance，上游侧一个账号触发风控即牵连整池；单账号被封后 clearance 连带失效导致全量重新求解（A7-6）；也无法利用多节点分摊。
- 修复：`egressAffinity` 改为账号维度（复用 `dpopCacheKey(token)` 或 `acc.ID`/`EgressIdentity`），并让 `cli` 路径传入 `acc.ID`；`Acquire` 增加 `WithAccountIdentity` 式的显式身份参数。

### A7-6 [P1] clearance 缓存键不含代理/求解器/target，且无版本绑定、无分布式锁、无持久化

- 本项目：`internal/grok/egress/manager.go:212` — `func (m *Manager) fingerprint(node Node, affinity string) string`（仅 node.Name + affinity）；`internal/grok/egress/manager.go:225` — `if ok && !state.invalid && state.cookies != "" && time.Since(state.refreshedAt) < m.refreshInterval() { return state.userAgent, state.cookies, state.version, nil }`（命中即复用，不校验代理/求解器/目标是否变过）。
- 本项目（配置热更丢状态）：`internal/grok/handler.go:99` — `client := New(cfg)` / `cliClient := NewCLIClient(cfg)`（`New` 内 `internal/grok/client.go:78 egress: egress.NewManager(cfg)`），旧 Manager 的 `clearances/health/sticky` 整体丢弃。
- grok2api：`B:backend/internal/infra/egress/manager.go:2040` — `func clearanceFingerprint(cfg ClearanceConfig, proxyURL string) string { value := strings.TrimSpace(cfg.FlareSolverrURL) + "\x00" + clearanceBindingFingerprint(cfg, proxyURL) … }`，`:2045` — `value := strings.TrimRight(strings.TrimSpace(cfg.TargetURL), "/") + "\x00" + strings.TrimSpace(proxyURL)`；新鲜判定要求指纹一致：`B:backend/internal/infra/egress/manager.go:1799` — `state.version == version && state.fingerprint == fingerprint && (state.bindingFingerprint == "" || state.bindingFingerprint == bindingFingerprint) && … now.Sub(state.refreshedAt) < interval`；刷新走分布式锁与持久化：`B:backend/internal/infra/egress/manager.go:1864` — `release, acquired, err := lock.Acquire(ctx, "egress-clearance:"+strconv.FormatUint(node.ID, 10), timeout+clearanceLockGrace)`。
- 差异/错误：A 换了节点代理地址（同名）或换 FlareSolverr 实例后，只要未到 600s TTL 就继续用旧 clearance/旧 UA；`m.version` 只用于失效竞态，不参与新鲜判定；配置热更则把健康与 clearance 全部清零。B 用 `(solver, target, proxy)` 指纹 + `clearanceVersion` 判定，并跨实例加锁、落库复用。
- 影响：换代理/换求解器后 A 持续用旧出口对应的 clearance 请求新出口（必然被 Cloudflare 拒），且拒绝后表现为反复 `egress clearance solve failed; reusing stale clearance`（`internal/grok/egress/manager.go:242`），排障困难；多实例部署下每个实例各解一次，浪费浏览器求解配额。
- 修复：`fingerprint` 改为 hash(`FlareSolverrURL|TargetURL|node.URL|account身份`)，缓存项记录该指纹并在不匹配时视为过期；`NewManager` 支持原地 `UpdateConfig`/`UpdateClearanceConfig`（B 的 `UpdateClearanceConfig` 语义）而不是重建 Handler client。

### A7-7 [P1] TLS/HTTP2 指纹：库常量 Chrome hello + 强制 h2，未按 UA 版本映射

- 本项目：`internal/util/browser_transport.go:79` — `http2: &http2.Transport{ AllowHTTP: false, DialTLSContext: func(ctx context.Context, network, addr string, cfg *stdtls.Config) (net.Conn, error) { return dialUTLSHTTP2Context(ctx, network, addr, cfg, proxyFunc) }, TLSClientConfig: &stdtls.Config{ MinVersion: stdtls.VersionTLS12, NextProtos: []string{"h2"} } }`；`:114` — `conn := utls.UClient(rawConn, utlsCfg, utls.HelloChrome_Auto)`；`:119` — `if proto := conn.ConnectionState().NegotiatedProtocol; proto != http2.NextProtoTLS { conn.Close(); return nil, fmt.Errorf("browser http2: unexpected ALPN protocol %q", proto) }`；`:26` — `if req != nil && strings.EqualFold(req.URL.Scheme, "https") { return rt.http2.RoundTrip(req) }`（HTTPS 只走 h2，无 HTTP/1.1 回退）。
- 本项目（UA 与 hello 无关联）：`internal/grok/egress/ua.go:12-18` 轮换 UA 含 `Chrome/148`、`Chrome/147`、`Chrome/146`、`Chrome/145` 与一条 Windows UA，而 hello 选择是常量。
- grok2api：`B:backend/internal/infra/egress/tlsclient.go:98` — `func browserProfile(userAgent string) profiles.ClientProfile { match := chromeMajorPattern.FindStringSubmatch(strings.TrimSpace(userAgent)); if len(match) == 2 { if profile, ok := profiles.MappedTLSClients["chrome_"+match[1]]; ok { return profile } … for _, candidate := range []int{146, 144, 133, 131, 124, 120, 117} { … } } return profiles.Chrome_146 }`；`:73` — `tlsclient.WithClientProfile(browserProfile(userAgent))`；HTTP/2 健康探测独立成层：`B:backend/internal/infra/buildtransport/http2.go:11` — `IdleConnTimeout = 30 * time.Second` / `HTTP2ReadIdleTimeout = 20 * time.Second` / `HTTP2PingTimeout = 10 * time.Second`，`:32` — `h2.ReadIdleTimeout = HTTP2ReadIdleTimeout; h2.PingTimeout = HTTP2PingTimeout`。
- 差异/错误：A 的 ClientHello 与 `Sec-Ch-Ua`/UA 声明的 Chrome 版本无任何绑定（hello 取库内"最新 Chrome"，UA 可以声称 145 或 Windows），且 HTTP/2 面（SETTINGS/伪头顺序）完全是 `golang.org/x/net/http2` 默认，没有任何配置入口；ALPN 若协商出 http/1.1 直接报错而非回退。B 由 UA 大版本查表、带最近邻回退与固定兜底 `Chrome_146`，tls-client 的 `ClientProfile` 同时覆盖 JA3 与 h2 指纹，并对 Build 传输开启 h2 PING。
- 影响：UA 与 TLS 指纹错配是 Cloudflare/xAI 侧最常用的关联特征；A 的 `http2.Transport` 也没有 `ReadIdleTimeout`，半死连接要等请求落到上面才暴露（B 已用 PING 主动探活）。
- 修复：`dialUTLSHTTP2Context` 改为按 `c.userAgent()` 选择 hello（`utls.HelloChrome_*` 映射 + 最近邻），或直接引入 `bogdanfinn/tls-client` 与 B 对齐；`http2.Transport` 设置 `ReadIdleTimeout`/`PingTimeout`；ALPN 非 h2 时回退 HTTP/1.1 而非报错。

### A7-8 [P1] Web chat 从不使用 mgw WebSocket；出口启用时 WS 路径直接不可用

- 本项目：`internal/grok/mgw_websocket_transport.go:47` — `func (t *appChatFallbackTransport) Chat(...) { if t != nil && t.mgw != nil { if _, err := t.mgw.fromLegacyPayload(token, payload); err != nil && !errors.Is(err, errMGWRequiresExplicitRequest) { return nil, err } } … return t.rest.Chat(ctx, client, token, payload) }`，而 `:79 fromLegacyPayload` 恒返回 `errMGWRequiresExplicitRequest` → 永远走 REST；`:94` — `if t.client.egress != nil && t.client.egress.Enabled() { return nil, fmt.Errorf("grok mgw websocket unavailable: egress websocket lease is not supported") }`；`Open` 在仓内无生产调用者（仅 `internal/grok/mgw_websocket_transport_test.go:113`）。
- 本项目（拨号器无浏览器指纹）：`internal/grok/mgw_websocket_transport.go:104` — `dialer := websocket.Dialer{HandshakeTimeout: mgwHandshakeTimeout, Proxy: mgwProxyFunc(t.client)}`（`gorilla/websocket` + 标准 crypto/tls）。
- grok2api：`B:backend/internal/infra/provider/web/gateway.go:120` — `if options.deferForbidden { connection, handshake, dialErr = lease.DialWebSocketDeferredForbidden(requestCtx, endpoint, gatewayHeaders(origin, userID, token, lease), gatewayHandshakeTimeout) } else { connection, handshake, dialErr = lease.DialWebSocket(...) }`；`B:backend/internal/infra/egress/tlsclient.go:41` — `dialer := &websocket.Dialer{ HandshakeTimeout: handshakeTimeout, NetDialTLSContext: l.browser.inner.GetTLSDialer(), NetDialContext: l.browser.inner.GetDialer().DialContext }`，`:47` — `if err == nil || !l.proxyPool || attempt >= proxyPoolRetryLimit || !safeProxyConnectionFailure(...)`，`:51` — `if invalidateForbidden && response != nil && response.StatusCode == http.StatusForbidden && l.clearanceManager != nil && l.clearanceKey != "" { l.clearanceManager.invalidateClearanceKey(l.clearanceKey, l.client) }`；B 的 Web chat 主路径即 `B:backend/internal/infra/provider/web/chat.go:421` — `func (a *Adapter) openChat(...) { return a.openGatewayChat(...) }`。
- 差异/错误：A 的 REST-only 路径把 Web chat 暴露在 B 有意规避的 REST 反爬面上，且 mgw WS 与出口互斥（要么没有指纹，要么直接拒绝）；B 的 WS 复用 lease 的浏览器 TLS/UA/cookies，具备代理池重试与 403 clearance 失效。
- 影响：A 无法用 WS 通道规避 REST 侧反爬/clearance 抖动；`egress` 与 mgw 的组合在 A 里是"配置后功能消失"而非降级。
- 修复：把 mgw `Open` 接入 chat 主路径，并在 lease 上实现 `DialWebSocket`（把 `browserLikeRoundTripper` 的 dialer/TLS 暴露给 gorilla 的 `NetDialTLSContext` + `Proxy`），让 WS 与 REST 共享同一 clearance/UA。

### A7-9 [P1] 无流内反爬识别：不重签 statsig、不重试、不给节点反馈

- 本项目：`internal/grok/util_media.go:658` — `for _, path := range [][]string{ {"userResponse", "streamErrors"}, {"modelResponse", "streamErrors"}, {"streamErrors"}, {"errors"} } { if v := valueAtPath(response, path...); v != nil { if s := diagnosticValueSummary(v); s != "" { out = append(out, strings.Join(path, ".")+"="+s) } } }`（`streamErrors` 仅进诊断文本）；全仓无 "anti-bot"/`code == 7` 分类：`grep -rEn 'anti-bot|streamErrors' internal/` 仅命中 `internal/grok/egress_classify.go:191` 的 HTTP body 关键字与本处诊断。
- grok2api：`B:backend/internal/infra/provider/web/chat.go:1133` — `code, _ := numberAsInt(value["code"]); if code == 7 || strings.Contains(strings.ToLower(message), "anti-bot") { return fmt.Errorf("%w: %s", errWebAntiBot, message) }`；`:319` — `if statsigTarget != "" && errors.Is(preflightErr, errWebAntiBot) && attempt == 0 && a.invalidateSignedStatsig(http.MethodPost, statsigTarget)`；`:375` — `func (a *Adapter) feedbackAntiBot(ctx, lease, statsigTarget) { if statsigTarget != "" { a.invalidateSignedStatsig(...) }; a.egress.Feedback(context.WithoutCancel(ctx), lease.NodeID, http.StatusForbidden, nil) }`。
- 差异/错误：B 对流内反爬做"失效签名 → 同请求重试一次 → 出口节点记 403（触发 clearance 失效、健康降级）→ 返回 `anti_bot_rejected`"；A 只把 `streamErrors` 写进诊断并把该次请求当普通上游错误返回。
- 影响：反爬命中后 A 不会自愈（不重签 statsig、不淘汰 clearance、不标记节点），后续请求继续打在已被判定的会话上，形成持续 403/空响应。
- 修复：在流解析层（`internal/grok/util_media.go` + `grok2api_sse.go`）增加 `code==7 || contains("anti-bot")` 分类，返回独立错误类型；上层收到后 `InvalidateAffinityClearance` + `FeedbackAffinityOutcome(OutcomeChallenge)` 并重试一次。

### A7-10 [P1] 节点健康无探测/无持久化/不淘汰连接池，冷却固定 30s

- 本项目：`internal/grok/egress/manager.go:23-24` — `healthSkipThreshold = 0.2` / `nodeCooldown = 30 * time.Second`；`:204` — `func (m *Manager) degradedLocked(name string, now time.Time) bool { score := m.health[name]; if score >= healthSkipThreshold || score == 0 { return false }; return now.Before(m.unhealthy[name]) }`；`:336` — `case OutcomeTransportError, OutcomeServerError, OutcomeChallenge: m.health[nodeID] = score*0.5 - 0.1; m.unhealthy[nodeID] = time.Now().Add(nodeCooldown)`（健康仅存内存 map `:46 health map[string]float64`，无落库、无探针、无 `CloseIdleConnections`）。
- grok2api：`B:backend/internal/infra/egress/manager.go:1697` — `case status == http.StatusForbidden: … value.FailureCount++; value.Health = max(0.05, value.Health*0.7); value.CooldownUntil = nil; value.LastError = "anti-bot rejection" … stale = m.invalidateClientLocked(nodeID)`；`:1707` — `case transportErr != nil: … cooldown := min(10*time.Minute, 30*time.Second*time.Duration(1<<min(value.FailureCount-1, 4)))`；`:1727` — `if err := stateRepository.UpdateEgressNodeHealth(ctx, value.ID, value.Health, value.FailureCount, value.CooldownUntil, value.LastError); err == nil { m.invalidateNodes(value.Scope); if transportErr != nil { m.scheduleFailureProbe(value) } }`；探针实现 `B:backend/internal/infra/egress/manager.go:268 scheduleFailureProbe` / `:498 ProbeEgressNode`（IPv4+IPv6 双栈出口 IP 校验）。
- 差异/错误：A 的降级窗口固定 30s、无指数退避、失败后不重建该节点的客户端连接池、健康分不落库（重启/热更即清零）、也没有任何主动探活手段把节点恢复到池中；B 有 FailureCount、指数冷却上限 10min、LastError、健康落库、失效并关闭该节点客户端、后台探针完成后刷新节点快照。
- 影响：A 在"节点网络抖动"场景要么反复回到坏节点（30s 后无条件恢复用），要么把连到坏节点的 keep-alive 连接复用（可能携带坏出口的 TLS 会话）；多实例无法共享节点健康。
- 修复：健康状态持久化 + 失败计数与指数冷却；降级时 `client.CloseIdleConnections()` 并按 node 维度淘汰共享 client；增加启动/失败后的出口 IP 探针（B 的 `ProbeEgressNode`）。

### A7-11 [P2] 节点失败指标统计反了：首次降级不计入

- 本项目：`internal/grok/egress/manager.go:326` — `wasDegraded := score < healthSkipThreshold && score != 0`；`:341` — `m.unhealthy[nodeID] = time.Now().Add(nodeCooldown); if wasDegraded { recordNodeFailure(m.scopeForNodeLocked(nodeID), outcomeReason(outcome)) }`（首次失败时 `score == 0` → `wasDegraded == false` → 不计数）。
- 本项目（成功分支写作对照，`!wasDegraded` 才是状态跃迁）：`internal/grok/egress/manager.go:332` — `if wasDegraded { delete(m.unhealthy, nodeID); recordNodeRecovery(m.scopeForNodeLocked(nodeID)) }`。
- grok2api：`B:backend/internal/infra/egress/manager.go:1699` — `value.FailureCount++; value.Health = max(0.05, value.Health*0.7)`，`:1727` 落库 `UpdateEgressNodeHealth(ctx, value.ID, value.Health, value.FailureCount, value.CooldownUntil, value.LastError)`。
- 差异/错误：`grok_egress_node_failures_total`（`internal/grok/egress/metrics.go:21`）只在"已经降级"的节点再次失败时自增，节点从健康→降级的那一次事件永不计数；`node_recoveries_total` 反而记的是真实的跃迁，两份指标口径不一致。
- 影响：出口审计/告警漏掉全部"首次故障"，运维按该指标看板会低估节点故障率；与 B 的 `FailureCount` 逐次累加语义相反。
- 修复：失败分支改为 `if !wasDegraded { recordNodeFailure(...) }`（或无条件计数但在 `wasDegraded` 时使用独立的 `node_failures_degraded_total`）。

### A7-12 [P2] Cloudflare Cookie 白名单少收 `_cfuvid`/`cf_chl_*`，且不去重不校验长度

- 本项目：`internal/grok/egress/flaresolverr.go:140-142` — `switch strings.ToLower(name) { case "cf_clearance", "__cf_bm": kept = append(kept, part) }`（`SanitizeCloudflareCookies` 全函数 `:131-146`，无 `seen`、无长度/控制字符校验、不改写为 `name=value` 规范形）。
- grok2api：`B:backend/internal/application/egress/service.go:1397` — `if lower != "cf_clearance" && lower != "__cf_bm" && lower != "_cfuvid" && !strings.HasPrefix(lower, "cf_chl_") { continue }`，`:1400` — `if _, exists := seen[lower]; exists { continue }`，`:1404` — `if cookieValue == "" || len(cookieValue) > maxCloudflareCookieBytes || strings.IndexFunc(cookieValue, func(character rune) bool { return character < 0x20 || character == 0x7f }) >= 0 { continue }` / `allowed = append(allowed, lower+"="+cookieValue)`。
- 差异/错误：A 丢弃 `_cfuvid`（Cloudflare 设备指纹 cookie）与整套 `cf_chl_*`（挑战态 cookie），也不去重、不限制单 cookie 长度、不拒绝含控制字符的值。
- 影响：clearance 复用时缺少 `_cfuvid`/`cf_chl_*` 会让 Cloudflare 认为会话不完整，容易二次挑战；求解器返回超长/含控制字符 cookie 时会把畸形 `Cookie` 头送到上游（B 直接丢弃）。
- 修复：白名单与 B 对齐（`cf_clearance`/`__cf_bm`/`_cfuvid`/`cf_chl_*`），加 `seen` 去重、值长度上限与控制字符过滤。

### A7-13 [P2] 会话身份探测不走出口，且请求头集与场景不符

- 本项目：`internal/grok/sessionidentity.go:108` — `req.Header = c.headers(token)`（即 `baseHeaders`），`:110` — `resp, err := c.httpClient.Do(req)`；`baseHeaders`（`internal/grok/client.go:122-138`）包含 `"Content-Type": {"application/json"}`、`"Origin": {"https://grok.com"}`、`"Baggage": {sentry…}`、`"Sec-Ch-Ua": {defaultAppChatSecCHUA}`，且**没有** `Accept-Encoding`。
- grok2api：`B:backend/internal/infra/provider/sessionidentity/session.go:59` — `request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, origin+"/api/auth/session", nil)` / `:63` — `request.Header = browserHeaders(token, origin, lease)` / `:64` — `response, err := lease.Do(request)`；`browserHeaders`（`:133-152`）只设 Accept/Accept-Encoding(`gzip, deflate, br, zstd`)/Accept-Language/Cache-Control/Cookie/Pragma/Priority/Referer/Sec-Fetch-*/User-Agent，并在 `:151` 用 `browserheaders.ApplyChromiumClientHints(value, userAgent)` 按 UA 推导 hints；入口 `:38` — `lease, err := egress.AcquireCredential(ctx, domainegress.ScopeWeb, credential)`。
- 差异/错误：A 用全局 client 直连（出口 IP 与随后的请求不一致），并在 GET 上发送 `Content-Type: application/json`、`Origin`、Sentry `Baggage` 与写死的 Chrome148/macOS hints；B 用同一 lease、不带 Content-Type/Origin/Baggage，且 hints 由 UA 推导。
- 影响：同一账号的身份探测与业务请求出自不同 IP/指纹（上游按 IP 关联会话时是明显异常）；伪造的 Sentry/Client Hints 与实际 UA 不匹配；`baseHeaders` 缺 `Accept-Encoding`，只会得到 Go transport 自动追加的 `gzip`，与 B 三处一致的 `gzip, deflate, br, zstd` 不同，也是可区分的编码能力指纹。
- 修复：`FetchSessionIdentity` 接受 lease（或内部 `Acquire(scope=app_chat, affinity=账号身份)`），并用独立的浏览器 GET 头集（无 Content-Type/Baggage/Origin，补 Accept-Encoding 与按 UA 推导的 hints）。

### A7-14 [P2] CLI/Build 缺 B 的 trace 身份头，且 `x-grok-session-id` 未规范化

- 本项目：`internal/grok/cli.go:82` — `h.Set("Authorization", "Bearer "+accessToken)` / `h.Set("X-XAI-Token-Auth", defaultCLITokenAuth)` / `h.Set("x-grok-client-version", …)` / `h.Set("x-xai-request-id", randomUUID())`；`:238` — `if session, _ := payload["prompt_cache_key"].(string); strings.TrimSpace(session) != "" { headers.Set("x-grok-session-id", strings.TrimSpace(session)); headers.Set("x-grok-conv-id", strings.TrimSpace(session)) }`（原样透传客户端字符串）；全仓无 `traceparent`/`x-grok-req-id`/`x-authenticateresponse`/`x-grok-agent-id`/`x-email` 写入（`grep -rEn 'traceparent|x-grok-req-id|x-authenticateresponse|x-grok-agent-id|x-email' internal/` 无命中）。
- grok2api：`B:backend/internal/infra/provider/cli/adapter.go:617` — `if err := a.applyHeaders(req, request.Credential, accessToken, request.Model, request.PromptCacheKey, true)`；`:1032-1052` — `req.Header.Set("x-authenticateresponse", "authenticate-response")` / `req.Header.Set("x-grok-agent-id", a.agentID)` / `if sessionID != "" { req.Header.Set("x-grok-session-id", sessionID); req.Header.Set("x-grok-conv-id", sessionID) }` / `req.Header.Set("x-grok-req-id", requestID)` / `req.Header.Set("traceparent", "00-"+traceID+"-"+spanID+"-01")`；`:1071` — `func grokSessionID(promptCacheKey string) (string, error) { … if parsed, err := uuid.Parse(key); err == nil { return parsed.String(), nil }; return uuid.NewHash(sha256.New(), uuid.NameSpaceURL, []byte("grok2api:session:"+key), 8).String(), nil }`；非 trace 分支另发 `x-userid`/`x-email`（`:1055`/`:1058`）。
- 差异/错误：B 在 Build 主推理请求上带完整 trace 身份（含 `traceparent`、`x-grok-req-id`、`x-authenticateresponse`、`x-grok-agent-id`），并把任意 `prompt_cache_key` 规范成 UUID 再作为 `x-grok-session-id/x-grok-conv-id`；A 缺这些头，且把客户端原始字符串直接当 session id。
- 影响：A 的 Build 请求在 xAI 侧缺少会话/追踪身份，xAI 的会话亲和与 prompt cache 统计会退化（B 注释明确指出随机/异形 session id 会让 `cached_tokens` 归零）；非 UUID 的 `prompt_cache_key` 可能被上游拒绝或忽略。
- 修复：补 `x-authenticateresponse`/`x-grok-agent-id`/`x-grok-req-id`/`traceparent`（可配置 trace 开关），并按 `grokSessionID` 语义规范化 `prompt_cache_key`。

### A7-15 [P2] `x-cluster` 发送范围过宽（所有 Console 请求）

- 本项目：`internal/grok/dpop.go:319` — `req.Header.Set("x-cluster", "https://us-east-1.api.x.ai")`（紧随 `req.Header = c.consoleHeaders(token)`，对所有 console 端口无条件设置）；`internal/grok/handler_voice_ws.go:235` 同样无条件。
- grok2api：`B:backend/internal/infra/provider/console/dpop.go:336` — `if strings.HasSuffix(request.URL.Path, "/responses") { request.Header.Set("x-cluster", "https://us-east-1.api.x.ai") }`。
- 差异/错误：同一 Console 通道，B 只对 `/responses` 声明集群路由提示，A 对 dpop/token、images、videos、voice 等所有请求都带。
- 影响：非 responses 端点上的多余内部路由头会被上游当成非浏览器客户端特征（真实浏览器只在 responses 流量上带该头），属于可被用于区分的身份信号。
- 修复：与 B 对齐，仅在 `/responses` 路径设置；语音 WS 的 proof 请求同步去掉。

### A7-16 [P2] 403 处理：A 仅在识别为 Cloudflare 挑战时失效 clearance

- 本项目：`internal/grok/client.go:717` — `kind := ClassifyUpstreamResponse(lastStatus, resp.Header, raw)` / `:718` — `if kind == UpstreamErrorCloudflareChallenge { recordUpstreamChallenge("cloudflare"); … lease.InvalidateClearance() … attempt--; continue }`（`UpstreamErrorGenericForbidden` 分支 `:733` 只 `recordGenericForbidden()`，不失效 clearance）。
- grok2api：`B:backend/internal/infra/egress/manager.go:118` — `if invalidateForbidden && err == nil && response != nil && response.StatusCode == http.StatusForbidden { l.InvalidateClearance() }`（默认 `Do` 即 `doRequest(request, true)`，`:94-96`）；分类后再决定是否重试的路径见 `:100 DoDeferredForbidden`。
- 差异/错误：凡是走 lease 的 403，B 默认失效该会话的 clearance（除非调用方显式用 `DoDeferredForbidden` 先分类）；A 需要一个能被 `IsCloudflareChallengeBody`/`CF-Mitigated` 命中的 403 才失效，纯 403（无 body 特征）会继续复用被污染的 clearance。
- 影响：上游返回无特征体的 403 时，A 反复用同一 clearance 重试同一出口（表现为持续 403 但无 clearance 刷新），B 至少会换一次 clearance/节点。
- 修复：把 `Lease.Do` 拆出"403 即失效（可延迟判定）"语义，或在 `doRequest` 里对 403 一律 `InvalidateClearance()` 后依赖上层决定是否重试。

### A7-17 [P2] 固定伪造 Client Hints，与轮换 UA（含 Windows）矛盾

- 本项目：`internal/grok/client.go:46` — `defaultAppChatSecCHUA = "\"Chromium\";v=\"148\", \"Google Chrome\";v=\"148\", \"Not/A)Brand\";v=\"99\""`；`internal/grok/client.go:132-134` — `"Sec-Ch-Ua": {defaultAppChatSecCHUA}, "Sec-Ch-Ua-Mobile": {"?0"}, "Sec-Ch-Ua-Platform": {"\"macOS\""}`；UA 来源可被出口改成 `Chrome/147|146|145` 或 `(Windows NT 10.0; Win64; x64) … Chrome/148`（`internal/grok/egress/ua.go:12-18`，`internal/grok/client.go:648 h.Set("User-Agent", lease.UserAgent)`）。
- grok2api：`B:backend/internal/infra/provider/browserheaders/chromium.go:18` — `func ApplyChromiumClientHints(header http.Header, userAgent string) { … match := chromiumVersionPattern.FindStringSubmatch(userAgent); … header.Set("Sec-Ch-Ua", fmt.Sprintf("\"%s\";v=\"%s\", \"Chromium\";v=\"%s\", \"Not(A:Brand\";v=\"24\"", brand, version, version)) … }`，调用点 `B:backend/internal/infra/provider/console/headers.go:36` / `B:backend/internal/infra/provider/sessionidentity/session.go:151`；Web 路径刻意不伪造：`B:backend/internal/infra/provider/web/headers.go:26` — `// applyAppHeaders 补齐真实浏览器同源 fetch 会携带的稳定请求头，不伪造 Sentry 或 Client Hints。`
- 差异/错误：A 把 Chrome 148 与 `"Not/A)Brand";v="99"`（旧版 GREASE 品牌格式）写死，`Sec-Ch-Ua-Platform` 永远 `"macOS"`，即使实际 UA 是大版本 145–147 或 Windows；B 的 hints 由真实 UA 推导（浏览器品牌顺序 `"Google Chrome"` 优先、GREASE 为 `"Not(A:Brand";v="24"`），且 Web 路径干脆不带 Sentry/Client Hints。
- 影响：UA/UA-CH 互相矛盾是高置信度机器人特征；`Baggage` 里固定的一年前 Sentry release（`internal/grok/client.go:125`）同样与"浏览器会话"矛盾。
- 修复：删除写死的 `Sec-Ch-Ua*`，改为按实际发送的 UA 推导（直接复用 B 的 `ApplyChromiumClientHints` 语义）；`Baggage` 改为可配置或移除。

### A7-18 [P2] 出口/浏览器客户端缓存永不淘汰，且键不含代理 URL（改代理后继续用旧出口）

- 本项目：`internal/grok/egress/manager.go:122` — `poolKey := "egress:" + node.Name + "|" + fingerprint` / `:123` — `client := util.GetSharedBrowserHTTPClientWithHeaderTimeout(poolKey, m.cfg.GrokRequestTimeout(…), 0, proxyFuncForNode(*node))`；`internal/util/browser_transport.go:50` — `cacheKey := "browser|" + sharedHTTPClientCacheKey(proxyKey, timeout) + fmt.Sprintf("|headers=%d", headerTimeout)`，`:52-63` 命中即返回，`:95` — `browserHTTPClientCache.clients[cacheKey] = client`；`clientPool`（`internal/util/http_pool.go:19-22`）只有 `map[string]*http.Client`，无任何删除/TTL/容量逻辑，全仓也没有对这些共享 client 调 `CloseIdleConnections`。
- grok2api：`B:backend/internal/infra/egress/manager.go:209` — `type clientCacheKey struct { nodeID uint64; scope domain.Scope; fingerprint string; accountIdentity string }`；`:37-40` — `clientCacheIdleTTL = 30 * time.Minute` / `clientCacheCleanupInterval = time.Minute` / `maxCachedClients = 4096`；`:344-352` — `for key, cached := range m.clients { if key.scope == domain.ScopeBuild { stale = append(stale, m.evictClientLocked(key, cached)) } } … closeRequestClients(stale)`；`:385-391` 同理在 `UpdateAccountIsolatedConnections` 时整体淘汰。
- 差异/错误：A 的缓存键用 `node.Name` 而非 `nodeID`，也不含代理 URL，因此同名节点改代理地址后仍复用**旧代理**的连接池直到进程重启；节点降级/配置热更都不会关闭这些池（A7-10、A7-6）。B 的键含 nodeID + scope + 指纹 + 账号身份，并有 TTL、容量上限、变更时主动淘汰并关闭。
- 影响：运维改出口地址后 A 的流量仍走旧代理（配置不生效且无任何提示）；长跑进程 client/map 无界增长；坏节点的 keep-alive 连接长期复用。
- 修复：poolKey 使用 `node.URL`（脱敏哈希）参与键；为 `clientPool` 增加 TTL/LRU 与 `Close`；节点降级或配置更新时按 node 维度淘汰并 `CloseIdleConnections()`。
