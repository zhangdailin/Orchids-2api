# 流式 / SSE 语义对比

## 结论摘要
- 本项目的流式链路是「上游（Web/Console/Build）→ 内部 OpenAI Chat → 下游协议转换」，B 是「上游 Responses → 下游 Chat/Messages 转换 + 原生 Responses 透传」。两者共有的 SSE 编解码器（`compatibleSSEEvent` / `consumeCompatibleSSE`）逐字节移植正确（多行 `data:`、`id:`/`retry:`/注释、BOM、8 MiB 单事件上限、短写检测都在），差异集中在**编解码之外的生命周期与守卫层**。
- 最严重两处：① 上游重复帧（doom loop）在本项目中**完全没有**任何检测，而重复 delta 又会被语义 idle 判定为「有效生成」从而不断续命，形成零成本无限流；B 有 128/256 阈值 + 上游 `response.doom_loop_check` 私有事件双重防护。② 下游写侧没有任何写超时、写错误被整体丢弃，而移植过来的注释与不变量明确依赖「客户端写超时」这一并不存在的机制。
- 其余为 idle 策略层（语义 idle 被套用到全部通道、超时错误无专用分类、缺 `TimedOut()`）、原生 Responses 透传语义（强制追加 `[DONE]`、私有控制事件外泄、失败信封字段缺失）、错误帧与头部形状（`event: error` vs data-only、缺 `X-Accel-Buffering`、桥接路径丢弃内层已提交头）、Anthropic 流式 usage 形状。
- 没有任何 keepalive/heartbeat 差异：两边都不在下游推理流中定期发送心跳/注释帧（已在两侧全仓 grep 确认），故不作为发现。
- 漂移说明：以下 12 项**均不是** 44a390b8→906b9493 漂移造成的，都是本项目移植时的遗漏。已用 `git log --oneline 44a390b8..906b9493 -- <streaming files>` 逐文件核对：`conversation/stream.go` 之外零改动，`stream.go` 的 2 个提交（ca392e68、8641a782）只改 reasoning replay 记录/合并函数，与本报告引用的行无关；B 侧被引用的行为在 44a390b8 已存在（如 `git show 44a390b8:backend/internal/infra/provider/conversation/stream.go` 第 19-28、821-838、843 行）。
- 合计 12 项：P0×0、P1×2、P2×7、P3×3。

## 发现

### A2-1 [P1] 上游重复帧（doom loop）零检测；重复 delta 反而让语义 idle 永不触发
- 本项目：`internal/grok/console_stream.go:78-84` — `func readResponseSSE(reader io.Reader, consume func(string, string) error) error { return consumeCompatibleSSE(reader, func(event compatibleSSEEvent) error { if !event.HasData() { return nil }; return consume(event.Event, string(event.Data())) }) }`
  以及 `internal/grok/grok2api_streamidle.go:156-157` — `if n > 0 && !r.finished && r.detector.Observe(buffer[:n]) { r.remaining = r.idle`
  （全仓 `grep -rniE "doom|loop detect|OutputLoop|repeat" internal/` 在流式路径上零命中，命中项仅为 `client.go:1221` 分页注释、`handler_images.go:328` 图片数量注释、`tool_call.go:168` 的 `strings.Repeat`；原生 Responses 路径同样没有跟踪器，见 `internal/grok/handler_responses_store.go:342-365`）
- grok2api：`B:backend/internal/infra/provider/conversation/stream.go:1014-1016` — `t.contentRepeatCount++; if t.contentRepeatCount > contentDoomLoopThreshold { return fmt.Errorf("%w (repeated content delta %d times)", neterror.ErrUpstreamOutputLoop, t.contentRepeatCount) }`
  同处 `B:backend/internal/infra/provider/conversation/stream.go:1030-1032`（reasoning 阈值 256）与 `B:.../stream.go:1039-1050`（`guardResponseStream` 对原生 Responses 流同样挂 `streamRepeatTracker`：第 1044-1050 行 `tracker := streamRepeatTracker{}` / `err := consumeSSE(io.TeeReader(source, writer), ... tracker.trackEvent(typeName, root)`）
- 差异/错误：B 在协议转换、缓冲、stop filter **之前**跟踪可见/推理 delta（`stream.go:297`），Content 连续同一 delta 超过 128 次、Reasoning 超过 256 次即以 `neterror.ErrUpstreamOutputLoop` 终止并映射为 `upstream_output_loop`；本项目两条上游读取路径（`readResponseSSE` 与原生 `consumeCompatibleSSE`）都把帧直接交给消费者，没有任何重复计数。更糟的是本项目的语义 idle 把 `response.output_text.delta` 等视为「有效生成」并重置截止时间，因此重复帧构成的死循环正好是 idle 唯一永远不会杀死的形态。
- 影响：Web/Console 通道一旦进入重复输出（模型退化、上游 loop 未被其自身 `doom_loop_check` 捕获），流会无限持续直到 600s 总超时（`config/grok_limits.go:38` 默认 600s，可配到 86400s），期间持续消耗账号配额并把重复内容写进客户端上下文；B 在 128/256 帧内终止同一流。
- 修复：把 B 的 `streamRepeatTracker` 按其阈值移植到本项目唯一的帧入口 `consumeCompatibleSSE`（或 `readResponseSSE`），并新增 `upstream_output_loop` 分类（`internal/errors/classify.go` 与 `writeSSEError` 的 code）。

### A2-2 [P1] 下游写错误被整体丢弃且无写超时；移植注释依赖的「client write deadline」在本项目不存在
- 本项目：`internal/grok/http_helpers.go:121-127` — `_, _ = w.Write(grokSSEEventPrefixBytes); writeSSEEventName(w, event); _, _ = w.Write(grokSSENewlineBytes); ... _, _ = w.Write(grokSSEDataPrefixBytes); _, _ = w.Write(data); _, _ = w.Write(grokSSEFrameSuffixBytes)`
  以及 `internal/grok/grok2api_streamidle.go:47-50` — `// Downstream backpressure (for example ConvertResponseStream blocked on an unbuffered io.Pipe write) must not look like an upstream stall; the client write deadline covers that case.`
  （全仓 `grep -rn "SetWriteDeadline" internal/ cmd/` 只有 `mgw_websocket_transport.go:237` 的 websocket 写；`cmd/server/main.go:283-294` 的 `http.Server` 未设置 `WriteTimeout`）
- grok2api：`B:backend/internal/transport/http/inference/handler.go:1440-1445` — `if err := setResponseWriteDeadline(writer); err != nil { return inspector.Metadata(), err }; if _, err := writer.Write(chunk); err != nil { return inspector.Metadata(), err }`
  以及 `B:backend/internal/transport/http/inference/handler.go:533-537` — `func setResponseWriteDeadline(writer http.ResponseWriter) error { err := http.NewResponseController(writer).SetWriteDeadline(time.Now().Add(responseWriteTimeout)); ... }`（`responseWriteTimeout = 30 * time.Second`，`B:.../handler.go:51`）
- 差异/错误：B 对下游每次写之前都重设 30s 写截止时间，写错误即中止整个流并释放上游；本项目 `writeSSEBytes` 对 4 次 `Write` 全部丢弃返回值（连短写也不检测，只有翻译器路径用的 `checkedStreamWriter` 才检测，见 `internal/grok/messages_search.go:22-32`），且没有任何写超时。由于语义 idle 在 `readers == 0` 时停止计时（`internal/grok/grok2api_streamidle.go:169-174`，本项目测试 `grok2api_streamidle_test.go:173` 明确断言「没人读上游时 idle 不计时」），下游一旦卡住（TCP 零窗口、慢客户端、`io.Pipe` 阻塞），既没有写超时打断，也没有读超时兜底——写调用会永久阻塞。
- 影响：单个卡死客户端即可永久占用 goroutine + 上游连接 + 账号并发额度（连接不被释放，上游也不会被取消），是可被外部触发的资源耗尽面；同时下游永远不会因为写失败而停止上游生成。
- 修复：在 `streamResponseHeaders` 返回前/每次 `writeSSELog`/`writeSSEBytes` 前调用 `http.NewResponseController(w).SetWriteDeadline(...)`（与 B 同值 30s），并让 `writeSSEBytes` 返回错误、由 `streamChat` 与原生 Responses 复制循环据此中止流。

### A2-3 [P2] 上游私有控制事件 `response.doom_loop_check` 原样透传给客户端
- 本项目：`internal/grok/handler_responses_store.go:365-368` — `if err := frame.writeTo(target); err != nil { result.Err = err; return err }`
  （该 `consumeCompatibleSSE` 回调（第 342-392 行）除对 `[DONE]` 与 `error`/`response.failed` 做处理外没有任何事件类型过滤；同包 `internal/grok/responses_alias.go:55-112` 的 `rewriteBuildToolAliasSSE` 也是全量重写后转发）
- grok2api：`B:backend/internal/infra/provider/cli/responses_response.go:46-49` — `err := consumeCompatibleSSE(source, func(event compatibleSSEEvent) error { if isPrivateBuildControlEvent(event) { return nil }`
  同处 `B:.../responses_response.go:92-97` — `func isPrivateBuildControlEvent(event compatibleSSEEvent) bool { if strings.TrimSpace(event.Event) == "response.doom_loop_check" { return true } ... payload.Type == "response.doom_loop_check" }`（`normalizeResponseStream` 对**每一条** Responses 流执行，见 `B:backend/internal/infra/provider/cli/adapter.go:445-447` — `if responsesOperation && resp.StatusCode >= 200 && resp.StatusCode < 300 { if request.Streaming { resp.Body = toolCompatibility.normalizeResponseStream(resp.Body)`；`c == nil` 分支同样过滤，见 `responses_response.go:50-52`）
- 差异/错误：B 把 `response.doom_loop_check`（无论以 `event:` 名还是 data 里的 `type` 出现）视为私有控制事件并在边界处丢弃；本项目没有等价过滤，原生 Build Responses 路径会把它当作普通事件转发给下游。该事件确实存在于上游流中——本项目自己的 idle 检测器测试就是用它做样本（`internal/grok/grok2api_streamidle_test.go:22`）。
- 影响：Codex/Grok TUI 等严格客户端会收到未在 Responses schema 中定义的 `response.doom_loop_check` 事件；若客户端对未知 `type` 直接报错，会把一次正常生成判为协议错误。
- 修复：在 `consumeCompatibleSSE` 的两个 Responses 消费点（`handler_responses_store.go` 与 `responses_alias.go`）加入与 B 等价的 `isPrivateBuildControlEvent` 过滤（同时保留 idle 检测器已有的识别行为不变）。

### A2-4 [P2] Build 专用「语义 idle」被套用到全部通道；检测器的活动模型与 Web 帧形状不相容
- 本项目：`internal/grok/request_helpers.go:24-26` — `if idle > 0 && resp.StatusCode == http.StatusOK && strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") { resp.Body = wrapBuildSemanticIdle(resp.Body, idle) }`
  （该函数由 Console `internal/grok/dpop.go:326`、Web `internal/grok/client.go:660`、Build `internal/grok/cli.go:422` 三处共用；检测器只承认 Responses 生成事件名，见 `internal/grok/grok2api_streamidle.go:18-25`，而 Web 帧是 `result.response` 信封，见 `internal/grok/util.go:221-224` — `resp, _ := result["response"]; if resp == nil { continue }`）
- grok2api：`B:backend/internal/infra/provider/console/adapter.go:161-162` — `if idleCancel != nil && response.StatusCode >= 200 && response.StatusCode < 300 && response.Body != nil { response.Body = providerstreamidle.New(response.Body, time.Duration(cfg.StreamIdleTimeoutSeconds)*time.Second, idleCancel) }`
  以及 `B:backend/internal/infra/provider/cli/adapter.go:423-424` — `if request.Streaming && isHTTPSuccess(resp.StatusCode) && resp.Body != nil { resp.Body = wrapBuildSemanticIdle(resp.Body, a.config().StreamIdleTimeout) }`（Web 侧同为按字节包装：`B:backend/internal/infra/provider/web/gateway.go:146-147`）
- 差异/错误：三层差异叠加。① B 只对 CLI/Build 使用「忽略 keepalive 与控制事件」的语义 idle，Web/Console 使用**任何字节都重置**的 `providerstreamidle.ReadCloser`；本项目没有字节级包装，把语义 idle 用在三个通道上。② B 的语义包装条件只要求 `request.Streaming && 2xx`，本项目还要求 `StatusCode == 200` 且 `Content-Type` 含 `text/event-stream`。③ 本项目的语义检测器只把 Responses 生成事件记为活动，而 Web 通道的帧是 `{"result":{"response":...}}` 信封（其解析器只接受含 `result.response` 的对象），这类帧无法产生检测器识别的根 `type`/事件名。结果是：Console 上「只有 keepalive 的长静默段」在 B 存活、在本项目会被 120s 杀掉；Web 上若上游确实声明 `text/event-stream`（否则包装根本不安装），连续数据也无法续命，等价于按累计读取时间计时。
- 影响：Web/Console 长生成（长推理、长工具调用前的静默期）在本项目会被提前以 idle 超时截断，而 B 用同一配置不会；反之未声明 SSE 的 2xx 流在本项目完全没有 idle 保护，只能等 600s 总超时（`internal/config/grok_limits.go:38-50`）。
- 修复：按 B 的分层方式实现两级 idle——通道级按字节重置（Web/Console，2xx 即可）+ Build 语义级；或在 `wrapBuildSemanticIdle` 中同时把 Web 信封帧（`result.response.modelResponse.token` 等）纳为活动。

### A2-5 [P2] idle 超时没有专用错误分类：本项目变成 `stream_read_error` / `stream parse error`
- 本项目：`internal/grok/grok2api_streamidle.go:16` — `var errGrokSemanticIdle = errors.New("upstream stream idle timeout")`
  （全仓除本文件外零引用，未接入 `internal/errors/classify.go`；消费点 `internal/grok/handler_responses_store.go:399-400` — `if err != nil && err != io.EOF { failureCode, failureMessage = "stream_read_error", "upstream response stream could not be read" }`；Chat 侧 `internal/grok/handler_chat.go:1300` — `writeSSEStreamError(w, flusher, logger, "stream parse error: "+err.Error())`）
- grok2api：`B:backend/internal/transport/http/inference/handler.go:1364-1365` — `case errors.Is(err, neterror.ErrUpstreamStreamIdleTimeout): return "upstream_stream_idle_timeout"`
  以及 `B:backend/internal/transport/http/inference/handler.go:1510-1514` — `func streamAbortTrailer(protocol streamProtocol, cause error, meta responseMetadata, compat *responsesCompatState) []byte { code, message := "upstream_stream_interrupted", "上游流式响应中断"; switch { case errors.Is(cause, neterror.ErrUpstreamStreamIdleTimeout): code, message = "upstream_stream_idle_timeout", "上游流式响应长时间无数据"`（哨兵定义 `B:backend/internal/pkg/neterror/classify.go:15`）
- 差异/错误：B 的 idle 错误是共享哨兵，`errors.Is` 可达，据此生成专用 code（Chat 中止尾帧、Responses `response.failed`、Anthropic `error` 三协议一致）；本项目的哨兵是包私有、从未被比较，Read 返回的错误沿 `consumeCompatibleSSE` 泛化上升：Responses 面报 `stream_read_error`，Chat 面报 `stream_error` 并把文案写成 `stream parse error: upstream stream idle timeout`（并非解析错误）。虽然 `apperrors.PublicMessage` 会按 category=`timeout` 输出「upstream stream timed out」文案，客户端可见的 **code** 仍是通用值。
- 影响：客户端无法把「上游静默超时」与真正的传输/解析错误区分，重试与告警策略只能退化为通用 5xx；运维侧也只能靠文案匹配。
- 修复：把哨兵提升为可 `errors.Is` 的导出错误（或复用 `internal/errors` 中的分类），在 Chat/Responses/Anthropic 三条错误帧路径映射为专用 code，并去掉 `"stream parse error: "` 前缀（该前缀同样污染 context.Canceled 等非解析错误）。

### A2-6 [P2] 原生 Responses 流被强制重新分帧并追加 `data: [DONE]`；B 保持上游字节
- 本项目：`internal/grok/handler_responses_store.go:426-428` — `// Emit exactly one DONE, after the terminal (including a synthesized failure). if err := (compatibleSSEEvent{data: []string{"[DONE]"}}).writeTo(target); err != nil { result.Err = err }`
  （所有帧都经 `frame.writeTo(target)` 重新序列化：CRLF→LF、多行 data 拆分为多行 `data:`，`internal/grok/grok2api_sse.go:30-63`）
- grok2api：`B:backend/internal/infra/provider/conversation/stream.go:1037-1045` — `// guardResponseStream 保持 native Responses SSE 的原始字节不变，同时在读取时解析事件并在检测到循环时关闭上游。 func guardResponseStream(source io.ReadCloser) io.ReadCloser { ... err := consumeSSE(io.TeeReader(source, writer), ...`
  （传输层只在需要补齐字段时改写单条 data 行，从不新增 `[DONE]`：`B:backend/internal/transport/http/inference/responses_compat.go:114-140`）
- 差异/错误：B 的原生 Responses 路径是「上游字节 → 下游」的透传（仅在字段缺失时补齐同一行 JSON），结束符完全由上游决定；本项目无论上游是否发送 `[DONE]`，都会在终止事件后自行补一个 `data: [DONE]\n\n`，并顺带把每个帧按 LF 重新分帧。若上游本身也发 `[DONE]`，本项目因为在首个终止事件即返回 `io.EOF`（`handler_responses_store.go:382-389`）不会重复，但两边的终止符契约不同：B 可以为「无 DONE 的 Responses 流」，本项目必然出现 DONE。
- 影响：Responses 协议本身不使用 `[DONE]`；下游若按 B（以及官方 Responses）语义解析，会把额外 DONE 当未知帧（多数客户端忽略，但严格客户端会报错），且 CRLF/分帧差异使同一上游流在两边的字节表示不同，不利于以字节比对做灰度校验。
- 修复：原生 Responses 透传路径不要注入 `[DONE]`（仅在检测到上游终止事件缺失时才合成失败信封），并保留上游原始分帧（透传 `scanner` 原始行而非 `writeTo` 重排）。

### A2-7 [P2] 本地合成 `response.failed` 信封缺 `created_at`/`completed_at`/`output`，且多一个 `[DONE]`
- 本项目：`internal/grok/handler_responses_store.go:414-419` — `failure, _ := json.Marshal(map[string]interface{}{ "type": "response.failed", "response": map[string]interface{}{ "id": responseID, "object": "response", "status": "failed", "model": model, "error": map[string]interface{}{"code": failureCode, "message": failureMessage} } })`
- grok2api：`B:backend/internal/transport/http/inference/handler.go:1548-1556` — `response := map[string]any{ "id": id, "object": "response", "created_at": compat.createdAt, "completed_at": compat.createdAt, "status": "failed", "model": model, "output": []any{}, "error": map[string]any{...} }`
  以及 `B:backend/internal/transport/http/inference/handler.go:1561-1567` — `event := map[string]any{ "type": "response.failed", "id": id, "sequence_number": meta.SequenceNumber + 1, "response": response }; sanitizeResponsesEvent(event, compat)`（`B:.../handler.go:1572` 输出 `event: response.failed` 命名帧，该分支结尾**不追加** `[DONE]`，对比 `streamProtocolChat` 分支 `B:.../handler.go:1533` — `return []byte("data: " + string(payload) + "\n\ndata: [DONE]\n\n")`）
- 差异/错误：同一类「上游中止」失败，B 输出带 `created_at`/`completed_at`/`output:[]`/`sequence_number` 并经 `sanitizeResponsesEvent` 补齐 `id`、`error.id` 的信封；本项目只输出 `id/object/status/model/error` 五字段，`created_at` 与 `output` 缺失（`responseID` 还可能是空串——上游未给 `id` 时 A 不回填）。B 只给 Chat 协议补 `[DONE]`，Responses 协议不加；本项目两条路径都加（见 A2-6）。
- 影响：把流式失败信封当作结构化对象解析的客户端（Grok TUI 明确依赖 `model`、时间戳、`output` 数组）会拿到字段残缺的信封；`sequence_number` 缺失使多事件流的顺序校验断档。
- 修复：合成失败信封时补齐 `created_at`/`completed_at`/`output: []`/`sequence_number`，并在无上游事件 id 时生成 `resp_*`；Responses 协议下不要追加 `[DONE]`。

### A2-8 [P2] Chat 中途错误帧形状不同：本项目 `event: error` + `request_id`，B data-only
- 本项目：`internal/grok/http_helpers.go:136-147` — `func writeSSEError(w http.ResponseWriter, message, errType, code string) { middleware.MarkStreamFailure(w); ... payload := map[string]interface{}{"error": map[string]interface{}{"message": ..., "type": ..., "code": ..., "request_id": requestID}}; writeSSEBytes(w, "error", encodeJSONBytes(payload)) }`
  （随后 `internal/grok/http_helpers.go:171-181` 再写 `data: [DONE]` 并 flush）
- grok2api：`B:backend/internal/infra/provider/conversation/chat_stream.go:125-130` — `func (c *streamConverter) streamErrorChat(data []byte) error { if err := c.writeData(map[string]any{"error": normalizeOpenAIStreamError(streamErrorValue(data))}); err != nil { return err }; _, err := io.WriteString(c.writer, "data: [DONE]\n\n"); return err }`
  （`writeData` 只写 `data: %s\n\n`，见 `B:backend/internal/infra/provider/conversation/stream.go:917-923`；传输层中止尾帧同样 data-only：`B:backend/internal/transport/http/inference/handler.go:1533`）
- 差异/错误：OpenAI Chat 流的中途错误，B 输出无 `event:` 名的 `data: {"error":{...}}` 紧跟 `data: [DONE]`；本项目输出带 `event: error` 名的帧，并在 error 对象内额外提供 `request_id`，错误对象里没有 `type: "api_error"`（B 的 `normalizeOpenAIStreamError` 默认带 `type`）。两边都发 `[DONE]`，这一点一致。
- 影响：按「SSE 事件名」或按 `error.type` 分派的客户端在两种实现下行为不同；带 `event: error` 的帧会被只解析 data 的客户端忽略事件名（无害），但反过来依赖 `error.type` 的客户端在本项目会取到空值。
- 修复：Chat 路径按 OpenAI 约定改为 data-only 错误对象并补 `type`；Anthropic/Responses 路径保留命名事件（B 也是这么分的：`handler.go:1573-1580` 走 `event: error`）。

### A2-9 [P2] SSE 响应头：本项目从不设置 `X-Accel-Buffering: no`、`Content-Type` 无 charset、多写 hop-by-hop `Connection`；桥接路径还丢弃内层已提交头
- 本项目：`internal/grok/http_helpers.go:101-107` — `func streamResponseHeaders(w http.ResponseWriter) http.Flusher { w.Header().Set("Content-Type", "text/event-stream"); w.Header().Set("Cache-Control", "no-cache"); w.Header().Set("Connection", "keep-alive"); flusher, _ := w.(http.Flusher); return flusher }`
  以及 `internal/grok/handler_responses.go:295-304` — `h.withChatStream(subReq, func(status int, header http.Header, reader io.Reader) { if status < 200 || status >= 300 { ... } writeResponsesStreamFromChatReaderRequest(w, req, reader) })`（2xx 分支不把内层提交的 `header` 复制到 `w`）
- grok2api：`B:backend/internal/infra/provider/web/chat.go:2681-2686` — `func streamHeaders() http.Header { value := http.Header{}; value.Set("Content-Type", "text/event-stream; charset=utf-8"); value.Set("Cache-Control", "no-cache"); value.Set("X-Accel-Buffering", "no"); return value }`
  以及 `B:backend/internal/transport/http/inference/handler.go:2246-2249` — `excluded := map[string]struct{}{ "connection": {}, "content-length": {}, "keep-alive": {}, ... }`（透传上游头时显式剔除 hop-by-hop）
- 差异/错误：B 在每个 SSE 响应上显式声明 `X-Accel-Buffering: no`（Web 提供方 `streamHeaders`，经 `copyHeaders` 到下游），本项目全仓（含 `deploy/`、`docs/`）无任何 `X-Accel-Buffering` 设置；本项目自行拼 `Connection: keep-alive`，而 B 明确把它排除在透传之外。此外 `/v1/responses` 桥接成功分支不复制内层 `HandleChatCompletions` 已提交的头（Messages 桥接则复制，`internal/grok/handler_messages.go:802-804`），导致 `X-Grok2api-*` 类告警头在该路径丢失。
- 影响：部署在 nginx（默认 `proxy_buffering on`）等反代后，SSE 会被缓冲，实时性失效（这是流式代理最常见的线上问题）；`Connection` 属于逐跳头，经 HTTP/2 或中间代理时不应由应用设置；桥接 Responses 路径少头会造成两个入口的响应头不一致。
- 修复：`streamResponseHeaders` 增加 `X-Accel-Buffering: no`、`Content-Type` 带 charset、删除 `Connection`；Agent 桥接路径在 2xx 分支先复制内层 `header` 再补齐 SSE 头。

### A2-10 [P3] Anthropic 流式 usage 形状不同：`message_start` 恒为零 usage，终端缺 `cache_creation_input_tokens`/`output_tokens_details`
- 本项目：`internal/grok/handler_messages.go:841` — `toolIndexes: map[int]int{}, open: map[int]bool{}, usage: map[string]interface{}{"input_tokens": 0, "output_tokens": 0},`
  同文件 `internal/grok/handler_messages.go:1083-1086` — `writeAnthropicSSE(w, "message_delta", map[string]interface{}{"type": "message_delta", "delta": ..., "usage": s.usage})`；映射函数 `internal/grok/handler_messages.go:784-789` 只产出 `input_tokens`/`output_tokens`（有缓存时加 `cache_read_input_tokens`）
- grok2api：`B:backend/internal/infra/provider/conversation/messages_stream.go:9-12` — `usage := anthropicUsage(c.usage, 0); // ... delete(usage, "output_tokens_details")`
  以及 `B:backend/internal/infra/provider/conversation/messages_response.go:112-118` — `usage := map[string]any{ "input_tokens": inputTokens - cacheReadInputTokens, "output_tokens": outputTokens, "cache_creation_input_tokens": 0, "cache_read_input_tokens": cacheReadInputTokens, "output_tokens_details": map[string]any{"thinking_tokens": thinkingTokens}, ... }`
- 差异/错误：B 用上游 `response.created`/终端的 usage（`setResponse` → `c.usage`）构造 `message_start` 与 `message_delta` 的 Anthropic usage 形状，包含 `cache_creation_input_tokens: 0`、`cache_read_input_tokens`、`output_tokens_details.thinking_tokens`、`cost_in_usd_ticks`、`server_tool_use.web_search_requests`；本项目 `message_start` 用固定零值 map（上游 `response.created` 里的 usage 完全不看），终端 `message_delta` 只在收到 OpenAI `usage` chunk 时由 `anthropicUsageFromOpenAI` 覆盖，缺 `cache_creation_input_tokens` 与 `output_tokens_details`（本项目只在 `finish()` 补 `server_tool_use`，见 `internal/grok/handler_messages.go:1065-1067`）。
- 影响：Claude 客户端/用量统计在 Anthropic 入口拿不到缓存创建字段与 thinking token 分解，与 B 同一上游请求的用量报告不一致（本报告不涉及计费正确性，仅协议字段）。
- 修复：`message_start` 使用上游 `response.created` 的 usage 转换结果（或至少在收到首个 usage 前延迟发送 `message_start`），并在 `anthropicUsageFromOpenAI` 中补齐 `cache_creation_input_tokens`、`output_tokens_details.thinking_tokens`（reasoning token 已由 `addReasoningUsage` 计算）。

### A2-11 [P3] 移植时丢失 `semanticIdleReadCloser.TimedOut()`
- 本项目：`internal/grok/grok2api_streamidle.go:184-191` — `func (r *semanticIdleReadCloser) Close() error { ... return r.closeInner() }` / `func (r *semanticIdleReadCloser) closeInner() error { r.closeOnce.Do(...) }`（之后直接进入 `buildSSEActivityDetector`，无 `TimedOut`）
- grok2api：`B:backend/internal/infra/provider/cli/semantic_streamidle.go:197-201` — `func (r *semanticIdleReadCloser) TimedOut() bool { r.mu.Lock(); defer r.mu.Unlock(); return r.timedOut }`
- 差异/错误：除该方法外，本项目的语义 idle 实现与 B 逐行等价（已用统一化 diff 核对：唯一差异即此处，以及哨兵错误类型见 A2-5）。B 的 `TimedOut()` 在生产代码中只被测试使用（`backend/internal/infra/provider/cli/semantic_streamidle_test.go:190,251,275,296`），因此这只是可观测性/测试接口的缺失，本项目测试改用 `grok2apiTestTimedOut` 直接读字段（`internal/grok/grok2api_streamidle_test.go:317-321`）。
- 影响：无运行时影响；本项目的 idle 状态无法被生产/诊断代码查询，后续要按 idle 状态做诊断时需要重新加接口。
- 修复：补回 `TimedOut()`（与 B 同名同语义），让测试与潜在诊断共用，而不是访问私有字段。

### A2-12 [P3] idle 超时配置语义不同：单值三通道共用、默认 120s、上限 3600s
- 本项目：`internal/config/grok_limits.go:53-58` — `func (c *Config) GrokStreamIdleTimeout() time.Duration { value := 0; if c != nil { value = c.GrokStreamIdleSeconds }; return time.Duration(boundedDefault(value, 120, 3600)) * time.Second }`
  （单一字段 `GrokStreamIdleSeconds`，见 `internal/config/config.go:105`，Console/Web/Build 共用；三个通道都调用同一函数：`dpop.go:326`、`client.go:660`、`cli.go:422`）
- grok2api：`B:backend/internal/domain/settings/settings.go:10-17` — `DefaultBuildStreamIdleTimeout = 2 * time.Minute; ... DefaultWebStreamIdleTimeout = 90 * time.Second; DefaultConsoleStreamIdleTimeout = 2 * time.Minute; MinProviderStreamIdleTimeout = 30 * time.Second; MaxProviderStreamIdleTimeout = 10 * time.Minute`
- 差异/错误：B 按通道分别配置并有 `[30s, 10min]` 归一化（Web 默认 90s、Console/Build 默认 120s）；本项目一个值管三个通道，默认 120s（与 B 的 Web 默认不同），且允许配到 3600s（6 倍于 B 的上限），也没有下限保护。首字节预算也不同：本项目三个 Grok 客户端都以 `headerTimeout = 0` 构造（`internal/grok/client.go:1536` — `return util.GetSharedBrowserHTTPClientWithHeaderTimeout(proxyKey, timeout, 0, proxyFunc)`、`internal/grok/cli.go:47`），即**没有**响应头超时，只有 600s 总 deadline（`internal/util/browser_transport.go:43-46` 注释：`A zero headerTimeout leaves header waiting bounded by the HTTP total deadline.`）；B 为 Build 单独提供默认 5 分钟的 `DefaultBuildResponseHeaderTimeout`（`B:backend/internal/domain/settings/settings.go:6-8`）。
- 影响：Web 通道的默认静默容忍度比 B 宽 33%，而允许的 3600s 上限会让运维配置出 B 会拒绝的极端值；无法像 B 那样只放宽某个通道。
- 修复：拆分为 web/console/build 三个字段，按 B 的默认值与 `[30s, 10min]` 归一化（若要保持兼容，可在读取旧单值字段时把它作为三者的初值）。
