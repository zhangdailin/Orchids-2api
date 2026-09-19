# 错误契约 / 鉴权 / 校验对比

对比范围：Orchids-2api（以下简称 A）Grok 通道 + 统一 `/v1` 入口，对 chenyme/grok2api（以下简称 B，HEAD 906b9493，B 路径相对 `backend/`）。
结论基于源码逐行阅读；无代码证据的猜测不作为发现。

## 结论摘要

- 最严重的问题是 **A 在非流式与 Anthropic 转发路径上把上游错误正文原样回传客户端**（P0）：`err.Error()` 形如 `grok upstream status=401 node=… body=<上游正文前 4096B>`，B 侧同场景只输出固定脱敏文案。
- A 的推理校验失败走 `http.Error`（纯文本），整条 `/v1/chat/completions`、`/grok/v1/chat/completions`、`/grok/v1/messages` 校验路径都没有 OpenAI 风格 JSON 错误对象；B 每个出口都是 `{error:{message,type,code,param}}`。
- 上游失败 → 客户端状态映射是**系统性反转**：A 把上游 401/403/402/404 翻成 401/429 并暴露“账号会话过期/额度耗尽”，B 把所有凭据类状态（401/402/403）统一脱敏成 503 `upstream_unavailable`。
- “没有可用账号”在 A 是 502（或 401，取决于最后一个错误）且 `error.type="unknown"`，B 是 503 `upstream_unavailable`。
- 限流闸门语义相反：A 阻塞等待最多 60s 再返回**纯文本 503 且无 Retry-After**；B 立即 503 + `Retry-After: 1` + JSON `server_overloaded`。
- A 的原生 Grok 推理端点**没有任何请求体大小上限**（全局无 MaxBytesReader，只有部分语音/媒体端点有限制）；B 全局 32 MiB + 每处理器 MaxBytesReader → 413。
- 请求级超时 A 默认 600s（并发闸门执行超时），B 默认 2h。
- 其余为 P2/P3：Retry-After 不回传、不校验 Content-Type、未知字段容忍度、`/v1/models` 鉴权可被配置关闭、请求 ID 头名不一致、客户端 Key 限制语义（默认 60 RPM / api_key_expired / 无 Billing 上限）。

## 发现

### A8-1 [P0] 上游错误正文（含凭据相邻信息）原样回传客户端

- 本项目（chat 非流式/早失败路径）：`internal/grok/handler_chat.go:410` — `resp, err := h.doChatWithAutoSwitchRebuild(r.Context(), sess, &payload, buildPayload)` / `if err != nil { http.Error(w, err.Error(), http.StatusBadGateway) }`；该 err 由 `internal/grok/client.go:712` 的 `newUpstreamError(lastStatus, headerCopy, []byte(lastBody), leaseNodeID)` 构造，`internal/grok/upstream_error.go:37` 把它渲染为 `b.WriteString(" body=" + e.body)`（`maxUpstreamBodyBytes = 4096`）。
- 本项目（messages 转发路径）：`internal/grok/handler_messages.go:1109` — `func writeAnthropicUpstreamError(w http.ResponseWriter, status int, body string)` → `message := strings.TrimSpace(body)` → `writeAnthropicError(w, status, message)`（`internal/grok/handler_messages.go:103` 用 `rec.body.String()` 调用它）；`internal/grok/console.go:519` 同样输出 `"console response parse error: "+err.Error()`。
- grok2api：`B:backend/internal/transport/http/inference/handler.go:2333` — `status, code, message = upstreamFailure.HTTPStatus, upstreamFailure.Code, upstreamFailure.PublicMessage`，而 `B:backend/internal/application/gateway/failure.go:102` 只允许固定文案 `Code: "upstream_error", PublicMessage: "上游服务返回错误"`，正文永不外泄。
- 差异/错误：A 的 HTTP 错误体里带着上游原始响应（可含 team/user 标识、上游内部错误串、与被拒原因相关的账号信息），状态码则统一被压成 502；B 用 `UpstreamFailure.PublicMessage` 白名单文案。
- 影响：越权信息泄露（上游账号/团队维度信息可被任意持 Key 的下游读取），且下游拿到的是 `text/plain` 而非错误对象，无法编程处理。
- 修复：A 的所有 `http.Error(w, err.Error(), …)` 出口改为 `apperrors.New(category, apperrors.PublicMessage(err.Error()), apperrors.StatusForCategory(category)).WriteResponse(w)`，并在 `grokUpstreamError.Error()` 里去掉 `body=` 段（正文只进 `auditAttemptDiagnostic`）。

### A8-2 [P1] 推理校验失败返回 text/plain，没有 OpenAI 错误对象

- 本项目：`internal/grok/http_helpers.go:51` — `func decodeJSONBody(...)` 内 `http.Error(w, "invalid json", http.StatusBadRequest)`；`internal/grok/handler_chat.go:198` — `if err := req.Validate(); err != nil { http.Error(w, err.Error(), http.StatusBadRequest) }`（同文件 199/221/225/229/238/354 等全部同类）。
- grok2api：`B:backend/internal/transport/http/inference/handler.go:312` — `if json.Unmarshal(body, &request) != nil || strings.TrimSpace(request.Model) == "" { writeOpenAIError(c, http.StatusBadRequest, "invalid_request", "Chat Completions 请求缺少有效 model") }`，`writeOpenAIError` 在 `B:backend/internal/transport/http/inference/handler.go:2285` 统一输出 `{"error":{"message":…,"type":…,"code":…,"param":nil}}`。
- 差异/错误：A 的 `/v1/chat/completions`（grok 模型走 `cmd/server/routes.go:158` 的原生 handler）在“模型不存在 / 参数非法 / body 非法 JSON / 空消息”时返回 `Content-Type: text/plain` 的裸字符串；B 全部返回 JSON 错误对象。
- 影响：OpenAI 兼容客户端 `resp.json()["error"]["message"]` 直接抛解析异常，丢失真实错误信息（属于常见路径，非边角）。
- 修复：把 grok handler 的校验出口统一改走一个 `writeOpenAIError` 等价函数，至少保证 `error.message/type/code` 三字段。

### A8-3 [P1] 上游 401/402/403/404 → 客户端状态映射与参考实现相反

- 本项目：`internal/errors/public.go:24` — `case "quota_exhausted", "rate_limit": return http.StatusTooManyRequests` / `case "auth", "auth_blocked": return http.StatusUnauthorized`；`internal/errors/classify.go:197` — `case HasExplicitHTTPStatus(lower, "403"): return UpstreamErrorClass{Category: "auth_blocked"…}`，`:199` 把上游 404 也归为 `auth_blocked`，于是 404→401。
- grok2api：`B:backend/internal/transport/http/inference/handler.go:2402` — `func isUpstreamCredentialStatus(status int) bool { return status == http.StatusUnauthorized || status == http.StatusForbidden || status == http.StatusPaymentRequired }`，`:2331` — `status, message = http.StatusServiceUnavailable, credentialErrorMessage(code)`（401/402/403 一律 503 脱敏）。
- 差异/错误：同一个上游 401，A 回 401“The upstream account session has expired…”，B 回 503“上游服务暂不可用”；上游 402 在 A 是 429（会被客户端当限流并自动重试），在 B 是 503；上游 403 在 A 是 401；上游 404（模型/端点缺失）在 A 是 401、在 B 原样 404（`B:backend/internal/application/gateway/failure.go:180` 的 default 分支）。
- 影响：下游把“账号池凭据问题”误判为需要重签自己的 Key（401），或把“额度耗尽”误判为限流无脑重试（429），放大上游压力。
- 修复：A 需要按 B 的口径区分“客户端 Key 的问题”和“上游账号池的问题”，后者统一 502/503，不要用 401/429 表达上游账号状态。

### A8-4 [P1] “没有可用账号”的状态码与类型不一致（502/401 + type=unknown）

- 本项目：`internal/handler/stream_handler.go:2900` — `func (h *streamHandler) InjectNoAvailableAccountError(lastErr string, selectErr error)` / `category := apperrors.ClassifyUpstreamError(lastErr).Category`，`:2930` — `h.reportRequestFailure(…)`；非流式分支 `:2852` — `apperrors.New(category, message, apperrors.StatusForCategory(category)).WriteResponse(h.w)`；`internal/errors/public.go:39` — `default: return http.StatusBadGateway`，`:75` — `default: … return "The upstream request failed. Use the request ID to inspect diagnostics."`。`lastErr` 若形如 `… status=401 …`（A8-1 的形态），`HasExplicitHTTPStatus` 会把 category 变成 `auth`，最终给客户端 **401**。
- grok2api：`B:backend/internal/transport/http/inference/handler.go:2340` — `case errors.Is(err, gateway.ErrResponseAccountUnavailable), errors.Is(err, gateway.ErrNoAvailableAccount): status, code = http.StatusServiceUnavailable, "upstream_unavailable"; message = "当前没有可用的上游账号"`。
- 差异/错误：同一种“账号池空/不可用”，A 输出 502 且 `error.type="unknown"`（甚至 401），B 输出 503 `upstream_unavailable`。
- 影响：客户端按 502/401 做的重试与告警策略完全不同；`type:"unknown"` 无法被下游识别。
- 修复：把“选号失败”单列成稳定类别（如 `no_account` → 503 / `upstream_unavailable`），不要复用上游错误分类。

### A8-5 [P1] 全局限流闸门：A 阻塞 60s 后纯文本 503（无 Retry-After），B 立即拒绝并带退避

- 本项目：`internal/middleware/concurrency.go:86` — `waitCtx, cancelWait := context.WithTimeout(r.Context(), waitTimeout)`，`:91` — `if err := cl.sem.Acquire(waitCtx, 1); err != nil { … }`，`:94` — `http.Error(w, "Request timed out while waiting for a worker slot or server busy", http.StatusServiceUnavailable)`（waitTimeout 上限见 `:66` `waitTimeout := 60 * time.Second`）。
- grok2api：`B:backend/internal/transport/http/middleware/concurrency.go:39` — `if g.active >= g.limit { g.mu.Unlock(); c.Header("Retry-After", "1"); c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"error": gin.H{"code": "server_overloaded", …}}) }`。
- 差异/错误：A 让请求先在闸门里排队（最多 60s）再 503，body 是纯文本、无 `Retry-After`、无错误码；B 立即 503 并给出 `Retry-After: 1` 与 `server_overloaded` 机器码。
- 影响：过载时 A 的连接与 goroutine 被排队请求长期占用（雪崩风险），下游无法获知何时重试。
- 修复：改为立即拒绝或严格限制等待窗口，并统一 `Retry-After` + JSON 错误对象。

### A8-6 [P1] Anthropic 信封 `error.type` 恒为 `invalid_request_error`

- 本项目：`internal/grok/handler_messages.go:1101` — `func writeAnthropicError(w http.ResponseWriter, status int, message string)` / `:1105` — `"type": "error", "error": map[string]interface{}{"type": "invalid_request_error", "message": message}`（status 为 500/502/403/429 时同样写 `invalid_request_error`，也没有 `code`）。
- grok2api：`B:backend/internal/transport/http/inference/handler.go:2442` — `func writeAnthropicError(c *gin.Context, status int, errorType, message string, errorCode ...string)` / `:2444` — `if len(errorCode) > 0 && errorCode[0] != "" && errorCode[0] != "upstream_unavailable" { errorPayload["code"] = errorCode[0] }`；调用方按状态选择 `overloaded_error`（`:2355`/`:2375`）、`permission_error`（`:2361`）、`rate_limit_error`（`:2358`）、`not_found_error`（`:2364`）。
- 差异/错误：A 的 `/v1/messages` 在 403（模型白名单）、429（额度）、502（上游失败）时都声称 `invalid_request_error`，客户端会认为“请求参数错、重试无用”；B 的类型与状态一一对应。
- 影响：Anthropic 客户端把上游额度/过载错误当成参数错误，直接放弃而不是重试/换 Key。
- 修复：按 status 派生 `error.type`，并把机器码放进 `error.code`。

### A8-7 [P1] 原生 Grok 推理端点没有请求体大小上限

- 本项目：`internal/grok/http_helpers.go:51` — `func decodeJSONBody(w http.ResponseWriter, r *http.Request, v interface{}) bool { if err := json.NewDecoder(r.Body).Decode(v); err != nil { … } }`（直接读 `r.Body`，无 `http.MaxBytesReader`）；50 MiB 上限只存在于另一条通用通道 `internal/handler/handler.go:129` — `const maxRequestBytes = 50 * 1024 * 1024`，`:370` — `r.Body = http.MaxBytesReader(w, r.Body, maxRequestBytes)`；`cmd/server/main.go:286` 的中间件链里没有任何体积限制。
- grok2api：`B:backend/internal/transport/http/inference/handler.go:301` — `c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, h.maxBodyBytes)`，`:308` — `writeOpenAIError(c, http.StatusRequestEntityTooLarge, "request_too_large", "请求体超过限制")`；并且全局中间件 `B:backend/internal/transport/http/middleware/request.go:79` — `if c.Request.Body != nil && limit > 0 { … c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, effective) }`（默认 32 MiB，`B:backend/internal/infra/config/config.go:877`）。
- 差异/错误：A 的 `/v1/chat/completions`、`/grok/v1/chat/completions`、`/grok/v1/messages`、`/grok/v1/responses` 对 body 无上限（`/v1/messages` 走 grok 原生时同样无上限），B 每层都有上限并被强制为 413。
- 影响：单个持 Key 客户端可提交任意大小的 JSON 触发 OOM/长 GC，属于拒绝服务面。
- 修复：在 `decodeJSONBody` 内（或路由包装层）统一套 `http.MaxBytesReader` 并在 `*http.MaxBytesError` 时返回 413 JSON。

### A8-8 [P2] 请求执行超时：A 默认 600s，B 默认 2h

- 本项目：`internal/config/config.go:332` — `cfg.RequestTimeout = boundedDefault(cfg.RequestTimeout, 600, 86400)` / `:337` — `cfg.ConcurrencyTimeout = boundedDefault(cfg.ConcurrencyTimeout, cfg.RequestTimeout, 86400)`；`internal/middleware/concurrency.go:121` — `execCtx, cancelExec := context.WithTimeout(r.Context(), cl.timeout)`（`cl.timeout` 即 ConcurrencyTimeout，默认 600s）。
- grok2api：`B:backend/internal/infra/config/config.go:880` — `RequestTimeout: Duration(2 * time.Hour)`；`B:backend/internal/transport/http/middleware/request.go:66` — `ctx, cancel := context.WithTimeout(c.Request.Context(), duration)`。
- 差异/错误：长推理/长工具链在 A 上 10 分钟即被上下文取消（且取消后的表现是流被截断，不是协议错误事件），B 允许 2 小时。
- 影响：A 上大模型长回答/多轮工具调用会被静默截断为“看起来正常的短答案”。
- 修复：明确区分“闸门等待超时”和“执行超时”，把执行超时提到与 B 同量级或可配置为 0（不限）。

### A8-9 [P2] Retry-After 不随 429 回传

- 本项目：`internal/handler/stream_handler.go:2852` — `apperrors.New(category, message, apperrors.StatusForCategory(category)).WriteResponse(h.w)`，`internal/errors/errors.go:30` — `w.Header().Set("Content-Type", "application/json"); w.WriteHeader(e.HTTPStatus)`（无 `Retry-After`）；上游 `Retry-After` 只被用于账号调度（`internal/grok/rate_limiter.go:170`）与诊断（`internal/grok/attempt_diagnostics.go:130`）。A 仅在客户端 Key 层面的 429 上写死退避（`internal/middleware/session.go:56`/`:72`/`:158`）。
- grok2api：`B:backend/internal/transport/http/inference/handler.go:2335` — `if !isUpstreamCredentialStatus(upstreamFailure.HTTPStatus) && upstreamFailure.RetryAfter > 0 { c.Header("Retry-After", strconv.FormatInt(max(1, int64(upstreamFailure.RetryAfter.Round(time.Second)/time.Second)), 10)) }`；选号失败路径同样在 `:2435` 写 `Retry-After`。
- 差异/错误：A 的“账号全体 429”响应没有可解析的退避时间；B 一定带 `Retry-After`。
- 影响：下游只能指数退避猜测或立即重试，放大上游限流。
- 修复：从 `RateLimitMetadata`/上游头解析出剩余冷却时间并在 429/503 上回写 `Retry-After`。

### A8-10 [P2] 不校验 Content-Type（无 415）

- 本项目：`internal/grok/http_helpers.go:52` — `if err := json.NewDecoder(r.Body).Decode(v); err != nil {`（`decodeJSONBody` 完全不读 `Content-Type`，grok 全部推理端点复用它）；415 只出现在语音/媒体/上传端点（如 `internal/grok/handler_voice.go:201`）。
- grok2api：`B:backend/internal/transport/http/inference/handler.go:1180` — `mediaType, _, err := mime.ParseMediaType(c.GetHeader("Content-Type")); return err == nil && strings.EqualFold(mediaType, "application/json")`，`:302` — `if !isJSONRequest(c) { writeOpenAIError(c, http.StatusUnsupportedMediaType, "invalid_request", "Chat Completions only supports application/json") }`。
- 差异/错误：`POST /v1/chat/completions` 带 `Content-Type: text/plain` 时 A 正常解析执行，B 返回 415 JSON。
- 影响：A 会接受来自表单/文本上下文的任意 JSON，增大 CSRF 类误用面，且下游拿不到一致的 415 契约。
- 修复：与 B 对齐，非 `application/json` 直接 415。

### A8-11 [P2] 未知字段容忍度：B 对视频请求显式拒绝未知字段

- 本项目：`internal/grok/http_helpers.go:52` — `json.NewDecoder(r.Body).Decode(v)`，全项目无 `DisallowUnknownFields`（`grep` 无命中），未知字段一律忽略。
- grok2api：`B:backend/internal/transport/http/inference/handler.go:1187` — `if disallowUnknown { decoder.DisallowUnknownFields() }`，调用点 `:729` — `if err := decodeSingleJSON(c.Request.Body, &request, true); err != nil { writeOpenAIError(c, http.StatusBadRequest, "invalid_request", label+" JSON 请求无效: "+err.Error()) }`（仅视频创建路径传 `true`）。
- 差异/错误：A 的 `/grok/v1/videos` 系列对拼错的字段名静默忽略（例如 `duratoin`），B 400 并指出字段。
- 影响：边角（P2）：A 上参数拼写错误会退化成默认行为而非明确报错。
- 修复：视频/图片等结构化参数端点对齐 `DisallowUnknownFields`。

### A8-12 [P2] `/v1/models` 鉴权可被配置整体关闭

- 本项目：`cmd/server/routes.go:92` — `registerWithPrefixes(mux, allPrefixes, "/models", inferenceAuth(h.HandleModels))`，而 `inferenceAuth`（`:53`）的 enabled 回调是 `currentConfig().InferenceAuthEnabled()`；`internal/middleware/session.go:134` — `if enabled != nil && !enabled() { ctx := context.WithValue(r.Context(), apiKeyFingerprintContextKey{}, "anonymous"); next(w, r.WithContext(ctx)); return }`；`internal/config/config.go:508` — `func (c *Config) InferenceAuthEnabled() bool { return c == nil || c.InferenceAuth == nil || *c.InferenceAuth }`。
- grok2api：`B:backend/internal/transport/http/server.go:190` — `v1.Use(middleware.ClientAuth(deps.ClientKeys))`（`/v1` 组无开关），`B:backend/internal/transport/http/middleware/auth.go:94` — `raw, ok := bearerToken(c.GetHeader("Authorization"))` → 失败即 `writeOpenAIError(c, clientErrorStatus(err), …)` 401。
- 差异/错误：A 只要 `inference_auth_enabled=false`，`/v1/models` 与全部推理端点（含统一前缀 `/v1`）即匿名开放；B 恒要求客户端 Key。
- 影响：部署方一个配置项即可让统一入口变成开放代理；审计中这是与参考实现最直接的鉴权边界差异。
- 修复：至少让“关闭鉴权”只作用于显式声明的公开端点，或对 `/v1` 统一前缀强制鉴权并单独暴露只读模型列表。

### A8-13 [P2] 请求 ID：头名不一致，且 A 的 HTTP 错误体不含 request_id

- 本项目：`internal/middleware/trace.go:49` — `w.Header().Set(DiagnosticRequestIDHeader, requestID)` / `:60` — `w.Header().Set(TraceIDHeader, traceID)`（`TraceIDHeader = "X-Trace-ID"`，`DiagnosticRequestIDHeader = "X-Orchids-Request-ID"`，全项目没有设置 `X-Request-ID` 响应头）；`internal/errors/errors.go:18` — `json.Marshal(map[string]interface{}{"type": "error", "error": map[string]string{"type": e.Code, "message": e.Message}})` 不含 request_id。
- grok2api：`B:backend/internal/transport/http/middleware/request.go:29` — `c.Set(RequestIDKey, requestID)` / `:30` — `c.Header("X-Request-ID", requestID)`，并且在每个处理器里以 `requestID, _ := c.Get(middleware.RequestIDKey)` 传入 gateway（`B:backend/internal/transport/http/inference/handler.go:322`）。
- 差异/错误：按 OpenAI/常见网关约定读取 `X-Request-ID` 的下游在 A 上拿不到任何值；A 只在 SSE 内联错误帧（`internal/grok/http_helpers.go:144` 的 `"request_id": requestID`）里给 ID，HTTP 错误响应无 ID。
- 影响：下游报障时无法把一次失败对应到 A 的审计日志（A 的审计日志键是 `middleware.GetRequestID(ctx)`）。
- 修复：A 同时回写 `X-Request-ID`，并在 HTTP 错误 JSON 中附 `request_id`。

### A8-14 [P2] 客户端 Key 限制语义与错误码：默认 60 RPM、过期码、无计费上限

- 本项目：`internal/store/store.go:883` — `rpm := key.RPMLimit; if rpm <= 0 { rpm = 60 }`，`:891` — `if !allowed { return nil, ErrApiKeyRateLimited }`；`internal/middleware/session.go:154` — `case APIKeyDenialExpired: writeAPIKeyError(w, http.StatusUnauthorized, "API key has expired", APIKeyDenialExpired)`（`APIKeyDenialExpired = "api_key_expired"`），`:158` — `w.Header().Set("Retry-After", "60")`。
- grok2api：`B:backend/internal/application/clientkey/service.go:448` — `if value.RPMLimit > 0 { … if !allowed { return … ErrRateLimited } }`（无默认 RPM）；`:435` — `if !value.IsAvailable(now) { return … ErrInvalidKey }`（过期与禁用同归 401 `invalid_api_key`，见 `B:backend/internal/transport/http/middleware/auth.go:139`）；`:442` — `if value.BillingLimitUSDTicks > 0 { … return … ErrBillingLimit }` → 429 `billing_limit_exceeded`（`auth.go:137`）；`:432` — `if value.InternalKind != "" { return … ErrInvalidKey }`。
- 差异/错误：A 给未配置 RPM 的 Key 强加 60 RPM；A 无用量/计费上限概念，也无“内部 Key 不得用于外部鉴权”的判定；B 不设默认 RPM、把内部 Key 与过期 Key 都判为 `invalid_api_key`、并额外有 429 `billing_limit_exceeded`。
- 影响：从 A→B 迁移的 Key 在 A 上会意外撞 60 RPM；A 缺少内部身份的拒绝分支，属于潜在越权面（`[待验证]` 是否真的存在内部 Key 行 —— `internal/store/store.go:309` 的 `ApiKey` 只有 `ID/Name/KeyHash/KeyFull/KeyPrefix/KeySuffix/Enabled/AllowedModels/RPMLimit/MaxConcurrent/ExpiresAt/…`，没有 `InternalKind`，也没有计费字段，`grep` 无命中，因此该子项只作为能力缺口，不断言已有绕过）。
- 修复：A 去掉隐式 60 RPM 默认值或让其显式可见；补齐内部身份拒绝与用量上限的错误码分支。

### A8-15 [P3] 模型白名单 403 的 `error.type` 与字段集合不同

- 本项目：`internal/grok/http_helpers.go:64` — `w.WriteHeader(http.StatusForbidden)` / `:66` — `"error": map[string]interface{}{"message": "API key is not allowed to use model " + …, "type": "permission_error", "code": "model_not_allowed"}`（无 `param`）。
- grok2api：`B:backend/internal/transport/http/inference/handler.go:2306` — `case errors.Is(err, clientkeyapp.ErrModelNotAllowed): status, code = http.StatusForbidden, "model_not_allowed"`，经 `:2285` 的 `writeOpenAIError` 输出 `type` 为 `invalid_request_error` 且含 `"param": nil`。
- 差异/错误：同为 403 `model_not_allowed`，A 的 `type` 是 `permission_error`（Anthropic 风格），B 是 `invalid_request_error`。
- 影响：只按 `error.type` 分派的客户端会走不同分支；`internal/handler/handler.go:389` 的通用通道也用 `permission_error`，与 B 不一致。
- 修复：统一 OpenAI 语义的 `type`，把 `param` 补上。

### A8-16 [P2] SSE 已刷头后的错误帧形状不同

- 本项目：`internal/grok/http_helpers.go:139` — `payload := map[string]interface{}{"error": map[string]interface{}{"message": …, "type": …, "code": …, "request_id": requestID}}`（**没有顶层 `"type":"error"`**），`:147` — `writeSSEBytes(w, "error", encodeJSONBytes(payload))`；Responses 协议失败帧见 `internal/grok/handler_responses_store.go:414` — `failure, _ := json.Marshal(map[string]interface{}{"type": "response.failed", "response": map[string]interface{}{"id": …, "object": "response", "status": "failed", "model": …, "error": …}})`，`:427` 之后还会补 `[DONE]`。
- grok2api：`B:backend/internal/transport/http/inference/handler.go:1522` — `json.Marshal(map[string]any{"type": "error", "error": map[string]any{"code": code, "message": message, "type": "server_error"}})` → `:1533` — `return []byte("data: " + string(payload) + "\n\ndata: [DONE]\n\n")`；Responses 用 `response.failed` 且带 `created_at/completed_at/output/status` 完整信封（`:1548`），且失败帧后不再补 `[DONE]`。
- 差异/错误：A 的 Chat SSE 错误帧缺顶层 `type`，按 `data.type === "error"` 判定的客户端不会识别；A 的 `response.failed` 缺 `created_at/completed_at/output` 字段，且额外发 `[DONE]`。
- 影响：流中途失败时下游可能把错误帧当成普通 chunk 或等待永不出现的结束事件（本次对比区外，但对客户端正确性影响明确）。
- 修复：SSE 错误帧补 `"type":"error"`；Responses 失败信封补齐必需字段，并与 B 对齐是否追加 `[DONE]`。
