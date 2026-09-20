# Orchids-2api Grok 通道 × chenyme/grok2api 深度对比审计

> 修复进展见 `docs/grok2api-fix-ledger.md`。**截至第十轮的总账**：171 条中已修复 154 条、有意保留 17 条、未修 **0** 条（6 条 P0 全部关闭）。逐条对账表与复算脚本见台账第十二节与 `docs/grok2api-audit/recount.py`。审计期间对 A5-10 做了更正（见该条）。

- 审计对象 A（本项目）：`/home/zhangdailin/Documents/Orchids-2api`，Grok 通道实现位于 `internal/grok/`、`internal/handler/`、`internal/api/`、`internal/store/`、`internal/middleware/`、`cmd/server/`。
- 参考实现 B：`chenyme/grok2api`，Go 后端根目录 `backend/`，审计基线 **HEAD = `906b9493`（v3.1.6，2026-09-16）**，克隆在 `.upstream/grok2api/`（未纳入版本控制）。
- 移植基点：A 的三个文件标注 `Derived from chenyme/grok2api, commit 44a390b8…`（`internal/grok/grok2api_sse.go`、`grok2api_streamidle.go`、`grok2api_streamidle_test.go`）。基点与 HEAD 之间相隔 **19 个提交**，其中 5 个是行为修复（`22ac653a`/`72a3a347`/`5d19ccff`/`e5285ebe` 工具 schema 根联合展平，`8641a782`/`ca392e68` 多轮推理恢复，`6db9f67f`/`df4dde39` console 空 tools 时丢弃 tool_choice，`7f3f3d3c`/`50c09e26` 质量守卫转储阈值，`7d1b4246` 回放分配溢出）。凡属该漂移造成的差异，条目中均标注 `[漂移]`。
- 方法：按 9 个功能面并行做双侧逐行对照，每条发现都要求 A 与 B 双方 `file:line` + 1–5 行代码引用；无法给出代码证据的条目已剔除或标 `[待验证]`。证据细节见 `docs/grok2api-audit/01..09-*.md`。
- 结论基于源码静态对照（未运行 A/B 服务做线上抓包）；`go test ./internal/grok/...` 在审计时为通过（`ok orchids-api/internal/grok 6.130s`），即下述问题都不被现有测试覆盖。

统计（详见各章与附录索引）：**P0 × 6、P1 × 69、P2 × 75、P3 × 21**，合计 171 条；其中约 15 条是同一根因在不同功能面的重复记录，去重后独立缺陷约 156 条。

| 严重度 | 含义 |
| --- | --- |
| P0 | 安全缺陷（SSRF、凭据外泄、越权开关）或主功能链路硬 400 |
| P1 | 常见路径行为错误 / 与 grok2api 语义相反 / 能力缺口导致客户端整轮失败 |
| P2 | 边角行为、错误契约、数值常量、字段缺失 |
| P3 | 文案、命名、无害的响应字段差异 |

---

## 0. 横向结论（先读这一段）

1. **A 的原生 Build/Console 面是"半透传"，B 是"重兼容层"。** B 在 `cli/responses_*.go`（约 3.6k 行）、`conversation/*`、`console/normalize.go` 里承担请求降级、历史项归一化、工具别名与还原、流事件字段补齐、compaction、推理回放；A 只移植了工具别名（`responses_alias.go`、`responses_normalize.go`）与 stream-idle 语义，其余靠"原样转发 + 出错重试"。结果是：同一客户端（Codex / Claude Code / Grok TUI / pi）在 B 能跑通的多轮工具链，在 A 上会遇到硬 400、字段缺失、参数被静默丢弃。
2. **错误契约是第二大系统性差异。** B 的每个出口都是 `{"error":{"message","type","code","param"}}`（`inference/handler.go:2285`），并按类别细分 20 余个 `code`；A 的 Grok 通道大量使用 `http.Error`（`text/plain`）与写死的 `invalid_request_error`，且把上游错误正文/内部错误串拼进客户端可见的 `message`。
3. **凭据类上游失败的处置语义相反。** B 把上游 401/402/403 一律脱敏成 503 `upstream_unavailable` 并回写 `Retry-After`；A 把上游状态码直接当客户端状态码透出（401/403/402→429），导致下游把"账号池问题"误判成"自己的 Key 有问题"。
4. **模型命名体系不可互换。** A 把上游 Provider 前缀（`console/…`、`build/…`）当作公开模型 ID，且没有 reasoning-effort 后缀别名与注册式兼容别名；B 对外剥离前缀、合成 `grok-4.5-high` 这类档位名、并保留 14 条兼容别名。
5. **三处安全面是 A 独有、B 已封堵**：`image_url` 服务端直连抓取无 SSRF 防护（`util.go:446`）、媒体绝对地址取自可伪造的 `X-Forwarded-*`/`Host`（`detectPublicBaseURL`）、媒体资产下载把 SSO Cookie 发往 CDN 域。
6. **A 也有 B 没有的东西**：统一 `/v1` 前缀下的六通道聚合、Redis 多副本租约、账号级分布式限流、审计日志与脱敏、更多媒体端点形态。本报告只列"错误"，不评价架构选择。

---

## 1. Chat Completions / Anthropic Messages（20 条）

证据：`docs/grok2api-audit/01-chat-messages.md`。

### A1-1 [P0] Anthropic `tool_result` 数组内容被 Chat 校验层 400 拒绝

- A：`internal/grok/handler_messages.go:603` 把 `tool_result.content[].type=="text"` 转成 `{"type":"input_text"}`，整块作为 **role="tool"** 的 Chat 消息内容；`internal/grok/util_messages.go:236-240` 对 tool 角色只允许 `text`（外加 `image_url`），于是 `req.Validate()`（`handler_chat.go:198`）必然报 `the 'tool' role only supports 'text' type, got 'input_text'`。
- B：`backend/internal/infra/provider/conversation/messages_request.go` 直接把 Anthropic 请求转成 Responses `input`，`conversation_test.go:417-425` 正是 `tool_result.content` 为 `[text, tool_reference, image]` 数组的 Claude Code 请求，转换成功。
- 差异/错误：Messages 入口始终经 `HandleChatCompletions`（`handler_messages.go:101`）再走 Chat 校验，数组形态 `tool_result` 100% 400；只有纯字符串内容能通过。
- 影响：Claude Code / Anthropic SDK 最常见的工具结果形态（多块文本、图片、MCP 返回）完全不可用，工具链断裂。
- 修复：Messages 面改为直接产出 Responses `input`（不经 Chat 校验），或至少让 Chat 校验接受 tool 角色的 `input_text`/`input_image`。

### A1-2 [P1] Anthropic `document` 块被 400 拒绝

- A：`handler_messages.go:608` 把 `document` 转成 `input_file`，但 `util_messages.go` 的 `userContentTypes` 白名单里没有 `input_file`（只有 Chat 原生 `file`）→ `invalid content block type: 'input_file'`。同一文件在 `responses_normalize.go:328-342` 却有 `input_file` 分支，内部自相矛盾。
- B：`messages_request.go` 完整支持 `document.source` 的 `text`/`url`/`base64` 三种形态。
- 影响：Anthropic 文档/PDF 输入在 Chat 与 Messages 入口都不可用。
- 修复：把 `input_file` 加入 user 白名单并补齐 Chat→Responses 的文件映射。

### A1-3 [P1] `tool_choice:{"type":"tool","name":"web_search"}` 与 `{"type":"any"}` 被 400 拒绝

- A：`handler_messages.go:661` 把 Anthropic `tool_choice` 转成 OpenAI 形状，而托管的 `web_search` 工具落在 `ResponsesTools`（`handler_messages.go:142-148`）而非 `Tools`；Chat 校验只在 `Tools` 中查名（`types.go:665`），因此报"必须引用已定义工具"；声明仅服务端工具时 `tool_choice=any` 又因 `len(r.Tools)==0` 报错。
- B：为这两种 Claude Code 形态做了专门降级（`conversation/messages_request.go`）。
- 影响：Claude Code 的 WebSearch 流程、`tool_choice:any` + 服务端工具全部 400。
- 修复：校验时把 `ResponsesTools` 一并纳入工具名解析表。

### A1-4 [P1] Chat `tools` 只接受 `function`，`web_search` / `web_search_options` 不可达

- A：`types.go:576-579` 要求所有工具 `type=="function"`，OpenAI 风格 `{"type":"web_search"}` 直接 400；即便绕过，`consoleToolsFromOpenAI` 也会静默丢弃非 function 工具；全仓无 `web_search_options`（grep 无命中）。
- B：`conversation/chat_request.go:62-64,285` 同时支持 `web_search_options` 与显式 web_search 工具，并校验 allowed/excluded domains 冲突。
- 影响：Chat 面的服务端联网搜索能力缺失；客户端拿到的是 400 而不是"降级为无搜索"。
- 修复：接受 `web_search`/`web_search_preview*` 工具与 `web_search_options`，映射到 Build/Console 的 native search 工具。

### A1-5 [P2] Anthropic thinking 请求不下发 `reasoning.summary`

- A：`handler_messages.go:197` 把 `sourceOperation` 设为 `messages`，从而跳过 Build 平面唯一补 `summary:"concise"` 的分支（`responses_normalize.go:193-197`）；`chatReasoningControls` 只写 `effort`，于是 `reasoning` 永远没有 `summary`。
- B：对 thinking 请求固定请求 `detailed` 摘要。
- 影响：Anthropic `thinking` 块可能为空、只剩签名；与 B 协议行为不一致。
- 修复：Messages 路径显式请求 summary 级别。

### A1-6 [P2] `thinking.type="disabled"` 被 `output_config.effort` 覆盖

- A：先读 `output_config.effort`，为空才看 `thinking.type`；`{thinking:{type:"disabled"},output_config:{effort:"high"}}` 被解析成 effort=high。
- B：`disabled ⇒ none`，忽略 `output_config.effort`。
- 影响：显式关闭思考的请求仍产生推理 token（计费/延迟/行为）。
- 修复：反转优先级，`disabled` 为最高优先级。

### A1-7 [P2] Anthropic usage 字段缺失

- A：只产出 `input_tokens`/`output_tokens`/`cache_read_input_tokens`，无 `cache_creation_input_tokens`、无 `output_tokens_details.thinking_tokens`；`server_tool_use` 只在流式 `finish()`（`handler_messages.go:1065-1067`）追加，非流式完全没有。
- B：两种模式都输出完整字段集。
- 影响：Claude Code 的 `/cost`、缓存统计失真；同一请求流式/非流式 usage 形状不一致。
- 修复：补齐字段并统一流式/非流式形状。

### A1-8 [P2] refusal 未映射为 `stop_reason="refusal"` / `finish_reason="content_filter"`

- A：`handler_messages.go:720-722` 把 refusal 当普通文本块，`finish_reason` 仍是 `stop`，`openAIFinishToAnthropic` 不识别 `content_filter`/`refusal`，一律回落 `end_turn`。
- B：Chat 面 `content_filter`、Messages 面 `refusal`，可区分。
- 修复：建立 refusal → finish_reason/stop_reason 映射表。

### A1-9 [P2] `stop` 同时下发上游，`stop_sequence` 可能无法回填 `[待验证]`

- A：把非标准 `stop` 字段写进 Responses 请求体，同时本地用 `stopFilter` 截断（`console_stream.go:116`）；若上游自行截断，客户端流里看不到被匹配 token，`filter.matched` 为空 → `stop_sequence` 不下发、`stop_reason` 回落 `end_turn`。
- B：全程本地截断，因此总能回填 `stop_sequence`。
- 影响：Anthropic `stop_sequences` 场景的 `stop_reason` 错误；给上游多传契约外字段还有被拒风险。
- 修复：只在本地截断，不下发上游。

### A1-10 [P2] 响应 id/created 被重新生成丢弃

- A：原生 Chat 路径不保留上游 `resp_*` id，生成随机 `chatcmpl_*`（`console_stream.go:108`）；Messages 非流式复用该 id，Anthropic `message.id` 变成 `chatcmpl_xxx`；`created` 用 `time.Now()`；tool_use id 无 `toolu_` 归一化。
- B：保持上游响应谱系与 Anthropic id 形态。
- 影响：id 形态不符合 Anthropic 协议；`previous_response_id`/日志追踪关联断裂。
- 修复：保留上游 id 并做 `msg_`/`toolu_` 形态归一。

### A1-11 [P2] Build 平面缺 `store=false` 默认

- A：Chat/Messages 转出的 Build 请求体不带 `store`（`types.go:21-44`、`handler_messages.go` 转换处），也不在 `normalizeBuildResponsesPayload` 中补默认（该函数不碰 `store`）。
- B：`cli/normalize.go:125` 调用 `applyBuildResponseDefaults`，对缺省/null 的 `store` 强制写 `false`，注释明确为 ZDR 安全默认；`cli/normalize.go` 自基点起零改动，故非漂移。
- 影响：上游可能持久化响应与推理状态，与"无状态代理"宣称不符。
- 修复：Build 平面缺省注入 `store=false`。

### A1-12 [P2] `x-anthropic-billing-header` 未剥离，破坏上游缓存前缀

- A：Claude Code 在 system 里带的每请求变化的 `x-anthropic-billing-header: …` 被原样拼进 `instructions`（位于前缀最前）。
- B：字符串与 block 两种形态都丢弃它，注释说明是为保住 prompt cache 前缀。
- 影响：上游 prompt cache 命中率下降，成本/延迟上升。
- 修复：转换时剥离该标记。

### A1-13 [P2] Anthropic 错误类型恒为 `invalid_request_error`，上游正文直通 `message`

- A：`handler_messages.go:1101-1107` 无论 429/401/529/400 都写 `invalid_request_error`，`writeAnthropicUpstreamError` 把上游 body 整体塞进 `message`。
- B：`inference/handler.go:2442` 按状态派生 `overloaded_error`/`permission_error`/`rate_limit_error`/`not_found_error`，并带 `code`。
- 影响：Anthropic SDK/Claude Code 依 `error.type` 决定退避重试，A 上全部被判为客户端错误不重试；错误信息不可读且含上游原文。
- 修复：按 status 派生 type，白名单化 message。

### A1-14 [P3] assistant 历史文本使用 `output_text` `[待验证]`

- A：对 assistant 历史发送 `output_text` 且不归一化；B 把输入侧所有文本块统一改写为 `input_text`，说明上游 `input` 契约只保证 `input_text`。
- 影响：多轮对话可能被上游拒绝或语义降级（缺上游实际接受度证据）。

### A1-15 [P3] Chat `tool` 消息缺 `tool_call_id` 时用 function name 顶替

- A：`responses_normalize.go` 把 `name` 当 `call_id` 生成一个不存在的 `function_call_output`；B 直接拒绝。
- 影响：上游收到无法配对的工具结果，错误被静默吞掉。

### A1-16 [P3] `max_tokens` 与 `max_completion_tokens` 同时出现时优先级相反

- A 取 `max_tokens`，B 取 `max_completion_tokens`（OpenAI 已弃用前者）。
- 影响：输出上限行为不一致。

### A1-17 [P3] `metadata` / `service_tier` 未透传；`parallel_tool_calls` 仅在有 tools 时下发

- A：`types.go:21-44` 没有 `metadata`/`service_tier` 字段；`parallel_tool_calls` 只在 `len(tools)>0` 时下发。B 无条件透传。
- 影响：无工具但显式 `parallel_tool_calls` 的请求参数被丢弃；依赖 metadata/service_tier 的上游特性不可用。

### A1-18 [P3] Chat 图片 part 未补默认 `detail:"auto"`

- A 省略该键，B 显式发送 `detail:"auto"`。影响取决于上游默认值。

### A1-19 [P3] 工具序列校验偏松

- A 允许没有 `tool_result` 的 `tool_use`，允许 `tool_reference` 指向未声明工具/空名；B 均报错。
- 影响：损坏的 Anthropic 历史被转发到上游，报错点后移。

### A1-20 [P2] `messages[]` 内 `role="system"` / `"developer"` 被 400 拒绝

- A：Chat 校验的 `allowedMessageRoles` 不含 system/developer（Anthropic 允许在 messages 里放 system 角色）。B 接受并归并。
- 影响：合法的 Anthropic 请求被拒。
- 修复：转换层把消息级 system/developer 归并进 `instructions`。

---

## 2. Responses API / Grok CLI(Build) 适配（14 条）

证据：`docs/grok2api-audit/03-responses-cli.md`。

### A3-1 [P1] 原生 Build Responses 流未做事件字段补齐，Codex / Grok TUI 解析失败

- A：`handleNativeCLIResponsesAt` → `copyNativeCLIResponseAndCaptureModel` 只做脱敏后原样写出；全仓无 `sanitizeResponsesEvent`/`ensureOutputTextAnnotations` 等价物（grep 0 命中）。
- B：`cli/responses_compat.go:183-221` 逐事件补齐 `output_text.annotations`（缺失 → Grok CLI `serialization error: missing field annotations`）、`response.id/created_at/object/output/model`、`event.item_id`、`item.id`。
- 影响：原生路径的 Codex/TUI 客户端在工具续跑、失败重试、会话恢复时整轮失败；A 的 chat 桥与原生路径对同一上游事件产出两种 wire shape。
- 修复：移植 `responses_compat.go` 的事件补齐层，或统一走一个写出器。

### A3-2 [P1] `apply_patch_call` 还原缺 `operation` 且残留 `arguments`

- A：`responses_alias.go:160-163` 只改 `type` 并删 `name`，不解码 `arguments`、不删除它。
- B：`cli/responses_response.go:388-397` 解码成 `operation` 对象并删 `name`/`namespace`/`arguments`（`responses_codex_tools.go:147` 校验 operation）。
- 影响：Codex 拿到缺必填 `operation` 且带非法 `arguments` 的补丁调用，无法执行或被 schema 拒绝。
- 修复：实现 `decodeApplyPatchArguments`。

### A3-3 [P1] `custom`（freeform grammar）工具未模拟

- A：`responses_normalize.go:558` 把 `type:"custom"` 原样转发；`collectBuildToolAliases`（`:78-107`）与 `rewriteBuildToolAliasValue`（`responses_alias.go:144-163`）都没有 `custom` 分支。
- B：`responses_custom.go:30` 把 freeform 工具降级为 `input: string` 的普通 function，响应侧还原成 `custom_tool_call` + `input`（`responses_response.go:372-381`），非 text format 仅告警。
- 影响：Codex 的 freeform/grammar 工具整轮被上游拒绝，或客户端收到无法识别的 `function_call`。
- 修复：补齐 custom 工具的模拟与还原（含流式 `custom_tool_call_input.delta/done`）。

### A3-4 [P1] 原生 Build Responses 的 `input` 历史项未归一化

- A：原生路径除 function_call 改名外完全不检查 `input`，未知/扩展项直接进上游。
- B：`cli/responses_history.go` 逐项处理 20 余种类型（`local_shell_call(_output)`、`agent_message`、`mcp_tool_call_output`、`tool_search_call/output`、`custom_tool_call(_output)`、`apply_patch_call(_output)`、`shell_call_output`、`compaction_trigger`、`additional_tools`…），未知类型替换为 boundary 文本并告警 `unsupported_input_history_omitted`；`responses_input.go:29` 注释指出原样转发会"伪造可再次执行的 hosted shell call"。
- 影响：Codex/pi 回放历史时被上游 400，或把不透明密文（`agent_message`）转发出去。
- 修复：移植 `normalizeInputItems`。

### A3-5 [P1] 工具 schema 只做浅层 nullable 折叠，且 `inputSchema` 未转 `parameters`

- A：`responses_normalize.go:567-604` 只删 `type` 数组里的 `"null"`，并在恰好剩 1 个 object 分支时上提；不解析 `$ref`/`$defs`、不做多分支叶子收集、不校验分支互斥；只读 `parameters`，`inputSchema`/`input_schema` 既不转换也不删除。
- B：`responses_tool_declarations.go:320-601` 的 `rootObjectLeafCollector` + `rootObjectLeavesPairwiseDisjoint` 完整展平根联合、合并 sibling 约束、按需保留 `$defs`，并把 `inputSchema` 转成 `parameters`。该能力是基点之后新增（`22ac653a`/`72a3a347`/`5d19ccff`/`e5285ebe`）`[漂移]`。
- 影响：Codex/MCP 常见的"根 anyOf/oneOf + $ref" schema 被上游 grammar 拒绝或参数 schema 静默丢失。
- 修复：移植根 schema 展平算法（或至少过滤非 object 分支并解析根 `$ref`）。

### A3-6 [P1] 会话种子只认 4 个头，Claude Code / Codex 会话信号全丢

- A：`session_state.go:58` 只认 4 个自定义头 + body `prompt_cache_key`，且 `session.Replay` 与 `explicitSession` 绑定。
- B：`transport/http/inference/prompt_cache.go:31-76` 识别 `X-Claude-Code-Session-Id`、Codex 系列头、`X-Session-Id`/`X-Conversation-Id`/`X-Grok-Conv-Id`，以及 body 的 `conversation_id`/`session_id`/`metadata.user_id`/`client_metadata["x-codex-window-id"]` 等。
- 影响：这两类客户端在 A 上永远 `Replay=false`：不注入也不捕获 reasoning 回放，账号亲和跨轮漂移，`cached_tokens` 归零。
- 修复：移植 `extractPromptCacheSeed` 的完整识别顺序。

### A3-7 [P1] 缺少网关侧 compaction（`compaction_trigger` / `g2a_compact_v1`）

- A：只有"把 `/responses/compact` 转发上游"一条路径；`grep "compaction_trigger\|g2a_compact"` 0 命中；`compaction` 类型 input 项仅在回放剥离时保留（`session_state.go:376-379`）。
- B：`application/gateway/responses_compaction.go` 做触发分类、网关自建摘要采样、清洗后加密成 `g2a_compact_v1.<cipher>` blob，并在后续请求里展开。
- 影响：Codex remote-v2 压缩在 A 上退化为"客户端自己压缩"，跨账号回放行为与 B 分叉。该能力在基点已存在，属移植缺口而非漂移。
- 修复：按需移植压缩管线，或至少识别 `compaction_trigger` 并给出明确错误。

### A3-8 [P1] `store` 语义双向不同

- A：上游侧不注入 `store=false`；本地侧只有 `store==true` 才写库（chat 桥还额外要求 `status=="completed"`，`responses_channel_bridge.go:366-373`）。
- B：`cli/normalize.go:184` 注入 `store=false`；`application/account/service.go:1131` 对任何 2xx Responses 都写 ownership，与 `store` 取值无关。
- 影响：依赖 OpenAI "store 缺省为 true" 的客户端在 A 的 chat 桥上无法用 `previous_response_id` / `GET /responses/{id}`（`response_not_found`）；上游留存策略相反。
- 修复：上游缺省注入 `store=false`；本地持久化与 `store` 解耦（按需配置）。

### A3-9 [P2] 原生路径未默认补 `include: reasoning.encrypted_content`

- A：`ensureReasoningEncryptedInclude`（`session_state.go:345-353`）只在已有缓存可回放时才调用；补全逻辑（`responses_normalize.go:173-177`）只覆盖 chat 桥。首轮不带 include → 上游不回 `encrypted_content` → `normalizeReplayItems` 缺 anchor → `reasoning_replay_capture.go:37-44` 反而清除会话。
- B：对所有 Build 请求默认补该 include。
- 影响：原生 Responses 多轮（Codex 每轮重发全量历史且不带 include）永远建立不了 reasoning 回放链。
- 修复：原生 Build 路径缺省补 include。

### A3-10 [P2] 上游会话头取值不同：64 字符 sha256 hex vs UUID

- A：`cli.go:238` + `session_state.go:92` 把 sha256 hex 直接写进 body `prompt_cache_key` 与 `x-grok-session-id`/`x-grok-conv-id`，且无条件设置。
- B：`cli/adapter.go:1077-1097` 把会话键规范成 UUID（已是 UUID 透传，否则 UUIDv5），且仅在存在稳定会话时设置这两个头。
- 影响：头格式与官方 CLI 不一致，可能影响会话亲和与 `cached_tokens`。
- 修复：对齐 UUID 规范化与条件设置。

### A3-11 [P2] 缺少 CLI 身份/追踪/模型覆盖头，却带上 web 平面的 `x-xai-request-id`

- A：`cli.go:88/234` 无 `x-authenticateresponse`、`x-grok-agent-id`、`x-grok-req-id`、`traceparent`，`x-grok-model-override` 只在 `/videos/` 设置；反而带 web 平面专用的 `x-xai-request-id`（B 只在 `infra/provider/web/headers.go:22` 使用）。
- B：`cli/adapter.go:1032-1066` 全量设置四类头并给所有 traced 请求加 `x-grok-model-override`。
- 影响：上游看到非官方 CLI 身份组合，风控/限流表现可能不同。

### A3-12 [P2] 非流式 Build Responses 上限 8 MiB（B 为 128 MiB）

- A：`handler_responses_store.go:308` 的 `newBoundedResponseCapture(8<<20)` 超限即回 502 `upstream_error`；128 MiB 常量只用于别名重写路径。
- B：`cli/responses_response.go:13` + `adapter.go:451` 为 128 MiB。
- 影响：长上下文/大工具输出的非流式请求被误判为上游故障，客户端会重发放大上游压力。

### A3-13 [P2] 函数 arguments 的整型字面量未修复（`60000.0`）

- A：Build/Responses 路径不做任何 arguments 规范化（`grep "UseNumber\|normalizeIntegralNumber"` 0 命中），只有 web 文本 `<tool_call>` 修复（`tool_call.go:173`）。
- B：`cli/responses_arguments.go:19-188` 按 schema 递归把语义为整数的 JSON 数字改写为整型字面量（含指数与精度边界），流式/非流式都做（`responses_response.go:191-205,358-365`），因为 Codex 严格解码器拒绝为整型字段传浮点。
- 影响：Codex 工具调用参数解析失败。
- 修复：移植 `normalizeFunctionArguments`。

### A3-14 [P2] `GET /responses/{id}/input_items` 对 Build 记录只返回本轮输入

- A：`handler_responses_store.go:135/178` 只存本轮 `payload["input"]`；用 `previous_response_id` 时历史缺失，且子资源分派在 provider 判定之前，永不回源。
- B：`inference/handler.go:109` 的路由表按 provider 回源取完整历史。
- 影响：Codex 读取历史时拿到不完整/空列表。

---

## 3. 模型目录 / 路由（17 条）

证据：`docs/grok2api-audit/04-models.md`。补充：公开 `/v1/models` 由 `internal/handler/models.go:99` 提供（不在 `internal/api/api.go`，后者是管理端 `/api/models`）。

### A4-1 [P1] 对外模型 ID 携带 Provider 前缀

- A：`internal/grok/models.go:55-87` 把 `console/`、`build/` 写进公开 `ModelSpec.ID` 并由 store 行原样输出，`/v1/models` 出现 `console/grok-4.3`、`console/grok-4.5` 等；A 自己的测试（`handler_model_validation_test.go:172`）刻意让裸 `grok-4.3` 不可解析。
- B：`domain/model/model.go:143` 用 `NormalizePublicID` 构造内部 ID、`ExternalPublicID` 还原对外名；对外恒为 `grok-4.3`。
- 影响：把 B 的模型名当契约的客户端在 A 上 400/404；反之 A 的客户端名在 B 上不存在。冲突的 Console 系列全部错位。

### A4-2 [P1] 缺少 reasoning-effort 后缀别名

- A：不在列表产出 `<model>-<effort>` 别名，也没有后缀解析函数（`ParseReasoningModelAlias` grep 0 命中）；`internal/handler/handler_helpers.go:222` 的 effort 变体逻辑要求库中真存在后缀行，而 A 不为 grok 建这种行；API Key 结构体无 `AllowModelAliases`。
- B：`application/gateway/service.go:568` 用 `ParseReasoningModelAlias` 反解，并按模型真实支持的档位合成（`grok-4.5-none`/`-xhigh` 被测试拒绝）。
- 影响：`grok-4.5-high`、`grok-4.6-xhigh` 这类写法在 A 上全部"模型不存在"。

### A4-3 [P1] 缺少注册式 Provider 兼容别名

- A：无别名层；前缀剥离是白名单式（`normalizeModelID` 只 `TrimPrefix(id,"web/")`），`Build/grok-4.5`、`Console/grok-4.3` 不剥。
- B：`console/catalog.go:56-73` 有 14 条注册别名，含 `-console` 后缀名、`grok-imagine-image-quality-2.0`、把 effort 固化到路由的 `grok-4.3-low` 等。
- 影响：依赖这些名字的客户端全部失败，迁移需重写模型名。

### A4-4 [P1] 模型不存在/无权的状态码与响应体

- A：`http.Error` → 400 + `text/plain`；`/grok/v1/messages` 同样 400（`handler_messages.go:75`）。
- B：`writeOpenAIError` → 404 JSON `model_not_found`。
- 影响：SDK 的 model-not-found 分支、探测降级逻辑、告警分桶全部错位。

### A4-5 [P1] Console 平面缺每模型默认 `max_output_tokens`

- A：`ModelSpec` 无 `MaxOutputTokens`，只在客户端显式传 `max_tokens` 时才发。
- B：`infra/provider/console/catalog.go:24-31` 为每模型声明默认输出上限并在缺省时注入（如 1_000_000 / 256_000）。
- 影响：Console 路由在客户端不给上限时被上游默认值提前截断。

### A4-6 [P1] Console 平面缺每模型 reasoning 剥离/默认档位

- A：`normalizeConsoleReasoningEffort` 收到 `model` 参数却不用；没有 `SupportsReasoning`/`SupportsReasoningEffort`/`DefaultReasoningEffort` 概念。
- B：`console/normalize.go:42,192` 按这三个字段分别"整段删 reasoning"、"只删 effort"、"缺省注入 medium"。
- 影响：A 把 `reasoning.effort` 发给明确拒绝它的模型（400 或被忽略），对 non-reasoning 模型发 reasoning 对象，缺省档位也不同。

### A4-7 [P1] Codex 目录缺少 5 个协议字段

- A：无 `default_service_tier`、`availability_nux`、`upgrade`、`model_messages`、`auto_compact_token_limit`；B 的字段无 `omitempty`，即使 nil 也序列化成 `null`（`inference/codex_models.go:28-65`）。
- 影响：按字段存在性解析的 Codex/CLI 客户端缺键，无法获知服务档位与自动压缩阈值。

### A4-8 [P1] Codex 工具能力判定未限定 Build Provider

- A：只要行 capability 含 `responses` 就打开 agent 工具集，而 A 给所有非媒体 Grok 行默认写入 `chat,messages,responses`，Web 模型也被标成支持。
- B：`codex_models.go:135-137,159` 只在 Build+Responses 路由上开 `apply_patch_tool_type:"freeform"`、`supports_parallel_tool_calls:true`。
- 影响：Codex 对 Web 模型下发 `apply_patch`/并行工具 → 静默失败或 400。

### A4-9 [P1] 同一对外名称无跨 Provider 回退

- A：一名一 Provider（两个公开 ID `grok-4.5` / `console/grok-4.5`），点名 `grok-4.5` 时 Build 池空即整请求失败。
- B：`gateway/service.go:549-588,697-708` 收集全部候选并按 Build→Web→Console 排序回退。
- 影响：单 Provider 容量/封禁/限流时 A 直接 5xx，B 自动降级。

### A4-10 [P2] Codex `supports_reasoning_summary*` 结论相反

- A：`console/grok-4.20-0309-reasoning`（固定推理）被判不支持 summary；未知模型兜底 `["none"]`（长度 1）反而被判支持。
- B：`domain/model/reasoning.go:74` 的 `IsFixedReasoningForProvider` 命中 → true；纯 `none` → false。
- 影响：能力宣告与实际相反（两类模型都判反）。

### A4-11 [P2] Build 账号目录补全被移除

- A：`internal/grok/provider.go:225-237` 注释承认删掉了 Composer、4.6 在位时的 4.5、super 的 video 1.5 三项补全。
- B：`cli/adapter.go:758-800` 的 `NormalizeAccountModelCapabilities` 仍补，并由同步写进目录。
- 影响：B 目录里的 `grok-composer-2.5-fast` 在 A 的列表不可发现（仍可路由）。

### A4-12 [P2] `grok-imagine-image-pro` / `-quality` 公开 ID 与弃用表冲突

- A：`models.go` 仍公开 `grok-imagine-image-pro`，但 `handler.go:302-304` 的 `IsDeprecatedModelID` 在 `ensureModelEnabled` 第一行就判死 → 任何请求 400 model not found；`handler_images.go:206-210` 仍把它算作 pro 模型。
- B：删除该公开 ID，改为 `grok-imagine-image-2.0` 的 `ImaginePro` 标志并用测试锁死旧名不得回归。
- 影响：目录/文档/前端出现一个"列出但必失败"的模型。

### A4-13 [P2] 公开 `/v1/models` 字段集合与 `created` 语义不同

- A：多输出 `capabilities`/`provider`/`upstream_model`（泄露内部 Provider 与上游模型名），`created` 固定 `1677610602`。
- B：固定 4 键，`created` 为路由行真实创建时间，`provider`/`capability` 用 `json:"-"` 隐藏。
- 影响：字段冗余 + 按创建时间排序/增量的客户端逻辑失效。

### A4-14 [P2] `grok-imagine-image-lite` 排除 Basic 账号池

- A：`PoolCandidates` 对同一公开 ID 返回 `lite/super/heavy`，排除 `basic`，仅 Basic 账号的部署上完全不可用（测试固化）。
- B：`MinimumTier=basic`，Basic（免费）可服务。
- 影响：免费账号部署在该模型上 A 报 503、B 正常。

### A4-15 [P3] 管理端模型接口形状不同

- B 为分页信封 + 分组 + `endpointCapabilities` + SSE 同步；A 的 `/api/models` 返回裸 `[]store.Model`，同步走 `/api/models/refresh`。
- 影响：管理端前端不能直接复用；"同名多能力路由"在界面上不可见。

### A4-16 [P3] 前端兜底模型清单与档位选项越界

- A 前端内置一份全部已弃用的兜底清单（接口失败时渲染成下拉项），并把 `none`/`xhigh` 暴露给所有模型（`grok-4.5` 不支持，`responses_normalize.go:700-711` 会静默把 `xhigh` 降为 `high`）。
- 影响：界面提供必然被拒的模型名与"看起来生效实则被改写"的档位。

### A4-17 [P3] Codex 未知模型描述文案不一致（仅文案）

---

## 4. 错误契约 / 鉴权 / 校验（16 条）

证据：`docs/grok2api-audit/08-errors-auth.md`。

### A8-1 [P0/P1] 上游错误正文与内部错误串透传客户端

- A：`console.go:370` `http.Error(w, err.Error(), upstreamHTTPResponseStatus(err))`，而 `err` 由 `upstream_error.go:37` 渲染为 `grok cli upstream status=400 node=<出口节点> body=<上游正文前 4096B>`；`handler_messages.go:103,807` 把上游响应体整体作为 Anthropic `error.message`；`console.go:519` 输出 `console response parse error: <内部 JSON 错误>`。
- B：`application/gateway/failure.go:102` 只允许固定文案 `Code:"upstream_error", PublicMessage:"上游服务返回错误"`，正文永不外泄。
- 影响：任意持 Key 调用方可读到出口节点标识与上游账号态信息（配额/套餐/团队提示）；错误体是 `text/plain`，下游无法编程处理。若上游正文含账号标识则为 P0。
- 修复：所有 `http.Error(w, err.Error(), …)` 改走 `apperrors.New(category, PublicMessage, status).WriteResponse`；`grokUpstreamError.Error()` 去掉 `body=`/`node=`（只进审计诊断）。

### A8-2 [P1] 校验失败返回 `text/plain`，没有 OpenAI 错误对象

- A：`http_helpers.go:51` 的 `decodeJSONBody`、`handler_chat.go:198/221/225/229/238/354` 等全部 `http.Error`。
- B：`inference/handler.go:2285` 的 `writeOpenAIError` 统一 `{error:{message,type,code,param}}`。
- 影响：OpenAI SDK `resp.json()["error"]` 直接抛解析异常，真实错误信息丢失（常见路径）。
- 修复：Grok 通道统一走项目已有的 `apperrors` JSON 出口。

### A8-3 [P1] 上游 401/402/403/404 → 客户端状态映射与 B 相反

- A：`internal/errors/public.go:24` 把 `auth/auth_blocked → 401`、`quota_exhausted/rate_limit → 429`；`errors/classify.go:197-200` 把上游 403/404 都归为 `auth_blocked`（404→401）。
- B：`inference/handler.go:2402` 的 `isUpstreamCredentialStatus`（401/402/403）→ `503 upstream_unavailable` + 脱敏文案；404 原样保留。
- 影响：下游把账号池问题误判为自身 Key 失效（401）或限流无脑重试（429），放大上游压力。
- 修复：区分"客户端 Key 问题"与"上游账号池问题"，后者统一 502/503。

### A8-4 [P1] "没有可用账号"的状态码与类型不一致

- A：`internal/handler/stream_handler.go:2852/2900/2930` 复用上游错误分类，`lastErr` 形如 `… status=401 …` 时最终给 401；否则 502 且 `error.type="unknown"`。
- B：`inference/handler.go:2340` → 503 `upstream_unavailable` + "当前没有可用的上游账号"。
- 修复：为选号失败单列稳定类别（503）。

### A8-5 [P1] 全局限流闸门阻塞 60s 后纯文本 503 且无 `Retry-After`

- A：`middleware/concurrency.go:66/86/91/94`：`waitTimeout=60s` 内排队，超时后 `http.Error(..., 503)`。
- B：`middleware/concurrency.go:39` 立即 503 + `Retry-After: 1` + `server_overloaded`。
- 影响：过载时连接与 goroutine 被排队请求长期占用（雪崩风险），下游无法获知退避时间。

### A8-6 [P1] Anthropic 信封 `error.type` 恒为 `invalid_request_error`

- A：`handler_messages.go:1101-1107`（无 `code`）。
- B：`inference/handler.go:2442` 按状态派生 `overloaded_error`/`permission_error`/`rate_limit_error`/`not_found_error` 并带 `code`。
- 影响：Anthropic 客户端把额度/过载错误当参数错误，直接放弃而不重试/换 Key。

### A8-7 [P1] 原生 Grok 推理端点无请求体大小上限

- A：`decodeJSONBody` 直接 `json.NewDecoder(r.Body)`，无 `http.MaxBytesReader`；50 MiB 上限只存在于另一条通用通道（`internal/handler/handler.go:129/370`），`cmd/server` 中间件链无体积限制。
- B：全局 32 MiB（`middleware/request.go:79`、`infra/config/config.go:877`）+ 每处理器 `MaxBytesReader` → 413 `request_too_large`。
- 影响：单个持 Key 客户端可提交任意大小 JSON 触发 OOM/长 GC（DoS 面）。
- 修复：`decodeJSONBody`/路由包装层统一 `http.MaxBytesReader` 并对 `*http.MaxBytesError` 返回 413 JSON。

### A8-8 [P2] 请求执行超时 A 600s vs B 2h

- A：`internal/config/config.go:332/337`（默认 600s）+ `middleware/concurrency.go:121` 的执行超时。
- B：`infra/config/config.go:880`（2h）+ `middleware/request.go:66`。
- 影响：长推理/长工具链在 A 上 10 分钟被静默截断为短答案。

### A8-9 [P2] 429 不回写 `Retry-After`

- A：错误出口无 `Retry-After`；上游值只用于账号调度与诊断（`grok/rate_limiter.go:170`、`attempt_diagnostics.go:130`），仅客户端 Key 层写死退避（`middleware/session.go:56/72/158`）。
- B：`inference/handler.go:2335/2435` 回写上游/选号退避。
- 修复：从 `ratelimit_metadata` 解析剩余冷却并在 429/503 回写。

### A8-10 [P2] 不校验 `Content-Type`（无 415）

- A：`decodeJSONBody` 完全不读 `Content-Type`；415 只出现在语音/媒体端点。
- B：`isJSONRequest` → 415 `invalid_request`。
- 影响：`text/plain` 也能执行推理，增大 CSRF 类误用面，且契约不一致。

### A8-11 [P2] 未知字段容忍度：B 对视频请求显式拒绝

- A：全项目无 `DisallowUnknownFields`。
- B：`inference/handler.go:729,1187` 仅视频创建路径 `DisallowUnknownFields` → 400。
- 影响：参数拼错（如 `duratoin`）在 A 上静默退化为默认行为。

### A8-12 [P2] `/v1/models` 鉴权可被配置整体关闭

- A：`cmd/server/routes.go:92` 用 `inferenceAuth`，其 enabled 回调为 `InferenceAuthEnabled()`（`internal/config/config.go:508`，默认 true 但可配 false），关闭后全部推理端点匿名开放。
- B：`transport/http/server.go:190` 的 `/v1` 组恒挂 `middleware.ClientAuth`，无开关。
- 影响：一个配置项即可把统一入口变成开放代理；审计中这是与参考实现最直接的鉴权边界差异。

### A8-13 [P2] 请求 ID 头名不一致，错误体无 `request_id`

- A：只回 `X-Trace-ID`/`X-Orchids-Request-ID`（`middleware/trace.go:49/60`），`internal/errors/errors.go:18` 的错误 JSON 不含 request_id。
- B：`middleware/request.go:29-30` 设 `X-Request-ID` 并贯穿处理器。
- 影响：按 OpenAI 约定读 `X-Request-ID` 的下游拿不到值，且无法把失败对应到 A 的审计日志。

### A8-14 [P2] 客户端 Key 限制语义与错误码

- A：`internal/store/store.go:883` 对未配置 RPM 的 Key 强加 **60 RPM**；过期码 `api_key_expired`（`middleware/session.go:154`）；无计费上限概念、无"内部 Key 不得外部鉴权"判定（`ApiKey` 结构体无 `InternalKind`，属能力缺口）。
- B：`application/clientkey/service.go:435-448` 无默认 RPM、过期/禁用同归 401 `invalid_api_key`、另有 429 `billing_limit_exceeded`，并拒绝 `InternalKind`。
- 影响：从 B 迁到 A 的 Key 会意外撞 60 RPM；缺少用量上限与内部身份拒绝分支。

### A8-15 [P3] 模型白名单 403 的 `error.type` 不同

- A：`http_helpers.go:64-70` → 403 `permission_error` + `code:model_not_allowed`（无 `param`）；B → 403 `model_not_allowed` 且 `type=invalid_request_error`、`param:null`。
- 影响：仅按 `error.type` 分派的客户端走不同分支。

### A8-16 [P2] SSE 刷头后错误帧形状不同

- A：`http_helpers.go:139` 的 Chat SSE 错误帧缺顶层 `"type":"error"`；`handler_responses_store.go:414` 的 `response.failed` 缺 `created_at`/`completed_at`/`output`，且随后补 `[DONE]`。
- B：`inference/handler.go:1522/1533/1548` 带顶层 `type`、完整信封、失败帧后不补 `[DONE]`。
- 影响：流中途失败时下游可能把错误帧当普通 chunk，或等待永不出现的结束事件。

## 5. 媒体 / 音频（45 条）

证据：`docs/grok2api-audit/05-media-audio.md`（其中 98 处 `file:line` 引用已机械校验）。

### A5-1 [P0] `/images/edits` 线格式完全不同：A 仅 multipart，B 仅 JSON

- A：`handler_image_edits.go:183-261` 只解析 `multipart/form-data`，字段 `image` / `image[]`，其余取 `r.FormValue`。
- B：`inference/handler.go:154-168,609-630` 先 `isJSONRequest`（非 `application/json` 一律 415），图像以 JSON `image`/`images` 的 `{url,file_id}` 传入并显式拒绝 `file_id`；全仓无 multipart 图片编辑路径。
- 影响：同一客户端按 B 契约发 JSON 在 A 得 400，按 A 契约发 multipart 在 B 得 415；两实现不可互为替代。
- 修复：两者都接受，或至少让 A 支持 JSON 形态与 B 对齐。

### A5-22 [P0] chat `image_url` 服务端直连抓取，无 SSRF 防护

- A：`internal/grok/util.go:446-479` 的 `fetchRemoteAsDataURI` 用默认 transport 直接 GET 客户端给的任意 URL：`isRemoteURL`（`util.go:440-443`）接受 `http` 与 `https`，无 IP/端口/userinfo 校验、无重定向限制、无内网段屏蔽。
- B：`infra/provider/web/image.go` 的 `newRemoteImageTarget` 强制 `https:443`、禁 userinfo、对 DNS 解析出的每个地址做公网校验并把连接固定到该 IP。
- 影响：任意持 Key 调用方在消息 `image_url` 填 `http://169.254.169.254/latest/meta-data/…` 或 `http://127.0.0.1:PORT/…` 即可让代理发起内网/云元数据请求（SSRF）。
- 修复：强制 https、解析后校验公网 IP、禁 userinfo、限制重定向与端口。

### A5-6 [P1] 客户端可控 `nsfw` 直通上游 `enable_nsfw`

- A：公开 `/images/generations` 接受请求体 `nsfw:true` 并传给 Imagine WebSocket；`PublicImagineNSFW()`（`config.go:477`）只作用于 admin/`public_api` 路径，不参与该端点判定。
- B：请求结构体无 `nsfw` 字段，`enable_nsfw` 唯一来源是服务端 `cfg.AllowNSFW`。
- 影响：任何持 Key 调用方可单方面开启上游 NSFW 生成，绕过运营方内容开关。

### A5-9 [P1] 媒体/音频端点只在 `/grok/v1` 注册；标准视频响应返回未注册的自链接

- A：`cmd/server/routes.go:171-205` 全部走 `grokPrefixes := []string{"/grok/v1"}`；`handler_videos.go:88` 的 `toStandardMap` 返回 `"url":"/v1/videos/<id>/content"`，而 `/v1/videos/` 从未注册（会落到 mux `/` 兜底 404）。
- B：`inference/handler.go:92-107` 全部媒体/音频端点挂在 `/v1` 下。
- 影响：把 base_url 指向 `/v1` 的 OpenAI/官方 SDK 客户端所有媒体与音频调用 404；即使只做视频轮询也因拿到不可达 `url` 而失败。
- 修复：媒体路由同时注册到 `/v1`，并让 `url` 使用可达路径与配置域名。

### A5-10 [P1] 媒体绝对地址取自可伪造的请求头

> **审计更正（修复时核实）**：本项目已有 `internal/middleware/trusted_proxy.go`：`trusted_proxies` 为空（默认）时**清除全部 `X-Forwarded-*`**，配置后只接受来自受信网络的取值并取最后一段、且校验协议。因此"任意客户端注入 `X-Forwarded-Host`"在默认配置下不可达，本条实为 **P2**（仅在显式配置 `trusted_proxies` 且该代理透传客户端头时可达；真实问题是 host 值未做字符校验）。已修复：`detectPublicBaseURL` 增加严格 host 校验（拒路径/查询/userinfo/空白/控制字符），协议白名单收敛为 http/https。

- A：`detectPublicBaseURL` 优先信 `X-Forwarded-Host`/`X-Forwarded-Proto`，其次 `Host`，无可信代理白名单。
- B：只来自配置/运行设置（`server.go:191`、`settings/service.go:227-231`），全仓不读 `X-Forwarded-Host`。
- 影响：在受信代理透传客户端头且 host 未校验的部署里，响应 `url`/`content_url` 可被指向攻击者域。
- 修复：已加 host 字符校验；如需彻底对齐 B，应改为只使用服务端配置的公开地址。

### A5-11 [P1] Web 免费档视频 6 秒钳制缺失

- A：全仓无 `WebTier`/`FreeVideoDurationCap` 概念，只做 1..15 通用校验。
- B：对 `WebTierBasic` 凭证在发请求前把时长压到 6 秒（可配 1..15），Super/Heavy 不钳制。
- 影响：基础/免费 Web 账号请求 >6 秒 → 上游 429 → 异步任务直接 failed；B 本地钳制避免无意义失败。

### A5-12 [P1] `/videos/generations` 硬绑 Console 账号池

- A：`cmd/server/routes.go:174` 的官方视频端点只从 Console 池取号，不读模型存储路由的 provider；而 `grok-imagine-video` 的默认路由是 web（`store.go:713-720`）。
- B：同一端点按能力收集全部 provider 路由，web 目录把 `grok-imagine-video` 注册为视频路由（`web/catalog.go:31`）。
- 影响：只配 Web 视频账号的部署在 A 上必然 503，drop-in 兼容失败。

### A5-13 [P1] Console 成品视频下载把 SSO Cookie 外发到资产域

- A：把任何 `*.x.ai`（含 `vidgen.x.ai`）视为需鉴权资产域，对视频内容 URL 附加 `Cookie: sso=…; sso-rw=…; cf_clearance=…`。
- B：资产下载只设 Accept/User-Agent，绝不携带 token 或会话身份头（注释明令禁止）。
- 影响：会话凭证被复制到媒体 CDN 域，中间层日志即可收集。
- 修复：资产域一律匿名 GET。

### A5-14 [P1] 视频创建阶段失败即判死任务

- A：创建与轮询错误统一走 `handleConsoleVideoJobError → failVideoJob`，首个错误即 `failed`。
- B：对 401/403/402/429 等创建期错误判定可重试并换账号，只有轮询/后处理失败才终止。
- 影响：瞬时 429/403 就把异步任务判死，成功率与配额利用率低于 B。

### A5-15 [P1] `/videos` 字段名另一套，`duration`/`aspect_ratio`/`resolution` 被静默丢弃

- A：JSON 分支无 `DisallowUnknownFields` 且没有这三个字段（`types.go:739-741` 默认 6 秒 / 720x1280→9:16），只有 form/multipart 分支映射 `video_length`/`aspect_ratio`。
- B：单一契约 `duration`/`aspect_ratio`/`resolution`/`image`/`reference_images`/`reference_audios`/`video`，未知字段 400。
- 影响：按 B schema 提交 `{"duration":10,"aspect_ratio":"16:9"}` 在 A 上得到 200 但成品是 6 秒 9:16，参数无提示被吞。

### A5-23 [P1] 对话附件数量与总字节无上限

- A：附件个数不限，只有每个 URL 各自 60 MiB 上限（`util.go:471`），无 Content-Length 预检与合计预算。
- B：单请求最多 8 个附件、合计 64 MiB，先看 Content-Length 再累计。
- 影响：一次请求即可串行下载 N×60 MiB 并 base64 放大约 1.33 倍（内存/带宽放大 DoS）。

### A5-24 [P1] 视频上传票据先消费后校验，失败即烧毁

- A：先 `GetDel` 删除票据再校验 Content-Type 与读取 body，415/400/500 都会永久废掉该上传地址。
- B：类型预检在消费之前，消费后失败会归还票据。
- 影响：任何失败/超限上传都让上传回执地址永久 404，任务卡死。

### A5-25 [P1] 视频上传上限 512 MiB/400 对 B 的 256 MiB/413

- A 用 `LimitReader` 读到 513 MiB 才判定并回 400；B 中间件套 256 MiB `MaxBytesReader` 回 413。
- 影响：状态码与允许体积都不一致，且超限时 A 把整个 body 读完。

### A5-26 [P1] 媒体读取端点路径/方法/鉴权不一致

- A：`/grok/v1/files/{kind}/{name}`，仅 GET（HEAD 被 `requireMethod` 拒为 405），无 ETag/304，整条路由需客户端 API key。
- B：`/v1/media/{images,videos}/:id`，公开、以不可猜测 ID 寻址、GET+HEAD、带 ETag/If-None-Match→304 与 `X-Content-Type-Options: nosniff`。
- 影响：按 B 契约实现的客户端（HEAD 探测、按 id 取图、ETag 缓存、匿名展示）在 A 上得到 404/405/401。

### A5-27 [P1] 管理端缓存列表 URL 指向从未注册的 `/v1/files/...`

- A：`admin_cache.go:373-377` 的 `ViewURL`/`PreviewURL` 用 `/v1/files/...`，而同文件 382 行用 `/grok/v1/files/...`，内部不自洽。
- 影响：管理端每条预览/查看链接恒为坏链（鉴权开启时先 401）。

### A5-28 [P1] 媒体输入无容量配额，且 TTL 过期后磁盘文件永不回收

- A：记录写 Redis 并设 24h TTL，但除显式 DELETE 与保存失败回滚外无删除路径；无写入前总量检查，无按过期时间清扫。
- B：写入前按 `cleanupThresholdBytes`/`MaxTotalBytes` 判容量（超限 507），并有后台 `Cleanup` 同时删对象与元数据。
- 影响：可反复上传 20 MiB 输入写满磁盘；24h 后记录过期而文件永久残留。

### A5-29 [P1] 媒体输入上传端点/鉴权/JSON 契约完全不同

- A：入口在推理前缀（客户端 API key + 并发限流），返回扁平 snake_case（`file_id`/`mime_type`/`bytes`），另有 `/media/inputs/{id}` GET/DELETE。
- B：入口在管理端（管理员会话），返回 `{"data":{...}}` 包裹的 camelCase（`fileId`/`mimeType`/`sizeBytes`/`expiresAt`），无公开对应端点；错误体为 `{error:{code,message,requestId}}`。
- 影响：按 B 契约编写的工具在 A 上 404 或字段解析失败，错误码字符串不可互换。

### A5-32 [P1] TTS/STT 上游响应头不校验直接透传

- A：把上游 Content-Type 与 Content-Disposition（含上游文件名）原样写给客户端并直接拷贝流；无 `nosniff`/CSP；`Retry-After` 丢失。
- B：类型限制在 `application/json`/`text/plain`/`application/ogg`/`audio/*` 白名单，固定加 `nosniff`/CSP/Cache-Control，只回传 Retry-After 与 X-Request-Id。
- 影响：上游返回 `text/html` 或 `application/octet-stream` 时存在内容嗅探与文件名注入面。

### A5-33 [P1] 上游 401/402/403 未屏蔽，原状态码直达客户端

- A 把上游状态码当客户端状态码；B 统一改写成 503 `upstream_unavailable` + 固定文案。
- 影响：客户端把"上游账号失效"误判为自身鉴权失败。

### A5-34 [P1] 错误体 `type` 恒为 `invalid_request_error` 且缺 `param`

- A：所有 voice 错误写死该 type 且无 `param` 键；B 按状态映射 type 并带 `param:null`。
- 影响：按 `type` 决定重试/换 Key 的 OpenAI 客户端全部误判；错误对象反序列化不完整。

### A5-2 [P2] `/images/generations` 缺省 model

- A 的 `req.Normalize()`（`handler_images.go:91`）把空 model 替换成 `grok-imagine-image` 继续生成；B 视为校验失败 400。影响：漏传 model 在 A 上成功并消耗图片额度。

### A5-3 [P2] 图像成功响应字段集不同

- B 每个 data 项固定含 `mime_type`，`revised_prompt` 为空串，整体只有 `{"created","data"}`（`web/image.go:1471`）；A 无 `mime_type`、`revised_prompt` 为 `null`，并额外注入自行估算的 `usage`。
- 影响：严格反序列化（`revised_prompt string`）的客户端在 A 上 `null` → 解析失败；`usage` 在 B 不存在，跨实现对账不一致。

### A5-4 [P2] 图片编辑 `resolution` 与像素别名 `aspect_ratio` 校验不一致

- A：Web 编辑路径只把 `resolution` 传给 Console 分支，非 Console 分支静默忽略（`resolution=2k` 照常成功）；`normalizeImageAspectRatio` 还接受 `1280x720`/`1024x1024` 像素别名。
- B：`resolution ∈ {1k,2k}` 由传输层校验；`validImageAspectRatio` 只接受纯比例串。
- 影响：能力边界不一致，`resolution=2k` 在 A 被吞后按 1k 出图且无提示。

### A5-5 [P2] 图像 URL 形态

- A 的编辑路径 `strict=false`，缓存失败且 URL 不属于必须缓存的主机时直接回上游 URL；成功时返回相对路径 `/grok/v1/files/image/<sha1>.<ext>`，仅 `publicBase != ""` 时补全。
- B 恒先落本地资产再返回配置域名的绝对地址，从不回传上游地址。
- 影响：客户端可能拿到站内相对路径或需要直连 `assets.grok.com` 的原始 URL。

### A5-7 [P2] 图像模型目录分歧

- 同名 `grok-imagine-image-quality`：B 走 Console 媒体 API，A 走 Web Imagine 且上游名替换为 `-lite`；A 额外暴露 Web 版 `grok-imagine-image-pro`（B 无此公开产品）；A 不接受 B 的兼容别名 `grok-imagine-image-quality-2.0`（`handler_images.go:102` 匹配不到即 400）。
- 影响：同一公开名落到不同上游通道与计费口径。

### A5-8 [P3] 流式图像事件 `size` 恒为 `auto`（B 回真实 `WxH`）

### A5-16 [P2] 视频失败错误码恒为 `internal_error`（B 会映射账号/模型类错误）

- 影响：客户端无法区分"账号暂不可用（可重试）"与"内部错误"。

### A5-17 [P2] 官方视频端点拒绝 `user` 字段

- A 的 `consoleVideoAPIRequest` 无 `user` 且开了 `DisallowUnknownFields` → 带 `"user"` 的请求 400；B 的 `videoGenerationRequest` 含该字段。

### A5-18 [P2] Web 视频不支持 4:3 / 3:4

- A 的 Web `/videos` 一律 400，而 A 自己的 Console 端点支持（`handler_videos_console.go:300`）；B 在 HTTP 层透传。同一实现两个入口能力不一致。

### A5-19 [P2] 1080p 资格判定用公开模型名

- A 比较公开 ID，导致 `build/grok-imagine-video-1.5` 被判 "不支持 1080p"（其上游模型正是 `grok-imagine-video-1.5`）；`/videos` 入口用 `spec.UpstreamModel`（`util_media.go:419-421`），同一模型在两个入口结果不同。

### A5-20 [P2] 带 Provider 前缀的视频模型别名不被接受

- A 只剥离 `web/`，`console/grok-imagine-video` 直接 400；B 先按外部名匹配再回退 Provider 命名空间写法（大小写不敏感）。

### A5-21 [P2] Console 403 挑战不失效/重建 clearance

- A 的 `doConsoleDPoPRequestWithHeaders` 命中 CF 403 只累加计数，不失效 clearance、不重试；B 会失效对应 lease 的 clearance 并由上层重试重建。
- 影响：一旦命中 CF 403，A 持续复用坏出口，后续请求连续失败。

### A5-30 [P2] 任意 `ftyp` 载荷被判为 `video/mp4`

- A 只看偏移 4..8 的 `ftyp` 魔数，不看 brand、不要求声明 `video/*`，HEIC/AVIF 被存成 video 并以 `data:video/mp4` 发往上游；B 要求嗅探或声明为视频，会拒 HEIC/AVIF。

### A5-31 [P2] 视频上传票据 TTL 1 小时（B 为 2 小时，并在接收时显式判过期）

### A5-35 [P2] 上游错误响应体写进客户端可见的 `error.message`（并丢 `Retry-After`）

### A5-36 [P2] `/tts/voices` 原样透传，未归一化列表形状

- B 由 adapter 重建每项 `{voice_id,name,language}`，`language` 缺失时显式 `null`，未知字段丢弃；A 原样转发 Console 字节。严格类型客户端在 A 上解析失败。

### A5-37 [P2] GET `/stt` 非 Upgrade：A 400 对 B 405

### A5-38 [P2] STT 不支持的 Content-Type：A 400 对 B 415

### A5-39 [P2] STT multipart 的 `sample_rate_hertz` 未归一化为 `sample_rate`

- B 显式接受该 OpenAI 旧字段别名；A 不识别，参数被 Console 忽略并按默认采样率转写。

### A5-40 [P2] TTS/STT 无账号级重试/故障转移

- A 只调用一次 `doConsoleVoice`，429/402/5xx 直接返回；B 在 `executeVoice` 里对 402/429 与 ≥500 释放租约后换账号重试。

### A5-41 [P3] 转录"不支持参数"的 code/message 不同（`invalid_request` vs `unsupported_parameter`）

### A5-42 [P3] 无 `[]` 的 `timestamp_granularities`：A 400、B 静默忽略

### A5-43 [P3] voice 请求体上限 64 MiB 对 B 的 32 MiB

### A5-44 [P3] voice 转发不回传上游 `Retry-After`

### A5-45 [P2] 视频内容下载 `os.ReadFile` 整段入内存

- A 把成品视频整体读入内存再写响应；B 用 `ReadSeeker` + `ServeContent`（Range 支持）+ ETag/Content-Disposition。
- 影响：大文件并发下载时内存放大，且不支持 Range/断点续传。

## 6. 流式 / SSE 语义（12 条）

证据：`docs/grok2api-audit/02-streaming.md`。移植基点核对：语义 idle 实现与 B **逐行等价**（唯一差异是丢失 `TimedOut()`，A2-11）。

### A2-1 [P1] 上游重复帧（doom loop）零检测，且重复帧恰好让语义 idle 永不触发

- A：两条上游读取路径（`readResponseSSE` 与原生 `consumeCompatibleSSE`）把帧直接交给消费者，无重复计数；语义 idle 把 `response.output_text.delta` 等记为"有效生成"并重置截止时间（`grok2api_streamidle.go:156-164`），因此死循环正好是 idle 唯一不会杀死的形态。
- B：在协议转换/缓冲/stop filter **之前**跟踪可见与推理 delta（`conversation/stream.go:297`），Content 连续同一 delta >128 次、Reasoning >256 次即以 `neterror.ErrUpstreamOutputLoop` 终止并映射为 `upstream_output_loop`。
- 影响：Web/Console 进入重复输出后流会持续到 600s 总超时（`config/grok_limits.go:38` 默认，可配 86400s），持续消耗账号配额并把重复内容写进客户端上下文。
- 修复：移植 doom-loop 计数（含 128/256 阈值与专用错误码）。

### A2-2 [P1] 下游写错误被丢弃且无写超时；移植注释依赖的"client write deadline"不存在

- A：`writeSSEBytes` 对 4 次 `Write` 全部丢弃返回值（连短写也不检测，只有 `messages_search.go:22-32` 的 `checkedStreamWriter` 检测）；无写超时。语义 idle 在 `readers == 0` 时停止计时（`grok2api_streamidle.go:169-174`，测试 `grok2api_streamidle_test.go:173` 明确断言），因此下游卡住时既无写超时打断也无读超时兜底。
- B：每次写前重设 30s 写截止时间，写错误即中止整条流并释放上游。
- 影响：单个卡死客户端（TCP 零窗口/慢客户端/`io.Pipe` 阻塞）即可永久占用 goroutine + 上游连接 + 账号并发额度（外部可触发的资源耗尽面）。
- 修复：检测写返回值、加写截止时间与整体写超时。

### A2-3 [P2] 上游私有控制事件 `response.doom_loop_check` 原样透传

- A：无等价过滤（`grep -i doom` 在非测试代码 0 命中），原生 Build Responses 路径把它当普通事件转发。
- B：`cli/responses_response.go:92-103` 的 `isPrivateBuildControlEvent` 无论以 `event:` 名还是 data `type` 出现都丢弃。
- 影响：Codex/Grok TUI 等严格客户端收到 Responses schema 未定义的事件，可能把正常生成判为协议错误。
- 修复：在 SSE 边界过滤该事件。

### A2-4 [P2] Build 专用语义 idle 被套用到全部通道；检测器活动模型与 Web 帧形状不相容

- A：无字节级包装，语义 idle 用于三个通道；包装条件额外要求 `StatusCode == 200` 且 `Content-Type` 含 `text/event-stream`；检测器只识别 Responses 生成事件，而 Web 帧是 `{"result":{"response":…}}` 信封，无法产生被识别的根 `type`/事件名。
- B：只对 CLI/Build 用语义 idle，Web/Console 用"任何字节都重置"的 `providerstreamidle.ReadCloser`；条件只需 `request.Streaming && 2xx`。
- 影响：Console 上"只有 keepalive 的长静默段"在 B 存活、在 A 被 120s 杀掉；Web 上若上游声明 SSE 则连续数据也无法续命（等价按累计读取时间计时），未声明 SSE 时则完全没有 idle 保护。

### A2-5 [P2] idle 超时无专用错误分类

- A：哨兵 `errGrokSemanticIdle` 是包私有且从未被比较，错误沿 `consumeCompatibleSSE` 泛化：Responses 面 `stream_read_error`，Chat 面 `stream_error` 且文案写成 `stream parse error: upstream stream idle timeout`。
- B：共享哨兵 + `errors.Is` 可达 → 三协议各给专用 code。
- 影响：客户端无法区分"上游静默超时"与真正的传输/解析错误，重试与告警退化为通用 5xx。

### A2-6 [P2] 原生 Responses 流被强制重新分帧并追加 `data: [DONE]`

- A：无论上游是否发送 `[DONE]`，都会在终止事件后自行补 `data: [DONE]\n\n`（`handler_responses_store.go:427`），并把每帧按 LF 重新分帧。
- B：原生 Responses 是字节透传（仅在缺字段时补齐同一行 JSON），结束符由上游决定。
- 影响：Responses 协议本身不用 `[DONE]`；严格客户端会报未知帧，且同一上游流两边字节表示不同，无法用字节比对做灰度。

### A2-7 [P2] 本地合成 `response.failed` 缺字段

- A：`handler_responses_store.go:414` 只给 `id/object/status/model/error`，缺 `created_at`/`completed_at`/`output`/`sequence_number`，`responseID` 可能为空串；失败帧后还补 `[DONE]`。
- B：`inference/handler.go:1548` 输出完整信封（含时间戳、`output:[]`、`sequence_number`），并经 `sanitizeResponsesEvent` 补 `id`、`error.id`；Responses 协议不追加 `[DONE]`。
- 影响：Grok TUI 明确依赖 `model`、时间戳、`output` 数组，残缺信封会解析失败。

### A2-8 [P2] Chat 中途错误帧形状不同

- A：带 `event: error` 事件名 + `request_id`，错误对象缺 `type: "api_error"`（`http_helpers.go:136-147`）。
- B：data-only（无 `event:` 名）+ `normalizeOpenAIStreamError` 默认带 `type`；两边都发 `[DONE]`。
- 影响：按 `error.type` 分派的客户端在 A 上取到空值。

### A2-9 [P2] SSE 响应头缺 `X-Accel-Buffering: no`，多写 hop-by-hop `Connection`

- A：全仓（含 `deploy/`、`docs/`）无任何 `X-Accel-Buffering` 设置，自行拼 `Connection: keep-alive`；`/v1/responses` 桥接成功分支不复制内层已提交头，导致 `X-Grok2api-*` 告警头丢失（Messages 桥接则复制，`handler_messages.go:802-804`）。
- B：每个 SSE 响应显式声明 `X-Accel-Buffering: no`，并把 `Connection` 排除在透传之外。
- 影响：部署在 nginx（默认 `proxy_buffering on`）后 SSE 被缓冲、实时性失效。

### A2-10 [P3] Anthropic 流式 usage 形状不同

- A：`message_start` 用固定零值（不看上游 `response.created` 的 usage），终端 `message_delta` 只在收到 OpenAI usage chunk 时覆盖，缺 `cache_creation_input_tokens` 与 `output_tokens_details`（`server_tool_use` 仅在 `finish()` 补）。
- B：两处都由上游 usage 构造，含 `cache_creation_input_tokens:0`、`cache_read_input_tokens`、`thinking_tokens`、`cost_in_usd_ticks`、`server_tool_use.web_search_requests`。
- 影响：Claude 客户端/统计拿不到缓存创建与 thinking 分解（协议字段问题）。

### A2-11 [P3] 移植时丢失 `semanticIdleReadCloser.TimedOut()`

- A 用测试辅助 `grok2apiTestTimedOut` 直接读字段；B 的方法在生产代码中只被测试使用。无运行时影响，仅诊断/测试接口缺失。

### A2-12 [P3] idle 超时配置语义不同

- A：单值三通道共用、默认 120s、可配到 3600s、无下限保护，`[30s,10min]` 归一化缺失。B：按通道配置（Web 默认 90s、Console/Build 默认 120s）+ `[30s,10min]` 归一化，Build 另有 `DefaultBuildResponseHeaderTimeout = 5min`。
- 影响：Web 通道默认静默容忍度比 B 宽 33%，上限是 B 的 6 倍；无法只放宽某个通道。

---

## 7. 出口 / 反爬 / 身份（18 条）

证据：`docs/grok2api-audit/07-egress-antibot.md`。

### A7-1 [P0] 出口节点校验失败把带凭据的代理 URL 原文写进日志

- A：`internal/grok/egress/node.go:95` 直接把 `n.URL` 打进 `slog.Warn`；同包 `flaresolverr.go:148` 已有 `sanitizeFlareSolverrMessage`（含 `proxyCredentialPattern`）但此处未用。
- B：出口校验错误文本不含 URL，探针/求解错误还再过一次脱敏。
- 影响：任何一次节点配置写错（端口非法、scheme 不支持）都会把 `http://user:pass@host:port` 明文写进运行日志/日志采集系统，代理账号泄漏。
- 修复：日志只输出节点名/ID 与脱敏后的地址。

### A7-2 [P1] `x-statsig-id` 为本地伪造串，无签名/缓存/失效

- A：每次随机拼一个 base64 的 `x1:TypeError…`，与账号/页面无关，反爬后也不更换。
- B：由外部签名器基于首页 metaContent 生成，`validStatsigID` 校验 base64 长度 70，按 URL+method 缓存 1h，反爬时 `Invalidate` 重签。
- 影响：伪值一旦被上游规则命中，只能靠换 IP/clearance 恢复，没有"重新签名"通道，表现为 chat 持续 403/流内拒答。

### A7-3 [P1] Console（DPoP）请求完全绕过出口代理池

- A：`client.go:749-758` 的 `egressScopeForURL` 能为 `console.x.ai` 映射出 scope，但 Console 路径从不调用 `Acquire`，用进程级 `c.httpClient`。
- B：Console 每个请求（含 `/v1/dpop/token`）都绑定 lease。
- 影响：为 console 配的专用出口与 clearance 完全无效，上游看到同一账号在不同 IP/指纹间跳变，更易触发风控。

### A7-4 [P1] Build/CLI 请求被注入浏览器 UA 与 grok.com clearance

- A：flaresolverr 模式下为 `cli` scope 去 grok.com 求解，把解出的浏览器 UA 与 `cf_clearance/__cf_bm` 合并进 `cli-chat-proxy.grok.com` 请求。
- B：Build scope 既不注入 UA 也不合并 clearance cookie。
- 影响：① `cf_clearance` 是 host 作用域凭据，跨源发送属凭据外泄；② UA 声称 Chrome 而其余 CLI 头声称 CLI，指纹自相矛盾（CLI 网关侧最易命中的组合）。

### A7-5 [P1] 出口亲和是常量，所有账号共用一个出口与一份 clearance

- A：web/CLI 亲和常量 → sticky 键 `scope:app_chat:grok-default`，落定后所有账号长期压在同一节点、同一 UA、同一份 clearance。
- B：亲和来自凭据身份，`AcquireCredential` 把 SSO/Web/Console 两套映射收敛到同一 `sso_<hash>`。
- 影响：一个账号触发风控即牵连整池；单账号被封后 clearance 连带失效导致全量重新求解。

### A7-6 [P1] clearance 缓存键不含代理/求解器/target，无版本绑定/分布式锁/持久化

- A：换节点代理地址（同名）或换 FlareSolverr 实例后，只要未到 600s TTL 就继续用旧 clearance/旧 UA；`m.version` 只用于失效竞态不参与新鲜判定；配置热更会清零健康与 clearance。
- B：用 `(solver, target, proxy)` 指纹 + `clearanceVersion` 判定，跨实例加锁、落库复用。
- 影响：换代理后持续用旧出口对应的 clearance 请求新出口（必被 CF 拒），表现为反复 `egress clearance solve failed; reusing stale clearance`（`manager.go:242`）；多实例各解一次浪费求解配额。

### A7-7 [P1] TLS/HTTP2 指纹与 UA 版本不绑定

- A：ClientHello 取库内"最新 Chrome"，UA 可声称 145 或 Windows，`Sec-Ch-Ua` 与 UA 无绑定；HTTP/2 面（SETTINGS/伪头顺序）是 `x/net/http2` 默认且无配置入口；ALPN 协商出 http/1.1 直接报错而非回退。
- B：由 UA 大版本查表 + 最近邻回退 + 固定兜底 `Chrome_146`，tls-client `ClientProfile` 同时覆盖 JA3 与 h2 指纹，Build 传输开启 h2 PING（`ReadIdleTimeout`/`PingTimeout`）。
- 影响：UA 与 TLS 指纹错配是 CF/xAI 最常用的关联特征；A 的半死连接只能等请求落到上面才暴露。

### A7-8 [P1] Web chat 从不使用 mgw WebSocket

- A：`fromLegacyPayload` 恒错 → 永远退回 REST；出口启用时 WS 直接拒绝（"配置后功能消失"而非降级）。
- B：主路径即经 lease 的 gateway WS，复用 lease 的浏览器 TLS/UA/cookies，具备代理池重试与 403 clearance 失效。
- 影响：无法用 WS 规避 REST 侧反爬/clearance 抖动。

### A7-9 [P1] 无流内反爬识别

- A：只把 `streamErrors` 写进诊断，按普通上游错误返回；不重签 statsig、不淘汰 clearance、不标记节点。
- B：流内反爬（`code=7`/"anti-bot"）→ 失效签名 → 同请求重试一次 → 节点记 403（触发 clearance 失效、健康降级）→ 返回 `anti_bot_rejected`。
- 影响：命中后不会自愈，后续请求继续打在已被判定的会话上，形成持续 403/空响应。

### A7-10 [P1] 节点健康无探测/无持久化/不淘汰连接池，冷却固定 30s

- A：降级窗口固定 30s、无指数退避、失败后不重建该节点的连接池、健康分不落库（重启/热更清零）、无主动探活恢复手段。
- B：`FailureCount` + 指数冷却上限 10min + `LastError` + 健康落库 + 失效并关闭该节点客户端 + 后台探针完成后刷新节点快照。
- 影响：网络抖动时要么 30s 后无条件回到坏节点，要么复用连着坏节点的 keep-alive 连接；多实例无法共享健康。

### A7-11 [P2] 节点失败指标统计口径反了

- A：`grok_egress_node_failures_total`（`egress/metrics.go:21`）只在"已经降级"的节点再次失败时自增，健康→降级那一次永不计数；`node_recoveries_total` 反而记真实跃迁。
- 影响：告警看板漏掉全部首次故障，低估节点故障率。

### A7-12 [P2] Cloudflare Cookie 白名单少收 `_cfuvid`/`cf_chl_*`，且不去重不校验长度

- A 丢弃设备指纹类 `_cfuvid` 与挑战态 `cf_chl_*`，不拒绝超长/含控制字符的值；B 白名单更全并丢弃畸形值。
- 影响：clearance 复用时被 CF 认为会话不完整，容易二次挑战；畸形 `Cookie` 头可能被送到上游。

### A7-13 [P2] 会话身份探测不走出口，且请求头集与场景不符

- A：用全局 client 直连（IP 与随后请求不一致），GET 上发 `Content-Type`/`Origin`/Sentry `Baggage` 与写死的 Chrome148/macOS hints；`baseHeaders` 缺 `Accept-Encoding`。
- B：同一 lease、不带这些头，hints 由 UA 推导，`Accept-Encoding: gzip, deflate, br, zstd`。
- 影响：身份探测与业务请求不同 IP/指纹是明显异常；编码能力指纹也可区分。

### A7-14 [P2] CLI/Build 缺 trace 身份头，`x-grok-session-id` 未规范化（与 A3-10/A3-11 同源）

### A7-15 [P2] `x-cluster` 发送范围过宽（所有 Console 请求）

- B 只对 `/responses` 声明该集群路由提示；A 对 dpop/token、images、videos、voice 都带。真实浏览器只在 responses 流量上带该头，属可区分信号。

### A7-16 [P2] 403 处理：仅识别 CF 挑战时才失效 clearance

- A 需要 `IsCloudflareChallengeBody`/`CF-Mitigated` 命中才失效；B 对走 lease 的 403 默认失效该会话 clearance。无特征体的 403 会让 A 反复复用被污染的 clearance。

### A7-17 [P2] 固定伪造 Client Hints 与轮换 UA 矛盾

- A 写死 Chrome 148 + `"Not/A)Brand";v="99"`（旧 GREASE 格式）、`Sec-Ch-Ua-Platform` 恒为 `"macOS"`，即使 UA 是 145–147 或 Windows；`Baggage` 里还有一年前的固定 Sentry release（`client.go:125`）。B 由真实 UA 推导 hints。

### A7-18 [P2] 出口/浏览器客户端缓存永不淘汰，且键不含代理 URL

- A 的缓存键用 `node.Name` 而非 `nodeID`、不含代理 URL → 同名节点改代理后仍复用**旧代理**连接池直到进程重启；降级/热更都不关闭这些池；长跑进程 client/map 无界增长。B 的键含 nodeID+scope+指纹+账号身份，有 TTL/容量上限/变更时主动淘汰关闭。

---

## 8. 用量统计 / 计费 / 缓存（13 条）

证据：`docs/grok2api-audit/09-usage-accounting.md`。

### A9-1 [P0] 没有价格表、成本字段与 Key 计费预留/结算

- A：`cost_in_usd_ticks`/`CostInUSD*`/`EstimatedCost`/`ReserveBilling`/`usage_source` 全仓 0 命中；`internal/audit/audit.go:46-49` 与 journal 行无成本列；`internal/store/store.go:309` 的 `ApiKey` 没有额度字段。
- B：`domain/audit/pricing.go:199-202` 用 `uncached*inputPrice + cached*cachedPrice + output*outputPrice` 得出 USD-ticks 并写入 `EstimatedCostInUSDTicks`/`PricingModel`/`PricingVersion`；`gateway/service.go:1030-1031` 按 `EstimateOfficialTextReservation` 预留、`:1103` 结算。
- 影响：作为带 API Key 的多渠道代理，A 无法对 Key 做额度扣减或超支拦截，审计也无法回答"这次请求花了多少钱/哪个 Key 快超支"。
- 修复：补价格表 + 预留/结算（或至少记录成本字段供外部计费）。

### A9-2 [P1] 不记录 usage 来源，估算值与上游报告值不可区分

- A：同一 grok 账号/模型，一次报上游数字、下一次报 `(runes+3)/4` 估算，journal 里形状相同。
- B：provider 定义层声明权威性（`web/definition.go:31` estimated、`cli/definition.go:24` upstream、`console/definition.go:40` upstream）并持久化成受约束列。
- 影响：按 journal 回溯用量/成本时无法判断该行是上游账单还是网关猜的，误差无法归因，也无法把 estimated 行排除在计费外。

### A9-3 [P1] 归一化上游 usage 时丢弃 `cost_in_usd_ticks`/`num_sources_used`/`context_details`

- A：`internal/grok/console.go:338-353` 的 `consoleUsage` 白名单重建 map，上述字段全部消失（`responsesUsageFromChat` 克隆的源已被洗过）。
- B：`conversation/chat_response.go:48-58` 原样透传上游计量。
- 影响：Build/Console 平面真实成本与上下文长度在 A 中不可见，下游也无成本字段可透传。

### A9-4 [P1] Anthropic usage 缺 `thinking_tokens`，`cache_read_input_tokens` 条件性缺省

- A：`handler_messages.go:784-798` 只在 `cached > 0` 时补 `cache_read_input_tokens`，无 `output_tokens_details.thinking_tokens`、无 `cost_in_usd_ticks`/`num_sources_used`/`context_details`，`output_tokens` 不做 `max(0,·)`。
- B：`conversation/messages_response.go:108-125` 无条件输出 cache 字段（缺省 0），`thinking_tokens = min(output, reasoning)`。
- 影响：Claude Code 的费用/缓存展示拿不到 thinking 与 cache-write 维度，无法区分"无缓存"与"字段缺失"。

### A9-5 [P1] `message_start` usage 恒为 0

- A：`handler_messages.go:841/844` 固定零值 map（上游 `response.created` 的 usage 完全不看），直到 `message_delta` 才被纠正。
- B：构造响应时就知道 input token，`message_start` 给出真值（`conversation/messages_stream.go:9-19`、`web/chat.go:2166`）。
- 影响：依赖 `message_start` 统计输入 token/缓存命中的客户端恒为 0。

### A9-6 [P1] prompt cache key 派生信号集不同（与 A3-6 同源）

- A 只能拿到 `x-session-id` 或原始 `user_id` 全串；且没有 `isClaudeCodeTitleRequest` 之类的排除，标题生成旁路请求会覆盖同一 `(model, sessionKey)` 的 reasoning replay 槽位。
- B 生成带 agent/window 维度的稳定种子（`claude:<sid>:agent:<id>`、`codex:window:<id>`），并排除标题请求。
- 影响：`cached_tokens` 长期为 0，replay 可能被辅助请求污染，A/B 混跑时缓存与 replay 状态不可迁移。

### A9-7 [P1] 没有本地额度扣减

- A：`quota.go:122/160` 只覆盖上游数字，两次上游同步之间 `Remaining` 陈旧偏高，负载均衡不因本地消耗改变选择。
- B：`gateway/service.go:1139-1151` 每个成功请求把 `remaining` 减 `max(1, QuotaUnits)` 并通知 selector 消费（weekly 明确不递减）。
- 影响：上游头缺失时 A 完全没有消耗记录。

### A9-8 [P1] Build billing 不反推百分比，且额度呈现优先级相反

- A：`cli_billing.go:65/91` 在百分比缺失时直接放弃；`api/quota_projection.go:297` 优先取 percent 周窗口。
- B：`cli/billing.go:136-143` 用 `used/limit*100` 或 `onDemandUsed/onDemandCap*100` 反推；`account/service.go:1185` 优先级为 MonthlyLimit → OnDemandCap → PrepaidBalance → percent。
- 影响：同一份 billing 响应两边展示的"额度/已用"量纲不同；A 只有月度数字的账号会显示"无额度"。

### A9-9 [P2] 工具调用的 completion token 估算把 JSON 结构算进去

- A：`usage_estimate.go:106-115` 估算 `json.Marshal(toolCalls)` 的体积（含键名/引号/逗号）。B：`web/chat.go:2120-2126+1800` 只算 name + arguments 文本。
- 影响：工具密集会话的 completion_tokens 系统性偏高（A9-1 修好后会直接多计费）。

### A9-10 [P2] prompt token 估算范围不同

- A：`usage_estimate.go:40-64` 把 role/name/tool_call_id、tool call 的 id/type、整个 tools 数组与 tool_choice 的 JSON 都按 rune/4 计入。B：`web/chat.go:247+351` 只算渲染后的 prompt 文本。
- 影响：A 的 prompt_tokens 系统性高于 B，跨实现不可比。

### A9-11 [P2] 图片 usage 是造出来的数

- A：`usage_estimate.go:160-184` 把 prompt 文本折成 input token 并给每张图 64 个 output token（`64` 在本仓无来源）；`:91` 还把 `file` 输入块按图片计价（当成 256 image token）。B：`web/image.go:1039-1042` 全 0、`:1471` 非流式不给 usage。
- 影响：图片请求上报不存在的 token/成本。

### A9-12 [P2] 审计/运营聚合缺 cached/reasoning/total 与 priced/unpriced 维度

- A：`opsagg.go:48-49`、`api.go:1110` 只加 input+output，`CachedInputTokens`/`ReasoningTokens` 落库后无人汇总。B：`audit_repository.go:689-698`、`audit/service.go:616-632` 汇总 5 个 token 维度 + 成本 + `priced_tokens`/`unpriced_tokens`。
- 影响：无法回答"缓存省了多少""多少用量没算钱"。

### A9-13 [P2] 缺 B 新增的"会话作用域 + call_id"推理证明缓存 `[漂移]`

- A：`session_state.go:165` 只按 model+session 存整轮 replay 列表（TTL 1h），`reasoning_replay_items.go:351/363` 要求已有该 call 的 output 才回放。
- B：`conversation/reasoning_cache.go:11/53/175` 新增有界 LRU（4096 条/30min，含 `|` 后缀归一化与 `toolu_` 等价候选），按 `(conversation scope, call_id)` 精确配对，`RememberReasoningForEnvelope` 支持多并行调用共享同一 reasoning item。
- 影响：多轮 Build 工具循环中客户端只回传部分历史时，A 更容易整体丢弃 reasoning。

## 9. 账号轮换 / 配额 / 限流（16 条）

证据：`docs/grok2api-audit/06-accounts-quota.md`（132 处 `file:line` 引用已机械校验）。

### A6-1 [P1] 质量降级（encrypted-thinking dump / 缺失思考）的识别、扣分与重试整体缺失 `[漂移]`

- A：无等价物。B 的 `application/gateway/quality_retry.go`：`defaultQualityMaxAttempts=6`、`defaultMissingThinkingCooldown=12h`、第二次 missing-thinking 直接 `Enabled=false`；该文件在移植基点 **之后**被 `7f3f3d3c` 改动（新增 `defaultFakeEncFlushMS=2000`、`defaultCipherDroolVisible=1024` 以及 fake-enc/明文占比/cipher-drool 三个 dump 判定），按基点移植会继续放行 `vis<8` 的 dump。
- 影响：质量退化的账号永不被隔离，客户端持续收到"看着像 200、内容是迟到整段正文或状态循环"的响应，账号池被污染。
- 修复：按 HEAD 移植 `quality_retry.go`（含新阈值），不要按基点移植。

### A6-2 [P1] 401（上游拒绝凭据）后 A 只冷却 5 分钟并无限回池

- A：5 分钟后同一 cookie/refresh token 回到候选继续打上游（`VerifiedAt` 只影响后台轮询频率，不影响请求路径选号）。
- B：标 `AuthStatus=reauthRequired`，直到人工重新登录才回到候选。
- 影响：对一个已被 xAI 判死的 session 反复发起鉴权失败请求（每 5 分钟一次 × 并发数），正是上游触发账号级风控的典型特征。

### A6-3 [P1] 403 处置粒度：A 整号 10 分钟；B 分三档

- A：模型被拒与账号被封都压成整号 `StatusCode=403` + 10 分钟。
- B：definitive block → 永久 reauth；"access to the chat endpoint is denied" 类 → Build 只封该模型 5 分钟（`gateway/service.go:1542` `MarkModelAccessDenied`）；其余 403 → 不惩罚只换号。
- 影响：一次模型级 403（能力不足）会把整号所有模型停 10 分钟；账号真被封时 10 分钟后继续用。

### A6-4 [P1] 429 冷却无上限，且 A 内部两套冷却自相矛盾

- A：把上游 `Retry-After`/`resets in:` 无上限写进 `QuotaResetAt`（可停数小时）；同一个 `StatusCode="429"`，LB 判 1 小时不可用，而 `accountpolicy.AccountHeld` 1 分钟就认为可用。
- B：`boundUpstreamRetryAfter` + `cooldownBase 30s → cooldownMax 30min`（配置硬上限 24h）指数退避。
- 影响：小时级限流的大 team 号被停到窗口结束，有效池骤减；运维面板与实际选号状态互相矛盾。

### A6-5 [P1] 免费额度耗尽作用域：A 把"按模型"耗尽当整号 24h 封停

- A：匹配串 `used all the included free usage` 是 B 模型级句 `used all the included free usage for model` 的前缀，必然把模型级升级为整号 24h。
- B：`subscription:free-usage-exhausted` 才整号，`…for model` 只封该模型。
- 影响：Build Free 号在 `grok-4.6` 耗尽免费额度后，视频/图像/其他模型一起停 24h。

### A6-6 [P1] 付费额度不参与选号门控；402 恢复窗口固定 24h

- A：`UsageCurrent`/`MonthlyLimit` 只展示不参与过滤，额度耗尽但上游未返 402 时继续发请求。
- B：`Billing.IsExhausted` 门控 + 按账期 `PeriodEnd` 重探（`paidProbeRetryInterval=15m`）。
- 影响：无额度付费号的整个账期变成持续 402 往返；24h 后无条件放回。

### A6-7 [P1] 团队级 429：A 不换号并在请求内阻塞等待

- A：把"团队限流"当"换号也没用"，不换号、把错误抛客户端，并把冷却下沉为下一次请求的等待（`teamCooldown.Wait` 上界 600s）。
- B：发送前预检 team+model 屏蔽，`attempt--/continue` 立刻换号，并把 Retry-After 回客户端。
- 影响：单请求可能白等几十秒到 600s（占用并发槽与配额），期间其他可用账号闲置。

### A6-8 [P2] 选号排序与公平性

- A：只按 `conns/weight` 取最小再随机，不看 `Priority`、不看"上次选中时间"。
- B：11 级比较器（priority/tier/quota/billing 新鲜度/inFlight/remaining/lastSelectedAt）。
- 影响：带 `priority` 的分组在 A 中失效；大池下随机平局形成热点账号。

### A6-9 [P2] 大池分层实现不同

- A：按 4 个池名依次全量扫描 + 全量排序，池顺序固定（`super` 打满才轮到 `lite`）。
- B：1024 shard + windowSize 64 + 4 窗口后全量回退。
- 影响：3000+ 账号池上每请求都可能扫描排序整个候选切片。

### A6-10 [P2] 换号/重试预算相差两个数量级

- A：换号上限默认 5 / 硬上限 20，`MaxRetries` 3；B：`MaxAttempts` 999 / 上限 65535 / `-1` 无限，另有非账号失败指纹上限 16、绑定账号只 1 次。
- 影响：同样的坏池，A 第 6 个账号就直接把错误抛给客户端；把上限调大又因缺少指纹上限与 pinned 保护而放大重试风暴（两项须一起改）。

### A6-11 [P2] 5xx 不写账号状态

- A 只换号不隔离；B `markSoftFailure` 给 5s 软隔离且不累计失败计数。
- 影响：上游整体 5xx 期间 A 下一个请求立即重选同一批账号。

### A6-12 [P2] 刷新调度：A 固定 30 分钟 ticker、每轮最多 5 个

- B：按 DB 到期时间 set timer、提前 3 分钟、批 100、退避 30s→15min。
- 影响：1000 个号需约 200 分钟走完一轮；`refreshCLIAccount` 的 5 分钟前置条件只在扫描到时判断，大池下长时间漏刷 → 请求路径直接吃 401。

### A6-13 [P2] 永久失效凭据无收敛出口

- A：只有 `StatusCode=401` + 每 30 分钟重验，唯一停用手段是管理端手动 `Enabled=false`。
- B：`RefreshPermanent` → `reauthRequired` + auto-clean（批 100/单轮删 10/`MinAge` 1m–30d）。
- 影响：失效 OAuth 号长期占用池与刷新配额，并持续写审计/日志。

### A6-14 [P3] 订阅/等级推断阈值不同

- A：只看 `limit` 数值、无 mode 维度、冲突时 auto 优先（150/50/140/25/70/12/30/20/8/7），并引入 B 没有的 `lite` 等级。
- B：按 auto(7/20/50/150)+fast(30/140/400) 形态表判级并取最低。
- 影响：auto=150/fast=30 的号会被 A 判成 heavy，低权限账号被放进高权限池（`models.go:118` 的 `PoolCandidates` 据此过滤）。

### A6-15 [P3] 无本地额度消耗计数

- A 只在收到下一个 `x-ratelimit-*` 头时被动覆写 remaining；B 每次成功请求 `ConsumeQuota` + `selector.ConsumeQuota`，选号即时判耗尽（与 A9-7 同源）。

### A6-16 [P3] 会话粘滞无解绑口

- A 只有 Save/Get，TTL 固定 1h，冷却结束后会话弹回原账号；B 在 401/402/403/429、额度耗尽、质量降级时 `DeleteByAccount` 永久解绑。

---

## 10. 修复优先级

**第一批（安全 + 硬 400，建议立即处理）**
1. `A5-22` 图片/附件 URL 服务端抓取 SSRF（`internal/grok/util.go:446`）。
2. `A1-1` / `A1-2` / `A1-3` Messages 入口三处硬 400（tool_result 数组、document、web_search tool_choice）——这三条直接决定 Claude Code 能否用。
3. `A8-1` 上游错误正文与内部错误串透传（含出口节点标识）。
4. `A7-1` 代理 URL 凭据写入日志（`egress/node.go:95`，同包已有脱敏函数可复用）。
5. `A5-13` 媒体资产下载外发 SSO Cookie；`A5-10` `X-Forwarded-Host` 注入；`A5-6` 客户端可控 `nsfw`。
6. `A5-1` `/images/edits` 线格式不兼容（决定该端点能否被 grok2api 客户端复用）。
7. `A8-12` `inference_auth_enabled=false` 时 `/v1` 全开放——若不是有意设计，建议至少对推理前缀强制鉴权。

**第二批（协议契约，影响 SDK/客户端兼容）**
8. 统一错误出口：`A8-2`、`A8-3`、`A8-4`、`A8-6`、`A8-15`、`A8-16`、`A5-33`、`A5-34`、`A1-13`（`http.Error` → JSON 错误对象 + status 映射 + Retry-After）。
9. `A5-9` 媒体/音频端点补 `/v1` 前缀 + 修 `toStandardMap` 的 `/v1/videos/{id}/content` 自链接（`handler_videos.go:88`）。
10. 模型命名体系：`A4-1`、`A4-2`、`A4-3`、`A4-4`、`A4-9`。
11. Responses 兼容层：`A3-1`（事件字段补齐）、`A3-3`（custom 工具）、`A3-4`（input 历史归一化）、`A3-2`（apply_patch operation）、`A3-13`（整型 arguments）、`A3-5`（schema 展平，按 HEAD 移植）。
12. 流式守卫：`A2-1`（doom loop）、`A2-2`（写错误与写超时）、`A2-3`（doom_loop_check 过滤）、`A2-9`（`X-Accel-Buffering`）。

**第三批（账号/配额/计费正确性）**
13. `A6-2`、`A6-3`、`A6-5`（冷却与作用域）、`A6-7`（团队 429）、`A6-1`（质量降级，按 HEAD 移植）。
14. `A9-1`（成本账本/额度扣减）、`A9-2`（usage 来源）、`A9-7`/`A6-15`（本地扣减）。
15. `A7-3`/`A7-4`/`A7-5`（出口身份贯通所有通道 + per-credential 亲和）、`A7-2`/`A7-9`（statsig 与流内反爬闭环）。

**第四批（数值/字段/文案对齐）**：其余 P2/P3，建议按 A 路径聚合批量处理。

---

## 11. 附录：发现索引与去重说明

| 区域 | 报告文件 | 条数 | P0 | P1 | P2 | P3 |
| --- | --- | --- | --- | --- | --- | --- |
| Chat Completions / Anthropic Messages | `docs/grok2api-audit/01-chat-messages.md` | 20 | 1 | 3 | 10 | 6 |
| 流式 / SSE | `docs/grok2api-audit/02-streaming.md` | 12 | 0 | 2 | 7 | 3 |
| Responses API / CLI(Build) | `docs/grok2api-audit/03-responses-cli.md` | 14 | 0 | 8 | 6 | 0 |
| 模型目录 / 路由 | `docs/grok2api-audit/04-models.md` | 17 | 0 | 9 | 5 | 3 |
| 媒体 / 音频 | `docs/grok2api-audit/05-media-audio.md` | 45 | 2 | 18 | 20 | 5 |
| 账号 / 配额 / 限流 | `docs/grok2api-audit/06-accounts-quota.md` | 16 | 0 | 7 | 6 | 3 |
| 出口 / 反爬 / 身份 | `docs/grok2api-audit/07-egress-antibot.md` | 18 | 1 | 9 | 8 | 0 |
| 错误契约 / 鉴权 / 校验 | `docs/grok2api-audit/08-errors-auth.md` | 16 | 1 | 6 | 8 | 1 |
| 用量 / 计费 / 缓存 | `docs/grok2api-audit/09-usage-accounting.md` | 13 | 1 | 7 | 5 | 0 |
| **合计** | | **171** | **6** | **69** | **75** | **21** |

跨区域重复计数（同一根因在不同面向被两次记录，去重后独立缺陷约 156 条）：

- `store` 缺省语义：`A1-11`（chat 面）↔ `A3-8`（原生 Responses 面）。
- prompt cache / 会话种子：`A3-6` ↔ `A9-6`；CLI 会话头规范化：`A3-10` ↔ `A7-14`；CLI 身份头：`A3-11` ↔ `A7-14`。
- Anthropic 错误类型恒为 `invalid_request_error`：`A1-13` ↔ `A8-6`；上游错误正文回传：`A1-13` ↔ `A8-1` ↔ `A5-35`。
- 上游 401/402/403 未屏蔽：`A5-33` ↔ `A8-3`。
- 本地额度扣减：`A6-15` ↔ `A9-7`；客户端 Key 计费上限缺失：`A8-14` ↔ `A9-1`。
- `grok-imagine-image-pro`/`-quality` 目录冲突：`A4-12` ↔ `A5-7`。
- Responses 失败信封缺字段：`A2-7` ↔ `A8-16`。
- Agent 工具能力判定与 Web 平面：`A4-8` ↔ `A5-12`。

已核实**不是**差异的项（避免误改）：

- `compatibleSSEEvent` / `consumeCompatibleSSE` 编解码与 B 逐字节一致（多行 data、`id`/`retry`/注释、BOM、8 MiB 上限、短写检测均正确），唯一差异是丢失 `TimedOut()`（`A2-11`）与哨兵错误类型（`A2-5`）。
- 语义 idle 的并发/计时实现与 B 逐行等价（`internal/grok/grok2api_streamidle.go` vs `B:backend/internal/infra/provider/cli/semantic_streamidle.go`）。
- 两侧都不在下游推理流中发送 SSE keepalive/心跳帧。
- 凭据提取（`Authorization: Bearer` vs `x-api-key` 优先级）一致；`?key=`/`?api_key=` 两侧都不支持。
- `EstimatedFreeBuildTokenLimit=500_000`、Free 窗口 24h、ratelimit 正则与 RPS 2s / RPM 1min 默认值两侧一致。
- Codex 目录的上下文窗口与模态元数据表逐条相同（仅 `description` 文案不同，`A4-17`）。
- `internal/api/api.go` 不是公开模型列表入口（公开列表在 `internal/handler/models.go`），初版任务书中的路径指向有误，已在 `A4` 报告中修正。

`[待验证]` 条目（需运行时实证，未计入结论）：`A1-9`（上游是否接受 `stop`）、`A1-14`（上游是否接受 `input` 中的 `output_text`）、`A8-14` 内部 Key 分支、`A5-7` 运行期路由合并次序、`A5-22` 未在实例上实证内网抓取、`A5-12` 未做双实例对照。

审计过程说明：`.upstream/grok2api` 为本次审计克隆的上游仓库（未纳入版本控制，可随时删除）；9 份区域报告位于 `docs/grok2api-audit/`。审计期间未修改任何产品源码，`go test ./internal/grok/...` 保持通过。
