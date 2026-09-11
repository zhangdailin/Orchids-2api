# Grok 与 grok2api 对比复核与差异补齐（2026-09-12）

## 基线

- 上游：`chenyme/grok2api`，`VERSION` = **v3.1.5**，HEAD `8913b53`（2026-09-09 15:43）。本轮 `git fetch` 后 `HEAD..origin/main` 为空，即该克隆与远端一致，未遗漏新提交。
- 当前侧：`orchids-api` HEAD `3187ab0`，工作区干净（除 `.workbuddy-ai/`）。
- 对照源码：本地 `.workbuddy-ai/upstream-grok2api`（上游完整克隆，含 `backend/` 与 `frontend/`）。

## 方法

逐文件、逐行为比对，**不沿用旧报告结论作为缺陷证据**。上一轮报告 `docs/grok2api-current-differences-2026-09-09.md` 的 12 项差异全部回到当前代码复核；复核过程中另发现 3 项旧报告未覆盖的差异。

> 说明：09-09 那份报告与修复是同一次提交（`d6b5ee9`）落库的，报告描述的是修复前状态，因此其 12 项中多数已不成立。

## 一、旧报告 12 项复核结论

| # | 项目 | 09-12 复核 | 代码证据 |
|---|---|---|---|
| 1 | 聊天历史长度 | **已修复** | `web/static/js/grok-tools.js:601-604` `trimChatSessionMessages` 直接 `return 0`，注释明确"请求上限不得静默删除历史"；附件 `dataUrl` 也已持久化（`:641`） |
| 2 | 思考与正文回传 | **已修复** | 消息把 `reasoning` 与 `content` 分开存（`:1462`），`buildChatPayload` 只发送 `content`（`:1968`），思考不再被当作正文回传 |
| 3 | 页面请求协议 | **保留差异（架构选择）** | 页面走 Chat Completions，由后端 `responsesPayloadFromChat` 转 Responses；上游页面直接发 Responses。非缺陷，见第三节 |
| 4 | 工具执行展示 | **已修复** | `toolActivities` 保存 `name/status/detail`，流中实时渲染（`:1484`）、历史重放（`:1275`）、并随消息持久化（`:1462`） |
| 5 | 默认思考摘要 | **本轮修复** | 见第二节 B |
| 6 | 固定推理模型控件 | **本轮修复** | 见第二节 C |
| 7 | 模型能力识别 | **已修复** | 改为读取 `/grok/v1/models` 的 `capabilities` 数组（`:1829`、`:1842-1853`），聊天下拉只取 `chat` 能力，不再按模型 ID 字符串猜 |
| 8 | 流中途出错 | **已修复** | 非正常结束抛"连接中断，已保留收到的部分回答"（`:1642`），catch 中 `persistAssistant()` 保留已收内容（`:1663`、`:1672-1675`） |
| 9 | 回放密文校验 | **已修复** | `validReplayCipher`（`internal/grok/session_state.go:208-228`）与上游 `validGrokReplayEncryptedContent` 同规则：RawStd base64、解码 ≥50 字节、熵 ≥0.85、拒 `gAAAA` 前缀与 `=`、≤8 MiB；存储与读取两侧都校验 |
| 10 | 400 恢复步骤 | **已修复** | `recoverChatReasoning`（`chat_reasoning_recovery.go`）覆盖三段：compaction 保护 → 去密文重试 → 会话重置（删 `prompt_cache_key`），条件与上游 `canResetReasoningSession`/`removePromptCacheKey` 一致 |
| 11 | 恢复保留语义与 compaction 保护 | **已修复** | `stripInjectedReasoningReplay`（`session_state.go:290-324`）跳过 `compaction` 项，并把可读 `summary` 转为普通 assistant 消息而非直接丢弃 |
| 12 | 恢复诊断粒度 | **已修复** | `auditAttempt` 按 stage 记录 `reasoning_replay_recovery` / `reasoning_encrypted_content_retry` / `reasoning_session_reset`，并通过 `X-Grok2API-Reasoning-Recovery` 回传阶段（`console.go:452-470`） |

**结论：12 项中 10 项已修复，1 项为本轮修复（#5/#6 合并处理），1 项为有意保留的架构差异（#3）。**

## 二、本轮新发现并补齐的差异

### A. 默认（自动）推理从不请求 encrypted reasoning —— 功能性缺陷

**上游行为**：两个平面都无条件补 include。
- Console：`console/normalize.go:49` 对每个请求调用 `ensureReasoningInclude`，缺则追加 `reasoning.encrypted_content`。
- Build：`cli/normalize.go` 的 `applyBuildResponseDefaults` 同样无条件补齐。

**当前行为**：只在 `reasoning_effort` 显式设置且非 `none` 时才补。

**后果**：页面默认档是"自动"（`reasoningEffort` 为空）→ `payload.reasoning_effort` 不下发 → `ReasoningEffort == nil` → 不请求密文 → 上游不返回 `reasoning.encrypted_content` → `storeReasoningReplay` 从不写入 → **多轮推理回放在最常用的默认档位上完全失效**，而显式选一个强度反而正常。

**修复**：`internal/grok/responses_normalize.go` 改为无条件补 `reasoning.encrypted_content`，与上游两个平面一致。

### B. Chat Completions 缺少 reasoning summary 通道

**上游行为**：
- 页面始终把 effort 与 summary 配对：`{effort, summary:"auto"}`；`auto` 档只发 `{summary:"auto"}`；`none` 档只发 `{effort:"none"}`（`creative-console-api.ts:73-75`）。
- Build 适配器在调用方未指定 summary 时补 `concise`（`cli/normalize.go:104-121`，对应 grok-build 1.0.4 契约）。

**当前行为**：请求结构只有 `reasoning_effort`，没有 summary 通道；Build 路径**无条件覆盖** `summary = "auto"`，既不保留调用方显式值，也与上游适配器默认值（`concise`）不一致。

**修复**：
1. `types.go` 新增 `reasoning_summary` 请求字段（含 `UnmarshalJSON` 解析）。
2. `responses_normalize.go` 新增 `chatReasoningControls`，从 effort + summary 重建 `reasoning` 对象；Build 路径仅在调用方**未提供** summary 时补 `concise`；Console 路径透传，不再凭空发明。
3. `validatePayloadReasoning` 增加 summary 结构校验（非空字符串）。
4. 页面在 `effort !== "none"` 时下发 `reasoning_summary = "auto"`。

### C. 固定推理模型未携带 summary

**上游行为**：`grok-4.20-0309-reasoning` 被固定为 `auto` 且**不下发** effort，最终线形是 `{summary:"auto"}`。

**当前行为**：页面清空 effort 并禁用控件，最终**不下发任何 reasoning**。

**修复**：`buildChatPayload` 对该路由固定 effort 为空、并下发 `reasoning_summary = "auto"`，线形与上游一致。

## 三、原先保留的差异：收敛结果

原先列出 4 项"保留差异"，本轮按"不保留差异"的要求收敛。结果如下。

### 已收敛

**1. 原生 Responses 恢复边界 —— 已收敛（并发现一个真实缺陷）**

复核发现根因比原先记录的更严重：原生 `/responses` 走的是 `handleNativeCLIResponsesAt` **直通路径**，它根本不经过聊天链路，因此**任何**推理解码失败都不会恢复，不只是"受 compaction 约束"。原先记录的"边界问题"实际上是完全缺失。

修复：
- 在直通路径上增加恢复：识别到推理解码 400 后，先清理服务端回放，再只剥离无法解码的密文（保留可读 summary），重试一次，并通过 `X-Grok2API-Reasoning-Recovery` 回传阶段。
- 收窄 compaction 保护：原先"请求带 compaction 项就永不恢复"过宽。上游只在**错误措辞是 compaction 解码失败**且请求真的带 compaction 项时才视为真实压缩拒绝。现改为同一条件（`preservesClientCompaction`），普通的 `invalid_encrypted_content` 即使同时带 compaction 项也可恢复——恢复只改写 reasoning 项，不会碰客户端压缩状态。
- 聊天链路（`recoverChatReasoning`）同步使用同一判断。

**2. reasoning effort 归一化 —— 已收敛**

按上游两个平面的规则实现，不再原样透传：
- Build：`minimal → low`；`xhigh`/`max` → 模型支持 xhigh 时为 `xhigh`，否则 `high`；Composer 完全不发 `effort`（但保留 `summary`）；其余值原样保留。
- Console：`minimal/low → low`，`xhigh/max → xhigh`，其余原样。
- 新增共享策略包 `internal/modelpolicy/reasoning.go`，同时被线上归一化与 Codex 目录使用，避免两处档位表漂移。

### 已收敛（续）

**3. 回放存储粒度 —— 已收敛**

改为上游的归一化条目列表：

- 新增 `internal/grok/reasoning_replay_items.go`：`extractReplayItems` / `normalizeReplayItems`（reasoning 需通过密文校验；message 只保留 `output_text` / `refusal`；`function_call` 需 `call_id` + `name` + `arguments`）、`filterReplayItemsForInput`（按密文与 `call_id` 去重、`toolu_` 前缀等价、工具调用必须能在 input 中找到对应 output，否则不重放）、`insertReplayItems`（优先插在对应 tool output 之前，其次最后一条 assistant 消息之前，否则第一条非 system 之前）。
- 存储：`StoredReasoningReplay` 增加 `Items`，**保留** `EncryptedContent` 并在读取时兼容旧数据（`Items` 优先）。
- 捕获：新增 `internal/grok/reasoning_replay_capture.go`。原生 Responses 从完整响应体提取（JSON 或 SSE 的 `response.completed`）；Console 非流式从响应对象提取；流式在 `response.output_item.done` 处累积。**没有可锚定条目时清除会话状态**，而不是留下会被反复拒绝的陈旧条目。
- 聊天链路只有密文可捕获时，仍写入单条 reasoning 条目，行为与之前等价。

**4. 页面协议 —— 已收敛**

页面改为直发 Responses（与上游页面一致）：

- 新增 `buildResponsesPayload`：`input` 为 `{type:"message", role, content:[{input_text|output_text}]}`，附件走 `input_file`，系统提示走 `instructions`，工具走 `tools`，推理走 `reasoning:{effort?, summary}`。
- 端点改为 `/grok/v1/responses`（同一路由前缀、同一鉴权，无需额外配置）。
- 流式解析改为 Responses 事件：`response.output_text.delta`、`response.reasoning_summary_text.delta`、`response.refusal.delta`、`response.function_call_arguments.delta`、`response.output_item.added|done`、`response.completed|incomplete`、`response.failed`。行扫描、断流保留部分内容、工具活动与思考块渲染全部复用原实现。

> **行为变更（需确认）**：Responses 规范未定义 `temperature` / `top_p`，而 Build 平面会把未知字段原样转发上游。因此采样参数现在**仅在 `provider === "console"` 时下发**；Build 路由不再发送。原先 Chat Completions 路径对所有路由都会发送。如果 Build 上游实际接受这两个字段，可以放开为无条件下发。



## 四、图片 / 视频 / 语音链路复核（本轮补充）

上轮报告明确未覆盖这三条链路，本轮补齐。以下为代码级比对，未调用真实上游。

### 图片

| 规则 | 上游 | 当前 | 结论 |
|---|---|---|---|
| `aspect_ratio` / `size` 映射 | 9 个比例 + 7 个尺寸别名 | `normalizeConsoleImageAspectRatio` 映射表逐项相同 | 一致 |
| `resolution` | `1k` / `2k` | 同 | 一致 |
| `quality` | 仅 `grok-imagine-image-2.0`，`low` / `medium` | 同 | 一致 |
| `response_format` | `url` / `b64_json`，缺省 `url` | 同，另接受 `base64` 别名 | 超集 |
| **`n` 缺省值** | 省略 → **1**；显式越界 → 400 | 省略 → 0 → **400** | **本轮修复** |
| 图片编辑 | 1–3 张、拒绝 `stream`/`partial_images`、`n` 1–10（Console） | 同 | 一致 |

### 视频

| 规则 | 上游 | 当前 | 结论 |
|---|---|---|---|
| 生成 `duration` | 缺省 8，1–15 | 同 | 一致 |
| 延长 `duration` | 缺省 6，2–10；编辑不接受 `duration` | 同 | 一致 |
| `aspect_ratio` | 缺省 `16:9`，7 个合法值 | 同 | 一致 |
| `resolution` | 缺省 `720p`，`480p`/`720p`/`1080p`，且按模型限制 `1080p` | 同 | 一致 |
| `image` 与参考输入互斥 | 互斥 | 互斥 | 一致 |
| `reference_audios` | ≤ 3 | ≤ 3 | 一致 |
| 参考图上限 | Console 7（`ConsoleVideoMaxReferenceImages`）、Build 8（`MaxInputImages`） | Console 7、Build 8 | 一致 |
| 参考模式约束 | 必须有 `prompt`；`resolution` ≤ 720p；参考时长上限 10s | 同 | 一致 |
| 生成不接受 `video`；编辑/延长必须 `prompt` + `video` | 是 | 同 | 一致 |

### 语音

| 规则 | 上游 | 当前 | 结论 |
|---|---|---|---|
| TTS 文本 | 非空、≤ 15000 字符 | 同 | 一致 |
| TTS `language` | 必填 | 同 | 一致 |
| TTS `speed` | 0.25–4.0 | 同 | 一致 |
| `optimize_streaming_latency` | 0–4 整数 | 同 | 一致 |
| **`output_format`** | 规范化为 `{codec, sample_rate?, bit_rate?}`；缺 `codec` 时默认 `mp3`；全空则整体省略 | 原样透传，未知键与缺失的 `codec` 都会下发 | **本轮修复** |
| STT 字段集 / `keyterm` / `vad_threshold` | 完整 | 同 | 一致 |
| STT `sample_rate_hertz` | 仅 multipart 别名 | multipart 原样透传，JSON 不支持（上游同样不支持） | 一致 |
| OpenAI 兼容层拒绝语义 | `prompt` / 非零 `temperature` / `timestamp_granularities` | 同（含"零值放行"判断） | 一致 |

## 五、改动文件

| 文件 | 改动 |
|---|---|
| `internal/grok/types.go` | 新增 `ReasoningSummary` 字段与解析；`ImagesGenerationsRequest` 缺省 `n` 归一为 1 |
| `internal/grok/responses_normalize.go` | 新增 `chatReasoningControls`；无条件补 `include`；Build summary 默认改为 `concise` 且不覆盖调用方值；`validatePayloadReasoning` 校验 summary |
| `internal/grok/handler_voice.go` | `output_format` 重建为规范形状（缺 `codec` 默认 `mp3`、丢弃未知键、全空则省略） |
| `internal/grok/reasoning_diagnostics_test.go` | 更新 `TestBuildChatSummaryBoundary` 以反映新契约；新增 `TestChatReasoningSummaryIsClientOwned`、`TestChatAlwaysRequestsEncryptedReasoning` |
| `internal/grok/compat_parity_test.go` | 新增 `TestImagesGenerationsRequest_OmittedNDefaultsToOne` |
| `internal/grok/handler_voice_test.go` | 新增 `TestValidateTTSRequestNormalizesOutputFormat` |
| `web/static/js/grok-tools.js` | 固定推理模型不下发 effort；非 none 档下发 `reasoning_summary=auto` |
| `web/grok_tools.test.cjs` | 新增 3 项断言/用例 |
| `web/static/js/grok-tools.min.js` | 重新生成（页面实际加载的是该文件） |
| `web/templates/pages/grok-tools.html` | 缓存版本号 `20260910-1` → `20260912-1` |
| `scripts/minify-grok-tools.sh` | 修复 Git Bash 下 `pwd` 返回 `/d/...` 被 Windows node 误解析为 `D:\d\...` 导致的失败 |
| `internal/handler/codex_models.go` | 新增：Codex 模型目录（`client_version` 触发，上下文窗口 / 输入模态 / 推理档位 / ETag） |
| `internal/handler/codex_models_test.go` | 新增 4 项测试：上下文窗口与模态、推理档位、媒体可见性、ETag 304 |
| `internal/handler/models.go` | `HandleModels` 在带 `client_version` 时改返回 Codex 目录 |
| `internal/modelpolicy/reasoning.go` | 新增：共享的每模型推理档位表与 `SupportsReasoningEffort` / `IsGrokComposerModel` |
| `internal/modelpolicy/reasoning_test.go` | 新增：档位、provider 前缀剥离、Console 固定推理、Composer 识别 |
| `internal/grok/handler_responses_store.go` | 原生 Responses 直通路径增加推理解码失败恢复（清理回放 → 去密文重试 → 回传恢复阶段头） |
| `internal/grok/chat_reasoning_recovery.go` | compaction 保护收窄为 `preservesClientCompaction` |
| `internal/grok/console.go` | 恢复触发条件改用同一 `preservesClientCompaction` 判断 |
| `internal/grok/session_state.go` | 新增 `isCompactionBlobDecodeError` / `preservesClientCompaction` |
| `internal/grok/responses_normalize.go` | 新增 Build/Console 两套 effort 归一化并在 payload 构建后调用 |
| `internal/grok/relay_policy_test.go` | 更新既有断言；新增 `TestRelayBuildEffortAliasesFollowModelContract`、`TestRelayNativeResponsesRecoversOpaqueReasoning` |
| `internal/grok/restrictions_regression_test.go` | 更新线上 effort 期望值（校验仍不改写调用方原值） |
| `internal/grok/reasoning_replay_items.go` | 新增：条目提取 / 归一化 / 过滤 / 插入位点（对齐上游 reasoningreplay 包） |
| `internal/grok/reasoning_replay_capture.go` | 新增：从 JSON 或 SSE 完整响应捕获条目；无锚定时清除状态 |
| `internal/grok/reasoning_replay_items_test.go` | 新增 7 项测试：锚定要求、去重、`toolu_` 前缀匹配、插入位点、旧数据兼容、捕获与清除 |
| `internal/grok/session_state.go` | 回放条目改为列表；新增 `loadReasoningReplayItems` / `storeReasoningReplayItems`，保留单密文入口 |
| `internal/grok/console.go` / `console_stream.go` | 捕获改为条目列表（流式在 `response.output_item.done` 累积） |
| `internal/grok/handler_responses_store.go` | 原生路径捕获改为条目列表 |
| `internal/store/store.go` | `StoredReasoningReplay` 增加 `Items`，保留旧字段以兼容已存数据 |
| `web/static/js/grok-tools.js` | 页面改为直发 `/grok/v1/responses`：`buildResponsesPayload` + Responses 事件解析 |
| `web/grok_tools.test.cjs` | 流式与载荷用例全部改为 Responses 协议，新增采样参数用例（16 项） |
| `docs/grok2api-parity-checklist.md` | 基线由 `62d2775`（2026-08-25）更正为 v3.1.5 `8913b53`，并链到本报告 |

## 六、验证

- `go build ./...` 通过。
- `go test ./...` 全部通过。
- `go test ./internal/grok/` 全绿。
- `node --test web/grok_tools.test.cjs`：**16/16 通过**（含新增 4 项载荷用例）。
- `scripts/minify-grok-tools.sh` 重新生成 `grok-tools.min.js`，并确认产物包含新逻辑（`reasoning_summary`、固定模型分支）。
- 未做端到端线上验证：本轮未调用真实上游，图片/视频/语音为代码级规则比对，未实际发请求回归。
- 恢复逻辑的测试均为故障注入式（测试服务器固定返回 400），未验证真实上游恢复成功率。

## 七、OpenClaw / Codex 类客户端适配复核（本轮补充）

针对"Grok 是否适合作为 OpenClaw 主力模型"提出的 6 点，逐条对照上游 v3.1.5 与当前实现：

| # | 说法 | 结论 | 依据 |
|---|---|---|---|
| 1 | 只能走 openai-completions、关了 developer role、关了 streaming usage | **不是网关限制，两侧都支持** | 上游与当前都同时暴露 `/v1/responses`、`/v1/messages`、`/v1/chat/completions`；都把 `developer` 角色纳入处理（当前 `responses_normalize.go` 将 system/developer 映射为 instructions，`util.go` 允许该角色）；当前流式响应也回传 `usage`（`console_stream.go`、`responses_stream.go`）。属于客户端侧配置选择。 |
| 2 | 会话钉死后没有退路（fallback 是自己） | **客户端配置问题** | 模型钉死与 fallback 由客户端配置决定；网关两侧都只做账号级重试与切换。 |
| 3 | 只有文本（`input: text`） | **当前确有差距，本轮补齐** | 网关两侧都接受图片输入；差别在"模型表"——上游在 `?client_version=` 时返回 Codex 目录并带 `input_modalities: ["text","image"]`，当前此前不暴露任何模态信息。 |
| 4 | maxTokens 8192、缓存 0% | **当前确有差距，本轮补齐** | 上游 Codex 目录带 `context_window`（grok-4.6 = 500k）；当前此前不暴露，客户端只能退回自身默认值。缓存方面两侧都支持 `prompt_cache_key` 并回传 `cached_tokens`。 |
| 5 | 场景匹配错位 | 非代码问题 | 属模型选型判断，网关不涉及。 |
| 6 | 历史钉残留（钉到已删除模型） | **客户端配置问题** | 网关只按请求中的 model 解析；模型不存在时返回 400，不会自行产生死钉。 |

**结论**：6 点中 3 点（#2/#5/#6）与网关无关，#1 两侧都支持、属客户端配置，只有 **#3/#4 是当前相对上游的真实差距**。

### 本轮补齐：Codex 模型目录

上游 `GET /v1/models?client_version=...` 会返回一套 Codex 目录（`backend/internal/transport/http/inference/codex_models.go`），包含 `context_window`、`max_context_window`、`effective_context_window_percent`、`input_modalities`、`supported_reasoning_levels`、`truncation_policy`、`apply_patch_tool_type` 等字段，并带 ETag 协商缓存。当前此前完全缺失，客户端因此只能按自身默认（纯文本、约 8k 上下文）处理——这正是 #3/#4 的根因。

已按上游实现补齐（新增 `internal/handler/codex_models.go`）：

- **触发条件**：`GET /v1/models?client_version=...`；不带该参数时仍返回原有 OpenAI 列表，行为不变。
- **上下文窗口与模态**：与上游逐项一致 —— grok-4.5/4.6 = 500k（含视觉）、grok-4.3 = 1M、grok-4.20 系列 = 2M、grok-build-0.1 = 256k、grok-3-mini(-fast) = 131072、grok-composer-2.5-fast = 200k，未知模型 128k。
- **推理档位**：grok-4.6 / multi-agent 到 `xhigh`，grok-4.5 到 `high`，composer / build-0.1 / non-reasoning 仅 `none`，未知模型仅 `none`；`console/grok-4.20-0309-reasoning` 为固定推理、不暴露可配置档位（对应上游的 provider 级覆盖）。
- **可见性**：图片 / 图片编辑 / 视频模型 `visibility: "hide"`；仅 Responses 文本模型开放 `apply_patch_tool_type` 与并行工具调用。
- **缓存**：ETag + `If-None-Match` 返回 304。

