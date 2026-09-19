# Responses API / CLI 适配对比

审计范围：OpenAI Responses 面（`/v1/responses`、`/responses/{id}`、`/responses/{id}/input_items`、`/responses/{id}/cancel`、`/responses/compact`）与 Grok CLI/Build（codex / grok-code）适配层。
A 侧路径相对 `Orchids-2api` 根；B 侧路径以 `B:` 前缀相对 `.upstream/grok2api/` 根（Go 后端根为 `backend/`，故 B 引用写作 `B:backend/...`）。

## 结论摘要

共 14 条发现：P1 × 8、P2 × 6。核心结论：

1. **原生 Build Responses 面在 A 中几乎是"裸透传"**：流事件不做 serde 兼容补齐（A3-1），响应侧工具（apply_patch / custom）不做身份与结构还原（A3-2、A3-3），请求侧 input 历史项不做归一化（A3-4），工具 schema 不做根联合展开与 `$ref` 解析（A3-5）。B 在 `responses_compat.go` / `responses_history.go` / `responses_input.go` / `responses_tool_declarations.go` 中承担了这些工作，A 侧对应文件（`grok2api_sse.go`、`responses_alias.go`、`responses_normalize.go`）只覆盖了其中很小一部分。
2. **会话身份与会话头取值分歧**（A3-6、A3-10）：A 只认 4 个固定头 + body `prompt_cache_key`，Claude Code / Codex 的会话信号（`X-Claude-Code-Session-Id`、`x-codex-*`、body `session_id`/`conversation_id`/`client_metadata`）全部丢失；且上游 `x-grok-session-id`/`x-grok-conv-id` 发送 64 字符 hex 摘要而非 UUID。
3. **compaction 能力缺失**（A3-7）：A 把 `/responses/compact` 与带 `compaction_trigger` 的普通 `/responses` 当成普通请求原样转发，没有 B 的网关侧摘要生成 + `g2a_compact_v1` blob 编解码。
4. **持久化与缺省语义不同**（A3-8、A3-9、A3-14）：`store` 缺省、`include: reasoning.encrypted_content` 缺省、`input_items` 内容范围三处与 B（及 OpenAI 契约）不一致。

部分差异属漂移（B 在 `44a390b8..906b9493` 之间新增），已在各条目中标注来源。

## 发现

### A3-1 [P1] 原生 Build Responses 流未做 serde 兼容补齐，Grok TUI/Codex 解析失败

- 本项目：`internal/grok/handler_responses_store.go:361` — `if redactResponseError(event) {` / `raw, _ := json.Marshal(event)` / `frame.data = []string{string(raw)}` / `}` / `if err := frame.writeTo(target); err != nil {`
- grok2api：`B:backend/internal/transport/http/inference/handler.go:1428` — `if protocol == streamProtocolResponses {` / `chunk = rewriteResponsesStreamChunk(chunk, &compat)` / `}`；以及 `B:backend/internal/transport/http/inference/responses_compat.go:248` — `if stringAny(typed["type"]) == "output_text" && typed["annotations"] == nil {` / `typed["annotations"] = []any{}`
- 差异/错误：A 的原生 Build Responses 路径（`handleNativeCLIResponsesAt` → `copyNativeCLIResponseAndCaptureModel`）对上游 SSE 事件只做错误脱敏，随后原样写出。A 中不存在 `sanitizeResponsesEvent` / `ensureOutputTextAnnotations` / `responsesCompatState` 任何等价实现（`grep -rn "sanitizeResponsesEvent\|ensureOutputTextAnnotations\|rewriteResponsesStreamChunk" internal/` 无命中）。B 在写出前逐事件补齐：`output_text.annotations`（缺失会让 Grok CLI 报 `serialization error: missing field annotations`）、`response.id` 单值化、`response.created_at`、`response.object`、`response.output`、`response.model`（`responses_compat.go:183-186`，缺失会让 TUI serde 在 `response.failed`/`response.completed` 上报错）、`event.item_id`（`responses_compat.go:211-221`）与 `item.id`（`responses_compat.go:193-209`）。A 仅在 chat→Responses 桥（`internal/grok/responses_stream.go`）自建事件时才带上这些字段。
- 影响：走原生 Build Responses 的 Codex / Grok TUI 客户端在工具续跑、失败重试、TUI 会话恢复时会因缺字段而整轮失败；A 的 chat 桥路径与原生路径对同一上游事件产出两种不同 wire shape。
- 修复：把 B 的 `responses_compat.go` 整体移植为 A 的 `responsesCompatibilityState`，在 `copyNativeCLIResponseAndCaptureModel` 写出前对每个 `compatibleSSEEvent` 调用等价 sanitize（含 EOF 时的 `flushResponsesStreamTail`）；至少在 native 路径补齐 annotations / model / id / created_at / output / item_id。

### A3-2 [P1] apply_patch_call 还原时缺少 operation，且保留 arguments

- 本项目：`internal/grok/responses_alias.go:160` — `case "apply_patch":` / `typed["type"] = "apply_patch_call"` / `delete(typed, "name")` / `}`
- grok2api：`B:backend/internal/infra/provider/cli/responses_response.go:388` — `case responsesApplyPatchTool:` / `operation, err := decodeApplyPatchArguments(call["arguments"], "response.output[].arguments")` / `call["type"] = "apply_patch_call"` / `call["operation"] = operation`
- 差异/错误：A 在响应侧把 `apply_patch` 别名的 `function_call` 改写成 `apply_patch_call` 时只改 `type` 并删 `name`，既不把 `arguments`（JSON 字符串）解码成 `operation` 对象，也不删除 `arguments`；B 解码为 `operation` 并删除 `name`/`namespace`/`arguments`（`responses_response.go:393-397`）。
- 影响：Codex 拿到的 `apply_patch_call` 缺少必填 `operation`，且残留非法字段 `arguments`，补丁调用无法执行或直接被客户端 schema 拒绝。
- 修复：移植 `decodeApplyPatchArguments` / `validateApplyPatchOperation`（B `responses_codex_tools.go:125-158`），在 `rewriteBuildToolAliasValue` 的 apply_patch 分支设置 `operation` 并删除 `arguments`；同时覆盖流式路径（B 会补发 `response.output_item.added`，见 `responses_response.go:408-438`）。

### A3-3 [P1] custom（freeform grammar）工具未模拟，原样透传给上游

- 本项目：`internal/grok/responses_normalize.go:558` — `case "mcp", "shell", "custom", "x_search", "web_search", "image_generation", "collections_search", "file_search", "code_execution", "code_interpreter":` / `return []map[string]interface{}{cloneStringInterfaceMap(tool)}, nil`
- grok2api：`B:backend/internal/infra/provider/cli/responses_custom.go:30` — `description += "Provide the custom tool input in the input string field."` / `c.addWarning("custom_tool_emulated")` / `return []any{map[string]any{` / `"type": "function", "name": c.alias(identity), ...`
- 差异/错误：B 把 `type: custom` 的 freeform 工具（含 `format` grammar）降级为一个 `input: string` 的普通 function，并在响应侧 `rewriteFunctionCall` 把 `function_call` 还原为 `custom_tool_call` + `input`（`responses_response.go:372-381`）；`format` 非 text 时仅告警 `custom_tool_format_downgraded`。A 的 `normalizeBuildTool` 直接把 `custom` 声明原样转发，且 A 的 `collectBuildToolAliases`（`responses_normalize.go:78-107`）与 `rewriteBuildToolAliasValue`（`responses_alias.go:144-163`）都没有 `custom` 分支，响应侧不会还原。
- 影响：Codex 的 freeform/grammar 工具（以及 MCP 输出型工具）在 Build 平面被整轮拒绝或产出客户端无法识别的 `function_call`，而不是降级为可用的字符串输入。
- 修复：移植 `normalizeCustomTool` / `encodeCustomToolArguments` / `decodeCustomToolInput`，在 `normalizeBuildTool` 的 `custom` 分支走模拟路径，并把 `custom_tool_call`/`custom_tool_call_output` 的输入与输出侧映射补齐。

### A3-4 [P1] 原生 Build Responses 的 input 历史项未做归一化，Codex 扩展项被原样转发

- 本项目：`internal/grok/responses_normalize.go:475` — `for _, item := range interfaceMaps(payload["input"]) {` / `if parseLooseStringAny(item["type"]) != "function_call" {` / `continue` / `}`（仅重写 function_call 的别名，其余 input 项不动）
- grok2api：`B:backend/internal/infra/provider/cli/responses_history.go:154` — `case "local_shell_call":` / `converted, err := normalizeLegacyLocalShellCallInput(item, param)`；以及 `B:backend/internal/infra/provider/cli/responses_input.go:39` — `return map[string]any{` / `"type": "message", "role": "assistant",` / `"content": []any{map[string]any{"type": "output_text", "text": "Local shell call (" + status + "): " + string(action)}}`
- 差异/错误：B 的 `normalizeInputItems` 逐项处理 `message`/`function_call`/`function_call_output`/`reasoning`/`shell_call`/`mcp_*`/`compaction`/`tool_search_call`/`tool_search_output`/`custom_tool_call(_output)`/`apply_patch_call(_output)`/`agent_message`/`local_shell_call(_output)`/`shell_call_output`/`mcp_tool_call_output`/`compaction_trigger`/`additional_tools`，未知类型还会替换为 boundary 文本并告警 `unsupported_input_history_omitted`（`responses_history.go:201-208`）。A 在原生路径上除 function_call 改名外完全不检查 `input`，未知/扩展项直接进上游。
- 影响：Codex / pi 等客户端回放 `local_shell_call`、`agent_message`、`mcp_tool_call_output` 历史时，A 会把这些宿主态调用原样送进 Build，B 的注释指出这会"伪造可再次执行的 hosted shell call"（`responses_input.go:29`）；同时 `agent_message` 里的不透明密文会被转发（B 用 boundary 文本替换以保证不泄露密文）。
- 修复：移植 `responses_history.go` 的 `normalizeInputItems` 与其依赖（`responses_input.go`、`responses_codex_tools.go` 的 call/output 归一化），在 `normalizeBuildResponsesPayload` 中对 `payload["input"]` 调用。

### A3-5 [P1] 函数工具 schema 只做浅层 nullable 折叠，且 inputSchema/input_schema 未转 parameters

- 本项目：`internal/grok/responses_normalize.go:580` — `for _, keyword := range []string{"anyOf", "oneOf"} {` / `branches, ok := out[keyword].([]interface{})` / `if !ok {` / `continue`；同文件 `509` — `if schema, ok := out["parameters"].(map[string]interface{}); ok {`
- grok2api：`B:backend/internal/infra/provider/cli/responses_tool_declarations.go:182` — `if inputSchema, exists := converted["inputSchema"]; exists {` / `converted["parameters"] = inputSchema` / `delete(converted, "inputSchema")`；同文件 `430` — `branchSchema, branchOK := branch.(map[string]any)` / `if !branchOK || isNullOnlySchema(branchSchema) {` / `c.changed = true` / `continue`
- 差异/错误：A 的 `normalizeBuildFunctionRoot`（`responses_normalize.go:567-604`）只做两件事：把 `type` 数组里的 `"null"` 去掉，以及在过滤后**恰好剩 1 个**分支且该分支 `type=="object"` 或含 `properties` 时把该分支上提。它不解析 `$ref`（含根 `$ref` 指向另一个 union 的情况）、不做多分支 object 叶子收集、不合并 sibling 约束、不裁剪/保留 `$defs`、不校验分支两两互斥（B 的 `rootObjectLeafCollector.walk` + `rootObjectLeavesPairwiseDisjoint`，`responses_tool_declarations.go:370-475`、`571-601`）。此外 A 只读取 `parameters`，当客户端用 `inputSchema`/`input_schema` 声明 schema 时既不转换也不删除，等于把 schema 丢弃后把未知字段交给上游。
- 影响：Codex/MCP 常见的"根 `anyOf`/`oneOf` + `$ref`"工具 schema 在 A 上被拒（Build grammar 无法编译）或参数 schema 静默丢失，表现为工具调用参数为空/整轮失败；B 会正常展开为对象叶子联合并把 `inputSchema` 转成 `parameters`。
- 修复：移植 `NormalizeBuildFunctionParametersRoot` 全套（`maxRootUnionDepth`/`maxRootUnionLeaves`/`isNullOnlySchema`/`isObjectRootSchema`/`resolveLocalSchemaRef`/`referencedDefs`），并在 `normalizeBuildTool` 的 function 分支先做 `inputSchema`/`input_schema` → `parameters` 转换。

### A3-6 [P1] 会话种子只认 4 个自定义头，Claude Code / Codex 会话信号全部丢失

- 本项目：`internal/grok/session_state.go:58` — `for _, header := range []string{"x-grok-session-id", "x-grok-conv-id", "x-session-id", "session-id"} {` / `if seed = strings.TrimSpace(r.Header.Get(header)); seed != "" {` / `break`
- grok2api：`B:backend/internal/transport/http/inference/prompt_cache.go:25` — `if seed := normalizePromptCacheSeed(headers.Get("X-Claude-Code-Session-Id")); seed != "" {` / `return claudeCodePromptCacheSeed(seed, headers)` / `}` / `if seed := codexPromptCacheSeedFromHeaders(headers); seed != "" {`
- 差异/错误：B 的 `extractPromptCacheSeed` 依次识别 `X-Claude-Code-Session-Id`、Codex 头、`X-Session-Id`/`Session-Id`/`X-Conversation-Id`/`X-Client-Session-Id`/`X-Grok-Conv-Id`，再从 body 读 `prompt_cache_key`、`conversation_id`/`conversationId`、`session_id`/`sessionId`、`metadata.session_id`/`metadata.user_id`（含 `_session_` 后缀解析）、`client_metadata["x-codex-turn-metadata"]`、`client_metadata["x-codex-window-id"]`（`prompt_cache.go:31-76`）。A 只认 4 个头 + body `prompt_cache_key`，且 `session.Replay` 仅在"显式会话"（`explicitSession`，`session_state.go:64`）时为真——因为 A 把 Replay 与 `explicitSession` 绑定。
- 影响：Claude Code（`X-Claude-Code-Session-Id`）与 Codex（`session_id`/`x-codex-window-id`）客户端在 A 上永远拿不到显式会话：`session.Replay=false` → 不注入也不捕获 reasoning 回放（`handler_responses.go:214-216`、`handler_responses_store.go:120`），账号亲和在多轮间漂移，`cached_tokens` 归零。B 对这些客户端正常工作。
- 修复：把 `extractPromptCacheSeed` 的头部/body 枚举移植到 A 的 `prepareGrokSession` 调用点（`HandleResponses`、`HandleChatCompletions`），并保持 `Replay` 仅由显式信号驱动（B 的 `soft` 身份不驱动 replay，`prompt_cache.go:22-24`）。

### A3-7 [P1] 缺少网关侧 compaction（compaction_trigger / TUI 摘要 / g2a_compact_v1 blob）

- 本项目：`internal/grok/handler_responses_store.go:167` — `payload["stream"] = false` / `h.handleNativeCLIResponsesAt(w, r, modelID, spec, payload, "/responses/compact", false)`
- grok2api：`B:backend/internal/application/gateway/service.go:514` — `switch classifyResponsesCompactionRequest(input.Body) {` / `case responsesCompactionTrigger:` / `input.Operation = audit.OperationCompaction`；以及 `B:backend/internal/infra/provider/cli/responses_compaction.go:46` — `func (c *gatewayCompactionCodec) encode(session, summary string) (string, error) {` / `if c == nil || c.cipher == nil {` / `return "", fmt.Errorf("compaction codec unavailable")`
- 差异/错误：B 会对**普通 POST /responses** 做 compaction 分类（`compaction_trigger` input 项，或最后一条 user 项含 canonical compaction prompt marker，`responses_compaction.go:11-44`），然后由网关自己发 canonical 摘要请求（`prepareGatewayCompactionSample`，`responses_compaction.go:140-170`）、清洗摘要（`cleanGatewayCompactionSummary`）、加密成 `g2a_compact_v1.<cipher>` blob 再返回（`responses_compaction.go:46-62`），并在后续请求里把自家 blob 展开为 developer/user 文本（`expandGatewayCompactionHistory`，`responses_compaction.go:95-135`）。A 只有"把 `/responses/compact` 转发上游"这一条路径，且 `grep -rn "compaction_trigger\|g2a_compact\|expandGatewayCompaction" internal/` 无命中；A 对 `compaction` 类型 input 项只在回放剥离时保留（`session_state.go:376-379`）。
- 影响：Codex remote-v2 压缩（`compaction_trigger`）在 A 上不是交给网关处理，而是把触发项透传给 Build；上游返回的压缩 blob 与 B 的 `g2a_compact_v1` 命名空间不兼容，跨账号回放和多轮续写行为与参考实现分叉，长会话在 A 上会退化为"客户端自己压缩"。
- 修复：移植 `application/gateway/responses_compaction.go` 的分类与 `infra/provider/cli/responses_compaction*.go` 的 codec/forward（含 `responses_compaction_prompt.txt`），并在 `HandleResponses` 入口按分类分流；`HandleResponsesCompact` 也应走同一网关路径而非裸转发。

### A3-8 [P1] store 缺省语义不同：仅 store=true 才持久化，且未向上游强制 store=false

- 本项目：`internal/grok/responses_channel_bridge.go:331` — `func storeRequested(req ResponsesCreateRequest) bool {` / `return req.Store != nil && *req.Store` / `}`
- grok2api：`B:backend/internal/infra/provider/cli/normalize.go:184` — `if raw, exists := payload["store"]; !exists || isEmptyJSON(raw) {` / `payload["store"] = mustJSON(false)` / `changed = true`
- 差异/错误：两处不同。(1) 上游侧：B 在 Build 所有 operation 上把缺省/null 的 `store` 置为 `false`（`applyBuildResponseDefaults`，由 `normalizeBuildRequestPayloadWithMetadata:125` 调用），注释明确这是 ZDR 安全默认；A 的 `normalizeBuildResponsesPayload` 完全不碰 `store`，原生路径把客户端的 `store`（含缺省即不发送）原样转给上游。(2) 本地侧：A 只在 `store == true` 时写库（chat 桥：`storeRequested`；桥流式回调还额外要求 `status == "completed"`，`responses_channel_bridge.go:366-373`），而 B 对**任何**成功（2xx）的 Responses 请求都写 response ownership（`service.go:1131`），与其 own 的 `store` 取值无关。
- 影响：依赖 OpenAI "`store` 缺省为 true" 语义的客户端在 A 的 chat 桥路径上无法使用 `previous_response_id` 与 `GET /responses/{id}`（返回 `response_not_found`），在 B 上可用；上游数据留存策略也与参考实现相反。
- 修复：在 `normalizeBuildResponsesPayload` 中补 `store=false` 缺省（保持显式 `store=true` 为调用方选择）；把本地持久化改为"成功即记录所有权"（与 `store` 解耦），`store=false` 只影响是否回放给客户端。

### A3-9 [P2] include 未默认补 reasoning.encrypted_content，原生多轮 reasoning 回放静默失效

- 本项目：`internal/grok/handler_responses.go:211` — `nativePayload["prompt_cache_key"] = session.Key` / `if session.Replay {` / `h.applyNativeReasoningReplay(req.Model, session.Key, nativePayload)` / `}`
- grok2api：`B:backend/internal/infra/provider/cli/normalize.go:196` — `if value == "reasoning.encrypted_content" {` / `return changed, nil` / `}` / `includes = append(includes, "reasoning.encrypted_content")` / `payload["include"] = mustJSON(includes)`
- 差异/错误：`ensureReasoningEncryptedInclude`（`internal/grok/session_state.go:345-353`）只在**已有缓存可回放**时被调用，而 `responsesPayloadFromChat` 里的 include 补全（`responses_normalize.go:173-177`）只覆盖 chat→Responses 桥。原生 Build Responses 首轮请求因此不带 `include: reasoning.encrypted_content`，上游不返回 `encrypted_content`，`normalizeReplayItems` 因缺 anchor 返回 false → `captureReasoningReplayItems` 走 `clearReasoningReplay`（`reasoning_replay_capture.go:37-44`）。B 对所有 Build 请求默认补该 include。
- 影响：原生 Responses 多轮（Codex 每轮重发全量历史、不带 `include`）在 A 上永远无法建立 reasoning 回放链，与 B 行为不一致；只有客户端显式请求 encrypted content 才生效。
- 修复：在 `HandleResponses` 的 native 分支与 `handleNativeCLIResponsesAt` 中无条件补 `include: ["reasoning.encrypted_content"]`（去重）。

### A3-10 [P2] 上游会话头取值不同：64 字符 hex 摘要 vs UUID

- 本项目：`internal/grok/cli.go:238` — `if session, _ := payload["prompt_cache_key"].(string); strings.TrimSpace(session) != "" {` / `headers.Set("x-grok-session-id", strings.TrimSpace(session))` / `headers.Set("x-grok-conv-id", strings.TrimSpace(session))`（值来自 `session_state.go:92-93` 的 `sha256` hex，64 字符）
- grok2api：`B:backend/internal/infra/provider/cli/adapter.go:1077` — `if parsed, err := uuid.Parse(key); err == nil {` / `return parsed.String(), nil` / `}` / `return uuid.NewHash(sha256.New(), uuid.NameSpaceURL, []byte("grok2api:session:"+key), 8).String(), nil`
- 差异/错误：B 把会话键规范为 UUID（已是 UUID 则透传，否则 UUIDv5），且 `prompt_cache_key` 写入 body 的也是同一 UUID（`injectPromptCacheKey`，`adapter.go:1083-1097`）；A 直接把 sha256 hex 当作会话 id 写入 body 与两个上游头。B 同时只在"存在稳定会话"时设置这两个头（`adapter.go:1026-1037` 注释），A 一律设置。
- 影响：`x-grok-conv-id` / `x-grok-session-id` 的格式与官方 CLI 不一致，上游可能不识别或拒绝，从而影响会话亲和与 `cached_tokens`；A 的 64 字符值也不可读，排障时无法与客户端 session id 对应。
- 修复：按 B 的 `grokSessionID`（UUID 规范化）生成上游会话 id，与 `prompt_cache_key` 的写入值保持同一来源。

### A3-11 [P2] 缺少 CLI 身份/追踪/模型覆盖头，却带上了 web 平面的 x-xai-request-id

- 本项目：`internal/grok/cli.go:88` — `h.Set("Accept-Encoding", "gzip")` / `h.Set("User-Agent", c.userAgent())` / `h.Set("x-xai-request-id", randomUUID())`
- grok2api：`B:backend/internal/infra/provider/cli/adapter.go:1032` — `req.Header.Set("x-authenticateresponse", "authenticate-response")` / `req.Header.Set("x-grok-agent-id", a.agentID)` / `req.Header.Set("x-grok-session-id", sessionID)` / `req.Header.Set("x-grok-conv-id", sessionID)` / `req.Header.Set("x-grok-req-id", requestID)`
- 差异/错误：B 的 Build CLI 身份头包含 `x-authenticateresponse`、进程级持久的 `x-grok-agent-id`、每请求 `x-grok-req-id`、`traceparent`，并对所有 traced 请求设置 `x-grok-model-override: <model>`（`adapter.go:1064-1066`）。A 的 `cliHeaders` 完全没有这些头，反而设置了 web 平面专用的 `x-xai-request-id`（B 只在 `infra/provider/web/headers.go:22` 使用）；A 的 `x-grok-model-override` 只在 `/videos/` 路径设置（`cli.go:234-236`、`284`）。
- 影响：Build 上游看到的是非官方 CLI 身份头组合，模型覆盖仅靠 body；被上游以 `x-grok-agent-id` 缺失/身份不一致做风控或限流时无法复现 B 的成功路径。
- 修复：在 `cliHeaders`/`doResponsesOnceAt` 中补齐 `x-authenticateresponse`、`x-grok-agent-id`（进程级 UUID）、`x-grok-req-id`、`traceparent` 与通用 `x-grok-model-override`，移除 CLI 平面的 `x-xai-request-id`。

### A3-12 [P2] 非流式 Build Responses 上限 8 MiB，B 为 128 MiB

- 本项目：`internal/grok/handler_responses_store.go:308` — `raw, readErr := io.ReadAll(io.LimitReader(body, (8<<20)+1))` / `if readErr != nil {` … / `if len(raw) > 8<<20 {`
- grok2api：`B:backend/internal/infra/provider/cli/responses_response.go:13` — `maxCompatibleResponseBytes        = 128 << 20`；`B:backend/internal/infra/provider/cli/adapter.go:451` — `data, readErr := io.ReadAll(io.LimitReader(resp.Body, maxCompatibleResponseBytes+1))`
- 差异/错误：A 的 `copyNativeCLIResponseAndCaptureModel` 与 `newBoundedResponseCapture(8 << 20)` 都把单次响应硬上限设为 8 MiB，超过即回 `502 upstream_error "Upstream response unavailable"`；B 在 Responses 非流式路径上用 128 MiB。A 的 128 MiB 常量 `maxBuildAliasResponseBytes`（`responses_alias.go:12`）只在别名重写路径生效。
- 影响：长上下文 / 大工具输出的非流式 Responses 请求在 A 上被误判为上游故障（502），客户端会按可重试错误重发，放大上游压力。
- 修复：把该路径上限统一为 128 MiB，并复用同一常量；错误改为明确的 `response_too_large` 而不是 `upstream_error`。

### A3-13 [P2] 函数调用 arguments 的整型数字未修复（60000.0）

- 本项目：`internal/grok/responses_alias.go:104` — `restoreBuildVisibleTools(payload, aliases)` / `rewriteBuildToolAliasValue(payload, aliases)` / `converted, err := json.Marshal(payload)`（`rewriteBuildToolAliasValue` 只改 `type`/`name`，见 `responses_alias.go:144-163`；A 的 `collectBuildToolAliases` 也不记录 schema：`responses_normalize.go:71-76`）
- grok2api：`B:backend/internal/infra/provider/cli/responses_response.go:358` — `if schema := c.functionSchemas[alias]; schema != nil {` / `if arguments, ok := call["arguments"].(string); ok {` / `normalized, changed := normalizeFunctionArguments(arguments, schema)`
- 差异/错误：B 在响应侧（流式 `responses_response.go:191-205`、非流式 `responses_response.go:358-365`）按工具 schema 递归地把"语义上是整数"的 JSON 数字（`60000.0`、`1e3`）重写为整型字面量（`responses_arguments.go:19-188`，含指数与精度边界处理），因为 Codex 的严格解码器会拒绝为整型字段传浮点。A 只有 web 平面的文本 `<tool_call>` 修复（`internal/grok/tool_call.go:173-189`），Build/Responses 路径不做任何 arguments 规范化（`grep -rn "UseNumber\|normalizeIntegralNumber" internal/grok/` 无命中）。
- 影响：Build 返回 `60000.0` 时 Codex 侧工具调用参数解析失败或类型不符，表现为工具调用被拒/重试；B 已修复该常见形态。
- 修复：移植 `responses_arguments.go` 的 `normalizeFunctionArguments` 与 `resolveLocalSchemaRef` 依赖，在 A 记录工具 schema 后对 `function_call.arguments`（流式 done 与非流式 output）做规范化。

### A3-14 [P2] GET /responses/{id}/input_items 对 Build 记录只返回本轮输入（B 无此端点，作为契约对照）

- 本项目：`internal/grok/handler_responses_store.go:135` — `// lets GET /responses/{id}/input_items answer locally instead of doing a` / `// second upstream round trip for data it already had.` / `InputItems: responsesInputItemsJSON(payload["input"])`
- grok2api：`B:backend/internal/transport/http/inference/handler.go:109` — `router.POST("/responses/compact", h.compactResponse)` / `router.GET("/responses/:responseId", h.getResponse)` / `router.DELETE("/responses/:responseId", h.deleteResponse)`（B 未注册 `/cancel` 与 `/input_items`，故此处以 B 的路由表与上游代理语义为对照）
- 差异/错误：A 在 native Build 路径保存的是 `payload["input"]`，即**客户端本轮发送的 input**；当客户端使用 `previous_response_id` 时，历史（上一轮 input 与 assistant output）只存在于上游，不在 `InputItems` 里。而 `HandleResponseResource` 在 provider 判定**之前**就把 `input_items` 分派给本地子资源处理器（`internal/grok/handler_responses_store.go:178-181`），因此 Build 记录永远不会回源上游，`input_items` 只能给出残缺历史。相对而言，chat 桥路径保存的是展开后的 input（`handler_responses.go:341-343`），两条路径语义不一致。
- 影响：按 OpenAI 文档"读回 input_items 再作为下一轮 input 发送"的客户端会丢失整段历史，多轮上下文被截断；同一 gateway 上不同 provider 的 `input_items` 含义不同。
- 修复：Build 记录要么把 `previous_response_id` 链展开后一并持久化（与 chat 桥一致），要么在 `input_items` 命中 Build 记录且 `InputItems` 不含历史时改为向上游 `GET /responses/{id}/input_items` 代理，并统一 `has_more`/`first_id`/`last_id` 语义。
