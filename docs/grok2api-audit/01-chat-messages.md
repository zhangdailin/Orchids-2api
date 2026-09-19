# Chat Completions / Messages 对比

## 结论摘要
- 本项目把 Anthropic Messages 先降级成 OpenAI Chat、再交给 Chat 处理链（`handler_messages.go:78-111`），因此 Chat 侧的校验/结构假设被强加到 Messages 面上；最严重的三处（tool_result 数组、document、托管 web_search 的 tool_choice）会在 `Validate()` 阶段直接 400。
- Chat 面只接受 `type="function"` 工具（`types.go:576-579`），B 支持 `web_search*` 与透传其它服务端工具（`chat_request.go:280-294`）；`web_search_options` 本项目完全没有实现。
- 响应侧：本项目丢弃上游 response id/created（自行生成 `chatcmpl_*`），Anthropic 非流式响应的 `id` 因此不是 `msg_*`；usage 缺 `cache_creation_input_tokens`/`thinking_tokens`，refusal 不映射为 `refusal`/`content_filter`，错误类型恒为 `invalid_request_error`。
- 参数侧：`stop` 被同时下发上游（B 只在本地截断并回填 `stop_sequence`）；Build 平面缺 `store=false` 默认；`reasoning.summary` 在 Anthropic 路径完全不下发。
- 漂移说明：以上差异均**不是** 44a390b8→906b9493 的漂移造成的。所引用的 B 行为在 44a390b8 已存在（`git show 44a390b8:backend/internal/infra/provider/conversation/messages_request.go` 第 10/57/106/461/800 行、`chat_request.go` 第 26/58/162/167 行），且 `cli/normalize.go` 在 44a390b8..HEAD 之间零改动；44a390b8..HEAD 仅有 5 个提交触碰 conversation/，全部集中在 reasoning replay。
- 合计 20 项：P0×1、P1×3、P2×10、P3×6。

## 发现

### A1-1 [P0] Anthropic `tool_result` 的数组内容在 Chat 校验层被 400 拒绝
- 本项目：`internal/grok/util_messages.go:239` — `} else if blockType != "text" && !((role == "assistant" || role == "tool") && blockType == "image_url") {`
  以及 `internal/grok/handler_messages.go:602` — `parts = append(parts, map[string]interface{}{"type": "input_text", "text": fmt.Sprint(block["text"])})`
- grok2api：`B:backend/internal/infra/provider/conversation/messages_request.go:593` — `parts = append(parts, map[string]any{"type": "input_text", "text": value})`（直接写入 `function_call_output`，无 role 内容白名单）
- 差异/错误：Anthropic `tool_result.content` 为数组时，本项目把它转成 `{"type":"input_text"}` 并作为 **role="tool"** 的 Chat 消息内容；而 Chat 校验只允许 tool/assistant 的 `image_url`，`input_text`/`input_image`/`input_file` 一律报错 `the 'tool' role only supports 'text' type, got 'input_text'`。Messages 永远走 `HandleChatCompletions`→`req.Validate()`（`handler_chat.go:198`），所以必然 400。仅有 `content` 为纯字符串时才通过。
- 影响：Claude Code / Anthropic SDK 常见的 `tool_result` 数组形式（文本块、图片块、MCP 返回）100% 返回 400，工具调用链路完全不可用；B 的 `conversation_test.go:417-425`（`TestConvertAnthropicClaudeCodeRequestToResponses`）正是 `tool_result.content` 为 `[text, tool_reference, image]` 数组的 Claude Code 请求，B 转换成功。
- 修复：不要复用 Chat 的 `validateChatMessages` 校验 Messages 转换结果（例如给内部转发的 Chat 请求打标绕过，或为 tool 角色放行 `input_text`/`input_image`/`input_file`）；理想做法是让 Messages 直接产出 Responses input，而不是经 Chat 中转。

### A1-2 [P1] Anthropic `document` 块被 400 拒绝（`input_file` 不在用户内容白名单）
- 本项目：`internal/grok/util.go:114-118` — `userContentTypes = map[string]struct{}{ "text": {}, "image_url": {}, "input_audio": {}, "file": {} }`
  以及 `internal/grok/handler_messages.go:573` — `out := map[string]interface{}{"type": "input_file", "file_url": url}`
- grok2api：`B:backend/internal/infra/provider/conversation/messages_request.go:281-283` — `case "document":`
  同处 `B:.../messages_request.go:283` — `document, err := anthropicDocument(block)`、`B:.../messages_request.go:287` — `messageParts = append(messageParts, document)`（`anthropicDocument` 见第 530-569 行，支持 text/url/base64）
- 差异/错误：本项目把 `document` 转成 `input_file`，但 `userContentTypes` 白名单里没有 `input_file`（只有 Chat 原生的 `file`），于是 `validateChatMessages`（`util_messages.go:235-238`）直接报 `invalid content block type: 'input_file'`。讽刺的是 `responsesMessageParts` 里反而实现了 `input_file` 分支（`responses_normalize.go:328-342`），说明是本项目内部不一致。
- 影响：Anthropic 文档/PDF 输入（url / base64 / text source）在 Chat 与 Messages 两个入口都不可用；B 完整支持 `text`/`url`/`base64` 三种 `document.source`。
- 修复：把 `input_file` 加入用户内容白名单（或让 Messages 路径跳过 Chat 白名单校验），并让 `input_file` 在 `validateChatMessages` 的字段级校验中允许 `file_url`/`file_data`。

### A1-3 [P1] Anthropic `tool_choice:{"type":"tool","name":"web_search"}` 与 `{"type":"any"}` 被 400 拒绝
- 本项目：`internal/grok/types.go:664-666` — `if !found { return fmt.Errorf("tool_choice.function.name must reference a defined tool") }`
  以及 `internal/grok/types.go:644-646` — `case "required": if len(r.Tools) == 0 { return fmt.Errorf("tool_choice required needs at least one defined tool") }`
- grok2api：`B:backend/internal/infra/provider/conversation/messages_request.go:869-870` — `if hasHostedWebSearch && strings.EqualFold(strings.TrimSpace(choice.Name), "web_search") {`
  同处 `B:.../messages_request.go:868` — `// When only one hosted tool remains, Grok Build accepts only required tool_choice.`
- 差异/错误：本项目把 Anthropic `tool_choice` 转成 OpenAI 形状的 `{"type":"function","function":{"name":...}}`（`handler_messages.go:661`），而托管 `web_search` 工具被放进 `ResponsesTools` 而非 `Tools`（`handler_messages.go:142-148`）。Chat 校验只在 `r.Tools` 里查找该名字，因此 `tool_choice=tool+web_search` 报“必须引用已定义工具”；`tool_choice=any` + 仅声明托管 `web_search` 时也因 `len(r.Tools)==0` 报错。B 明确为这两种 Claude Code 形态做了特判。
- 影响：Claude Code 的 WebSearch（次级检索）流程与本项目的 Anthropic 入口完全不通；`tool_choice:any` + 仅服务端工具同样 400。
- 修复：`Validate()` 需要把 `ResponsesTools`（含 `web_search`）计入 `tool_choice` 的引用集合，或在 Messages→Chat 转换时把托管工具排除在严格校验之外（对齐 B 的 `hasHostedWebSearch` 特判）。注意 `compat_parity_test.go:243-249` 断言 Chat 面必须严格，因此修复应限定在 Messages 路径。

### A1-4 [P1] Chat `tools` 只接受 `function`；`web_search` 工具与 `web_search_options` 直接 400 / 丢失
- 本项目：`internal/grok/types.go:576-579` — `for i, tool := range tools { if !strings.EqualFold(strings.TrimSpace(tool.Type), "function") { return fmt.Errorf("tools.%d.type must be function", i) } }`
  以及 `internal/grok/console.go:70-72` — `if !strings.EqualFold(strings.TrimSpace(tool.Type), "function") { continue }`
- grok2api：`B:backend/internal/infra/provider/conversation/chat_request.go:285` — `case "web_search", "web_search_preview", "web_search_preview_2025_03_11", "web_search_2025_08_26":`
  以及 `B:.../chat_request.go:280` — `if typeName != "function" {`（第 291-292 行把其余非 function 工具原样透传）、`B:.../chat_request.go:62-63` — `if !isEmptyJSON(source["web_search_options"]) && !containsToolType(tools, "web_search") {` / `tools = append(tools, map[string]any{"type": "web_search"})`
- 差异/错误：本项目 `Validate()`（`handler_chat.go:198`）在路由之前就要求所有工具 `type="function"`，OpenAI 风格的 `{"type":"web_search"}`/`web_search_preview*` 直接 400；即便绕过（例如从 `ResponsesTools` 进入）`consoleToolsFromOpenAI` 也会把非 function 工具静默丢弃。`web_search_options` 在本项目全部源码中不存在（grep 无命中）。
- 影响：服务端联网搜索这一常用能力在 OpenAI Chat 面不可达；声明 `web_search` 的客户端得到的不是“无搜索”而是 400，迁移成本高。B 同时支持 `web_search_options` 与显式 web_search 工具，并校验 allowed/excluded domains 冲突。
- 修复：`validateToolDefinitions` 区分“Chat 兼容面”与“原生 Responses 面”，放行并转发 `web_search*`（映射为 `{"type":"web_search"}`）；实现 `web_search_options` → `web_search` 工具的隐含追加。

### A1-5 [P2] Anthropic thinking 请求不下发 `reasoning.summary`（B 明确 `detailed`，Build 平面还被 `sourceOperation` 跳过默认）
- 本项目：`internal/grok/responses_normalize.go:198` — `if req.sourceOperation == "" && (req.ReasoningEffort == nil || *req.ReasoningEffort != "none") {`
  以及 `internal/grok/responses_normalize.go:667` — `reasoning["effort"] = effort`
- grok2api：`B:backend/internal/infra/provider/conversation/messages_request.go:119` — `target["reasoning"] = map[string]any{"effort": effort, "summary": "detailed"}`
- 差异/错误：Messages 路径把 `sourceOperation` 设为 `"messages"`（`handler_messages.go:197`），因此 Build 平面那唯一会补 `summary:"concise"` 的分支被显式跳过；同时 `chatReasoningControls` 只写 `effort`。结果 Anthropic 请求的 `reasoning` 对象永远没有 `summary`。B 对 thinking 请求固定请求 `detailed` 摘要。
- 影响：上游是否返回 `reasoning.summary` 取决于请求的 summary 级别；本项目在只提供摘要（Build 官方客户端默认行为，见本项目自己的注释 `responses_normalize.go:193-197`）时拿不到 thinking 文本，Anthropic `thinking` 块会为空、只剩签名；与 B 的协议行为不一致。
- 修复：Messages 转 Chat 时把 `reasoning_summary="detailed"` 一并写入（`ChatCompletionsRequest.ReasoningSummary` 字段已存在，只是 Messages 路径从不设置）。

### A1-6 [P2] `thinking.type="disabled"` 被 `output_config.effort` 覆盖
- 本项目：`internal/grok/handler_messages.go:369-371` — `if effort := strings.ToLower(strings.TrimSpace(fmt.Sprint(outputConfig["effort"]))); effort != "" && effort != "<nil>" { return &effort }`
- grok2api：`B:backend/internal/infra/provider/conversation/messages_request.go:44` — `reasoningEffort = "none"`
  以及 `B:.../messages_request.go:102` — `if thinkingEnabled {`（`output_config.effort` 只在该分支内生效，见第 107-109 行）
- 差异/错误：本项目先看 `output_config.effort`，只有在它为空时才看 `thinking.type`；因此 `{"thinking":{"type":"disabled"},"output_config":{"effort":"high"}}` 会被解析成 effort=high（即开启推理）。B 的优先级相反：`disabled` ⇒ `none`，且 `output_config.effort` 被忽略。
- 影响：客户端显式关闭思考的请求可能仍然产生推理 token（计费/延迟/行为不一致），与 B 语义相反。
- 修复：先处理 `thinking.type`（`disabled` ⇒ `none` 并短路），只在 thinking 启用时才采纳 `output_config.effort`。

### A1-7 [P2] Anthropic usage 缺 `cache_creation_input_tokens`、`output_tokens_details.thinking_tokens`；非流式缺 `server_tool_use`
- 本项目：`internal/grok/handler_messages.go:794` — `result["cache_read_input_tokens"] = cached`（`result` 只写入 `input_tokens`/`output_tokens`，见第 786-789 行）
  以及 `internal/grok/handler_messages.go:1066` — `s.usage["server_tool_use"] = map[string]interface{}{"web_search_requests": len(s.searches)}`（仅流式 `finish()` 内）
- grok2api：`B:backend/internal/infra/provider/conversation/messages_response.go:114` — `"cache_creation_input_tokens": 0, "cache_read_input_tokens": cacheReadInputTokens,`
  以及 `B:.../messages_response.go:118` — `"output_tokens_details":      map[string]any{"thinking_tokens": thinkingTokens},`、`B:.../messages_response.go:126-127` — `if webSearchRequests > 0 {` / `usage["server_tool_use"] = map[string]any{"web_search_requests": webSearchRequests}`
- 差异/错误：本项目只产出 `input_tokens`/`output_tokens`/`cache_read_input_tokens`，从不产出 `cache_creation_input_tokens` 与 `output_tokens_details.thinking_tokens`；`server_tool_use` 只在流式 `finish()`（`handler_messages.go:1065-1067`）追加，非流式 `anthropicResponseFromChat` 完全不带。
- 影响：Anthropic 客户端/计费侧读取 `cache_creation_input_tokens`、`thinking_tokens` 会拿到缺失值（Claude Code 的 /cost、缓存统计失真）；同一请求流式与非流式 usage 形状不一致。B 两种情况都输出完整字段。
- 修复：在 `anthropicUsageFromOpenAI` 中补齐 `cache_creation_input_tokens:0`、`output_tokens_details.thinking_tokens`（可由 `completion_tokens_details.reasoning_tokens` 换算），并把 `server_tool_use` 一并放入非流式 usage。

### A1-8 [P2] refusal 不映射为 `stop_reason="refusal"` / `finish_reason="content_filter"`
- 本项目：`internal/grok/console.go:589` — `finishReason := "stop"`（第 590-596 行只按 tool_calls 改写，refusal 不参与）
  以及 `internal/grok/handler_messages.go:777` — `case "stop", "end_turn", "", "<nil>": return "end_turn"`
- grok2api：`B:backend/internal/infra/provider/conversation/chat_response.go:24-25` — `} else if value.Refusal != "" { finishReason = "content_filter" }`
  以及 `B:.../messages_response.go:47-48` — `} else if value.Refusal != "" { stopReason = "refusal" }`
- 差异/错误：本项目把 refusal 作为普通文本块输出（`handler_messages.go:720-722`），`finish_reason` 仍是 `stop`，`openAIFinishToAnthropic` 也不认识 `content_filter`/`refusal`，一律回落到 `end_turn`。B 在 Chat 面用 `content_filter`、Messages 面用 `refusal`。
- 影响：安全拒绝在 Chat 面无法与正常完成区分；Anthropic 客户端（及依赖 `stop_reason=refusal` 的上层）会把拒答当正常回答处理。B 的行为可区分。
- 修复：`collectConsoleChat`/`streamConsoleChat` 在 `refusal != ""` 且无工具调用时置 `finish_reason="content_filter"`，并让 `openAIFinishToAnthropic` 映射为 `refusal`。

### A1-9 [P2] `stop` 序列被下发上游，导致 `stop_sequence` 可能永远无法回填
- 本项目：`internal/grok/responses_normalize.go:159-161` — `if len(req.Stop) > 0 { payload["stop"] = append([]string(nil), req.Stop...) }`
- grok2api：`B:backend/internal/infra/provider/conversation/chat_request.go:38-41` — `stopSequences, err := parseChatStopSequences(source["stop"]) ...`（`stop` 只进 `ResponseOptions`，从不进 `target`）
  以及 `B:.../response.go:148` — `parsed.Text, parsed.StopSequence = applyStopSequences(parsed.Text, options.StopSequences)`
- 差异/错误：本项目把非标准的 `stop` 字段写进 Responses 上游请求体，同时又在本地用 `stopFilter` 截断（`console_stream.go:116`）。若上游接受并自行在 stop 处截断，客户端流里就看不到被匹配的 token，`filter.matched` 为空，`message["stop_sequence"]` 不下发、`stop_reason` 回落为 `end_turn`。B 全程本地截断，因此总能回填 `stop_sequence`。附带风险：上游 Responses 契约未必接受 `stop`（[待验证]：缺少 Build/Console 对未知字段的实际响应证据）。
- 影响：`stop` + Anthropic `stop_sequences` 场景下 `stop_reason`/`finish_reason` 可能错误；给上游多传一个契约外字段还有被拒风险。
- 修复：不要把 `stop` 放进上游 payload，只保留本地 `stopFilter` 截断与回填（与 B 一致）。

### A1-10 [P2] 响应 id 重新生成：Anthropic 非流式响应 id 为 `chatcmpl_*`，上游 id/created 被丢弃
- 本项目：`internal/grok/console.go:612` — `"id": firstNonEmpty(interfaceString(raw["id"]), "chatcmpl_"+randomHex(8)),`
  以及 `internal/grok/handler_messages.go:748` — `"id": firstNonEmpty(interfaceString(chat["id"]), "msg_"+randomHex(12)),`
- grok2api：`B:backend/internal/infra/provider/conversation/chat_response.go:35` — `id := strings.Replace(value.ID, "resp_", "chatcmpl_", 1)`
  以及 `B:.../messages_response.go:81-83` — `if strings.HasPrefix(value, "resp_") { return "msg_" + strings.TrimPrefix(value, "resp_") }`
- 差异/错误：本项目原生 Chat 路径（Console/Build）不保留上游 `resp_*` id，而是生成随机 `chatcmpl_*`（流式同样，`console_stream.go:108`）；Messages 非流式直接复用该 id，于是 Anthropic `message.id` 变成 `chatcmpl_xxx` 而不是 `msg_xxx`。`created` 也用 `time.Now()` 而非上游 `created_at`。
- 影响：Anthropic 协议 id 形态不符（不以 `msg_` 开头）；上游响应 id 丢失使日志/追踪与 `previous_response_id` 关联断裂，B 可保持谱系。同样影响 tool_use id（本项目无 `toolu_` 归一化）。
- 修复：保留上游 id 并按协议前缀改写（`resp_`→`chatcmpl_` / `resp_`→`msg_`），仅在缺失时回退到随机 id；`created` 用上游 `created_at`。

### A1-11 [P2] Build 平面缺少 `store=false` 默认
- 本项目：`internal/grok/responses_normalize.go:214-215` — `normalizeBuildReasoningEffort(payload, model); return payload, nil`（build 分支全程不设置 `store`；只有 Console 分支在第 218 行 `payload["store"] = false`）
- grok2api：`B:backend/internal/infra/provider/cli/normalize.go:184-186` — `if raw, exists := payload["store"]; !exists || isEmptyJSON(raw) { payload["store"] = mustJSON(false); changed = true }`
  以及 `B:.../messages_request.go:72` — `"max_output_tokens": request.MaxTokens, "store": false,`
- 差异/错误：Chat 请求体里没有 `store` 字段（`types.go:14-45`），Messages 转换也不设置它；Build 分支因此向上游发送不带 `store` 的响应请求，而上游缺省可能是持久化。B 对所有 Build 请求补 `store=false`，Anthropic 路径无条件 `store:false`。`normalize.go` 自 44a390b8 起零改动，故非漂移。
- 影响：ZDR/隐私语义不一致：本项目在 Build 平面可能让上游保留响应与推理状态，与“无状态代理”的宣称不符。
- 修复：Build 分支补齐 `store=false`（Console 已有），并让 Messages 转换显式携带 `store:false`。

### A1-12 [P2] Anthropic 系统提示中的 `x-anthropic-billing-header` 未剥离，破坏上游缓存前缀
- 本项目：`internal/grok/handler_messages.go:422` — `if text := strings.TrimSpace(fmt.Sprint(block["text"])); text != "" {`
  同处 `internal/grok/handler_messages.go:423` — `parts = append(parts, text)`（无任何过滤；字符串形态在 416 行直接 `return strings.TrimSpace(v)`）
- grok2api：`B:backend/internal/infra/provider/conversation/messages_request.go:500-502` — `func isAnthropicBillingHeaderText(text string) bool { return strings.HasPrefix(strings.TrimLeft(text, " \t\r\n"), anthropicBillingHeaderPrefix) }`
  以及 `B:.../messages_request.go:492-494` — `// Claude Code identity blocks may carry a per-request cch fingerprint ... if isAnthropicBillingHeaderText(block.Text) { continue }`
- 差异/错误：Claude Code 会在 system 里带 `x-anthropic-billing-header: ...`（每请求变化）。B 在字符串与 block 两种形态下都丢弃它；本项目原样拼进 `instructions`，而 `instructions` 位于消息前缀最前，等于每轮都改变缓存前缀。
- 影响：上游 prompt cache 命中率下降（B 的注释明确说明这是剥离原因），推理成本/延迟上升；无隐私泄漏但属行为漂移。
- 修复：在 `anthropicSystemText` 中复用 B 的判定，跳过以 `x-anthropic-billing-header: ` 开头的文本块。

### A1-13 [P2] Anthropic 错误类型恒为 `invalid_request_error`，上游错误正文直通 `message`
- 本项目：`internal/grok/handler_messages.go:1105` — `"type": "error", "error": map[string]interface{}{"type": "invalid_request_error", "message": message},`
  以及 `internal/grok/handler_messages.go:1113` — `message := strings.TrimSpace(body)`（上游原始 body 直接作为 message）
- grok2api：`B:backend/internal/infra/provider/conversation/messages_response.go:155-157` — `case "invalid_request_error", "authentication_error", "billing_error", "permission_error", "not_found_error", "rate_limit_error", "timeout_error", "overloaded_error", "api_error": return strings.ToLower(strings.TrimSpace(value))`
- 差异/错误：本项目无论 HTTP 状态是 429/401/529 还是 400，错误体都写死 `invalid_request_error`，并把上游原始 body（可能是 HTML/网关 JSON）整体塞进 `message`；B 建立完整的 Anthropic 错误类型映射（并按 `type`/`code` 归一到 9 类）。
- 影响：Anthropic SDK 与 Claude Code 的退避/重试策略以 `error.type` 判断（`rate_limit_error` 退避、`overloaded_error` 重试）；本项目一律 `invalid_request_error` 会被当成客户端错误而不重试，且错误信息不可读。
- 修复：按状态码/上游 `type|code` 映射 Anthropic 错误类型（照搬 B 的 `normalizeAnthropicErrorType`），并对上游非 JSON body 做摘要化处理。

### A1-14 [P3] assistant 历史文本使用 `output_text`，B 统一归一化为 `input_text`［待验证］
- 本项目：`internal/grok/responses_normalize.go:296-299` — `textType := "input_text"; if assistant { textType = "output_text" }`
  以及 `internal/grok/handler_messages.go:247-250` — `partType := "input_text"; if role == "assistant" { partType = "output_text" }`
- grok2api：`B:backend/internal/infra/provider/conversation/chat_request.go:170-175` — `case "text", "input_text", "output_text": ... result = append(result, map[string]any{"type": "input_text", "text": value})`
  以及 `B:.../messages_request.go:273` — `messageParts = append(messageParts, map[string]any{"type": "input_text", "text": value})`
- 差异/错误：B 把输入侧所有文本块（含客户端传来的 `output_text`、含 assistant 文本）一律改写为 `input_text`，说明上游 Responses `input` 契约只保证 `input_text`；本项目对 assistant 历史发送 `output_text`，且不做归一化。缺少上游对 `input` 中 `output_text` 的实际接受度证据。
- 影响：多轮对话（含 assistant 历史）在 Build/Console 平面可能被上游拒绝或语义降级；至少与 B 的线协议不一致。
- 修复：输入侧统一输出 `input_text`（需要保留角色语义时用 role 字段区分），仅在 `output` 项里使用 `output_text`。

### A1-15 [P3] Chat `tool` 消息缺 `tool_call_id` 时用 function name 顶替，而不是报错
- 本项目：`internal/grok/responses_normalize.go:244` — `callID := firstNonEmpty(strings.TrimSpace(message.ToolCallID), strings.TrimSpace(message.Name))`
- grok2api：`B:backend/internal/infra/provider/conversation/chat_request.go:138-139` — `if strings.TrimSpace(message.ToolCallID) == "" { return nil, errors.New("tool 消息缺少 tool_call_id") }`
- 差异/错误：本项目把 `name` 当 `call_id` 使用，生成一个不存在的 `function_call_output`；B 直接拒绝该请求。注意 `validateChatMessages`（`util_messages.go:205-206`）已要求 tool 消息必须有 `tool_call_id`，因此该回退只对内部构造的请求（如 Messages 转换/工具历史）生效，属隐蔽分支。
- 影响：一旦进入该分支，上游收到无法配对的 `function_call_output`，模型会丢失工具结果或报参数错误；错误被静默吞掉而非显式 400。
- 修复：删除 `name` 回退，缺失 `tool_call_id` 时返回错误（与 B 一致）。

### A1-16 [P3] 同时给出 `max_tokens` 与 `max_completion_tokens` 时优先级相反
- 本项目：`internal/grok/types.go:401-404` — `if _, ok := rawMap["max_completion_tokens"]; ok {`
  同处 `internal/grok/types.go:402-404` — `r.MaxCompletionTokens = &maxCompletionTokens` / `if r.MaxTokens == nil {` / `r.MaxTokens = &maxCompletionTokens`（`max_tokens` 在第 398-400 行先赋值，故同时出现时 `max_tokens` 胜出）
- grok2api：`B:backend/internal/infra/provider/conversation/chat_request.go:42` — `if raw := firstJSON(source["max_completion_tokens"], source["max_tokens"]); !isEmptyJSON(raw) {`
- 差异/错误：两者同时出现时，本项目取 `max_tokens`，B 取 `max_completion_tokens`（OpenAI 已将 `max_tokens` 标记为弃用并建议改用 `max_completion_tokens`）。
- 影响：同时携带两个字段的客户端在本项目会得到较小的/过期的上限，输出长度行为与 B 不一致（单字段场景两者一致）。
- 修复：转换时优先 `max_completion_tokens`。

### A1-17 [P3] `metadata`/`service_tier` 未透传；`parallel_tool_calls` 仅在存在 tools 时下发
- 本项目：`internal/grok/responses_normalize.go:180-187` — `if len(tools) > 0 { payload["tools"] = tools; ... if req.ParallelToolCalls != nil { payload["parallel_tool_calls"] = *req.ParallelToolCalls } }`
- grok2api：`B:backend/internal/infra/provider/conversation/chat_request.go:30` — `copyFields(target, source, "stream", "temperature", "top_p", "parallel_tool_calls", "metadata", "store", "service_tier")`
- 差异/错误：B 无条件透传 `parallel_tool_calls`、`metadata`、`store`、`service_tier`；本项目仅在 `len(tools) > 0` 时下发 `parallel_tool_calls`，且 `ChatCompletionsRequest`（`types.go:14-45`）没有 `metadata`/`service_tier` 字段，因此这两者对所有平面被静默丢弃。
- 影响：无工具但显式 `parallel_tool_calls` 的请求参数被丢弃；依赖 `metadata`/`service_tier` 透传的上游特性（计费/优先级/审计标记）在本项目不可用。
- 修复：按 B 的字段清单透传；`parallel_tool_calls` 移出 `len(tools)>0` 分支。

### A1-18 [P3] Chat 图片 part 未补默认 `detail:"auto"`
- 本项目：`internal/grok/responses_normalize.go:318-325` — `part := map[string]interface{}{"type": "input_image", "image_url": url} ... if detail != "" { part["detail"] = detail }`
- grok2api：`B:backend/internal/infra/provider/conversation/chat_request.go:223-227` — `detail := "auto"; var directDetail string; if json.Unmarshal(part["detail"], &directDetail) == nil && strings.TrimSpace(directDetail) != "" { detail = strings.TrimSpace(directDetail) }`
  以及 `B:.../chat_request.go:218` — `return map[string]any{"type": "input_image", "detail": detail, "image_url": imageURL}, nil`
- 差异/错误：客户端未给 `detail` 时，B 显式发送 `detail:"auto"`，本项目省略该键（Anthropic `image` 块两边都不加 detail，B 在 tool_result 里才补 `"auto"`，见 `messages_request.go:599`）。
- 影响：省略 `detail` 时是否等价取决于上游默认值；若上游默认不是 auto，则分辨率/成本与 B 不一致。
- 修复：与 B 对齐，缺省填 `"auto"`。

### A1-19 [P3] 工具序列校验偏松：未强制 `tool_use` 配对、`tool_reference` 不校验已声明工具
- 本项目：`internal/grok/handler_messages.go:365` — `return nil`（`validateChatToolSequence` 结束时 `pending` 非空也不报错）
  以及 `internal/grok/handler_messages.go:613` — `if name := parseLooseStringAny(block["tool_name"]); name != "" {`（未检查工具是否已声明，空名静默跳过）
- grok2api：`B:backend/internal/infra/provider/conversation/messages_request.go:409-410` — `if len(pendingCalls) > 0 { return nil, nil, errors.New("messages 必须为每个 tool_use 提供 tool_result") }`
  以及 `B:.../messages_request.go:612-613` — `if _, exists := declaredTools[toolName]; !exists { return nil, fmt.Errorf("tool_reference 引用了未声明的工具 %q", toolName) }`
- 差异/错误：本项目允许存在没有 `tool_result` 的 `tool_use`（B 直接拒绝），并允许 `tool_reference` 指向未声明工具或空名（B 报错）。这是兼容性放宽而非崩溃，但对上游是非法历史。
- 影响：截断/损坏的 Anthropic 历史会被转发到上游，错误在下游表现为难以定位的上游 4xx，而不是清晰的本地 400。
- 修复：补上 `pendingCalls` 的终态校验与 `tool_reference.tool_name` 的声明集校验。

### A1-20 [P2] `messages[]` 内的 `role="system"`/`"developer"` 被 400 拒绝
- 本项目：`internal/grok/handler_messages.go:435-436` — `if role != "user" && role != "assistant" {`
  同处 `internal/grok/handler_messages.go:436` — `return nil, fmt.Errorf("unsupported message role %q", message.Role)`
- grok2api：`B:backend/internal/infra/provider/conversation/messages_request.go:225-226` — `if role == "system" || role == "developer" {`
  同处 `B:.../messages_request.go:231` — `instructions = append(instructions, text)`（并入 instructions 后 `continue`）
- 差异/错误：B 接受 `messages[]` 内出现的 system/developer 角色并把文本并入 `instructions`；本项目只接受 user/assistant，其他角色直接 400。B 的 Claude Code 兼容用例（`conversation_test.go:410-413`）就同时包含 `{"role":"system","content":"legacy system"}` 与 `role="developer"`。
- 影响：与 Claude Code 的历史载荷兼容性差：一旦客户端把 system/developer 放进 messages（B 明确支持），本项目整个请求 400，且报错信息无法定位到具体消息。
- 修复：按 B 的做法把 messages 内的 system/developer 文本并入 instructions（保持顺序），而不是报错。
