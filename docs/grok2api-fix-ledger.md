# Grok 通道修复台账（对应 `docs/grok2api-parity-audit.md`）

本轮按审计报告逐条处理。原则：

1. **只做能自证正确的修复**——每条改动都通过 `go build ./...`、`go vet ./...`、`go test ./...`（全量）；新增回归测试见 `internal/grok/security_guard_test.go`、`fix_regression_test.go`、`egress/redact_test.go`。
2. **不破坏本项目已测试的契约**——有两处与 grok2api 的差异是项目有意为之（Responses 原生中继字节透明），已在下方"保留差异"中说明理由，不做静默改写。
3. **不做破坏性变更**——公开模型 ID 重命名、媒体端点线格式替换等需要产品决策的条目留在"未修"清单。

## 一、已修复（40 条）

### 安全（5）

| 编号 | 修复 | 位置 |
| --- | --- | --- |
| A5-22 | 客户端 URL 的服务端抓取加 SSRF 防护：新增 `outbound_guard.go`，强制 http/https、禁 userinfo、解析并逐个校验公网地址（拒 loopback/私网/CGNAT/链路本地/组播/保留段）、直连时把连接钉在已校验 IP（防 DNS rebinding）、重定向最多 3 跳且每跳复检；经代理时至少做预检 | `internal/grok/outbound_guard.go`、`util.go` |
| A7-1 | 出口节点校验失败不再打印带凭据的代理 URL，复用同包 `sanitizeFlareSolverrMessage` | `internal/grok/egress/node.go` |
| A5-13 | `vidgen.x.ai` / `imagine-public.x.ai` / `imgen.x.ai` 及其子域的资产下载改为匿名，不再外发 SSO Cookie（`assets.grok.com` 仍带鉴权，因其现有测试证明需要） | `internal/grok/client.go` |
| A5-10 | `detectPublicBaseURL` 对 host 做严格字符校验（拒路径/查询/userinfo/空白/控制字符），协议只接受 http/https；并确认可信代理中间件默认清除 `X-Forwarded-*`（见"审计更正"） | `internal/grok/handler_chat.go` |
| A5-6 | 请求体 `nsfw:true` 不再能单独开启上游 `enable_nsfw`，必须服务端 `PublicImagineNSFW` 允许 | `internal/grok/handler_images.go` |

### 错误契约（9）

| 编号 | 修复 | 位置 |
| --- | --- | --- |
| A8-2 | 新增统一 OpenAI 错误对象 `writeGrokError`/`writeGrokErrorCode`（`{error:{message,type,code,param}}`），把 Grok 通道 166 处 `http.Error`（纯文本）全部替换；错误码按状态派生 | `internal/grok/http_helpers.go` + 18 个文件 |
| A8-1 / A5-35 | 新增 `writeGrokUpstreamError`：上游错误的正文、节点 ID、内部 `status=/body=` 形态只进日志与诊断，客户端只收到分类化文案；本地校验错误保留原文与 400 | `internal/grok/http_helpers.go` |
| A8-3（Grok 范围） | 凭据类上游失败（auth/auth_blocked/configuration）对客户端统一 503，不再回 401/403 | 同上 |
| A8-4 | 选号失败改用 `writeGrokNoAccountError`：稳定 503 + `upstream_unavailable` | 同上 |
| A8-9 | 429/503 回写 `Retry-After`（从上游响应头或限流元数据解析） | 同上 |
| A8-7 | 所有 JSON 入口加 `http.MaxBytesReader`（32 MiB）：`decodeJSONBody` + 新增 `readBoundedJSONBody`（覆盖原生 `/responses` 与桥接） | `http_helpers.go`、`handler_responses.go`、`responses_channel_bridge.go` |
| A8-10 | 非 `application/json` 的 JSON 端点返回 415 | `decodeJSONBody` |
| A8-13 | 响应头补 `X-Request-ID`（与 `X-Trace-ID` 同值） | `internal/middleware/trace.go` |
| A8-16 | SSE 错误帧补顶层 `"type":"error"` | `http_helpers.go` |

### Chat Completions / Anthropic Messages（10）

| 编号 | 修复 | 位置 |
| --- | --- | --- |
| A1-1 | `tool`/`assistant` 角色接受 `input_text`/`input_image`/`input_file`（Anthropic `tool_result` 数组不再硬 400），并补齐三类校验分支 | `util_messages.go` |
| A1-2 | `userContentTypes` 增加 `input_text`/`input_image`/`input_file`（`document` 块不再 400） | `util.go` |
| A1-3 | `tool_choice` 名字解析纳入 `ResponsesTools`（托管 `web_search` 可被引用），`tool_choice:"required"` 同样计数 | `types.go` |
| A1-4 | `tools` 接受 `web_search`/`web_search_preview*`/`x_search` 原生工具（`ToolDef.Raw` 保留全部字段），`web_search_options` 降级为原生 `web_search`；非 function 工具不再被静默丢弃 | `types.go`、`console.go`、`responses_normalize.go` |
| A1-5 | Anthropic thinking 请求下发 `reasoning.summary:"detailed"`（否则 thinking 块只有签名） | `handler_messages.go` |
| A1-6 | `thinking.type="disabled"` 优先于 `output_config.effort` | `handler_messages.go` |
| A1-12 | 剥离每请求变化的 `x-anthropic-billing-header`，恢复上游 prompt cache 前缀 | `handler_messages.go` |
| A1-16 | 同时给出时 `max_completion_tokens` 覆盖 `max_tokens` | `types.go` |
| A1-17 | `metadata`/`service_tier` 透传；`parallel_tool_calls` 不再要求存在 tools | `types.go`、`responses_normalize.go` |
| A1-20 | `messages[]` 内 `role=system/developer` 归并进 instructions，不再 400 | `handler_messages.go` |

### Responses / Build（6）

| 编号 | 修复 | 位置 |
| --- | --- | --- |
| A2-1 | 新增 doom-loop 守卫 `streamRepeatTracker`（内容/推理连续重复上限），接入原生 Build Responses 流；阈值高于 grok2api（1024/2048，见"保留差异"），仍能在秒级终止失控生成 | `stream_loop.go`、`handler_responses_store.go` |
| A2-3 | 过滤上游私有控制事件 `response.doom_loop_check`（SSE 事件名与 payload type 两种形态） | `grok2api_sse.go`、`handler_responses_store.go` |
| A2-7 | 合成的 `response.failed` 信封补 `created_at`/`completed_at`/`output` | `handler_responses_store.go` |
| A3-2 | `apply_patch` 还原：模拟函数声明改为结构化 `operation`，响应侧解码成 `operation` 并删除 `arguments` | `responses_normalize.go`、`responses_alias.go` |
| A3-3 | `custom`（freeform）工具模拟：声明降级为 `input:string` 的 function，响应侧还原 `custom_tool_call` + `input`，流式参数缓冲与 `tool_search` 同路径 | 同上 |
| A3-5（部分） | 托管搜索工具剥离 Build 契约拒绝的字段（`external_web_access`/`search_context_size`/`max_search_results` 等 7 个）；`custom_tool_call`/`apply_patch_call` 及其 output 的历史项降级为 function_call 形态 | `responses_normalize.go` |

### 媒体 / 音频（4）

| 编号 | 修复 | 位置 |
| --- | --- | --- |
| A5-9 | 媒体/音频全部 18 条路由同时注册到统一 `/v1` 前缀（与 README 宣称一致），标准视频响应里的 `/v1/videos/{id}/content` 自链接因此可达 | `cmd/server/routes.go` |
| A5-27 | 随之修复：管理端缓存 `view_url`/`preview_url` 的 `/v1/files/...` 不再是死链 | `admin_cache.go`（无需改动） |
| A5-24 | 视频回调上传先校验 Content-Type，再消费一次性票据；读取/落盘失败时把票据归还（内存与 Redis 两条路径都还原） | `handler_video_upload.go` |
| A5-25 | 回调上传上限 512 MiB→256 MiB，超限返回 413 `request_too_large` | 同上 |

### 账号 / 配额 / 用量（6）

| 编号 | 修复 | 位置 |
| --- | --- | --- |
| A6-5 | 免费额度"按模型"耗尽（`... free usage for model X`）只冷却该模型（复用此前无写入方的 `store.RecordModelCooldown`），不再整号停 24h | `internal/grok/cli.go` |
| A6-3（部分） | 模型级 403（`access to the chat endpoint is denied` 等）只冷却该模型 5 分钟，不再整号停 10 分钟 | `internal/grok/handler.go` |
| A7-11 | 出口节点失败指标修正：首次降级也计数（原来只在已降级节点再次失败时自增） | `egress/manager.go` |
| A9-11 | 图片 usage 不再编造成本：`file` 块不再按 256 image token 计价；图片生成的 completion token 由 `n×64` 改为 0（上游本不以 token 计量） | `usage_estimate.go` |
| A9-9 | 工具调用 completion 估算只算 name+arguments，不再把 JSON 结构（键名/引号/逗号）计入 | `usage_estimate.go` |
| A9-10（部分） | 同上范围内的结构性开销不再进入 prompt 估算（`file` 块部分） | `usage_estimate.go` |

### 附带修正（审计报告本身）

- **A5-10 降级为 P2 并更正描述**：本项目已有 `TrustedProxyMiddleware`（`internal/middleware/trusted_proxy.go`），`trusted_proxies` 为空（默认）时**清除全部 `X-Forwarded-*`**，配置后也只接受来自受信代理的值并取最后一段。因此"任意客户端注入 `X-Forwarded-Host`"在默认配置下不可达；真实缺陷只剩"host 值未做字符校验"（本轮已修）。审计报告第 5 章已同步更新。

## 二、保留差异（有意不改为与 grok2api 一致）

| 编号 | 为什么不改 |
| --- | --- |
| A1-11 / A3-8 / A3-9 | 本项目的原生 Build Responses 中继**故意字节透明**，并有测试锁定：`relay_policy_test.go` 断言客户端 payload 除 `prompt_cache_key` 外原样到达上游、且重复 delta 不被抑制。强行注入 `store:false` / `include` 会破坏该契约。若产品决定改为 grok2api 语义，需要同时改这两条测试与文档，属于产品决策。 |
| A2-1（阈值） | 采用 1024/2048 而非 grok2api 的 128/256：`TestRelayRepeatedResponsesDeltasArePreserved` 明确要求 300 次合法重复必须原样传递。当前值仍能终止真正的死循环。 |
| A8-12 | `inference_auth_enabled=false` 时 `/v1` 全部匿名，是部署方显式开关。改为强制鉴权会改变现有部署行为，留给产品决策。 |
| A4-1/A4-2/A4-3 | 公开模型 ID 去前缀化、effort 后缀别名、注册式兼容别名是**破坏性变更**（会改变现有客户端可用的模型名），且 A 的测试刻意让裸 `grok-4.3` 不可解析。需要版本化迁移方案。 |

## 三、未修（需要整块移植或产品决策）

第四轮结束后的未修条目（现为 29 条：P2×21、P3×8），以及为什么：

| 编号 | 仍未修的原因 |
| --- | --- |
| A9-1 | 计费层：价格表 + Key 额度预留/结算 + `usage_source` 列。属**新功能**，不是修 bug；本轮只补齐了用量口径与 unpriced/priced 维度 |
| A3-7 | 网关侧 compaction（`compaction_trigger` 分类、canonical 摘要、`g2a_compact_v1` blob 编解码与展开），需要按上游 `responses_compaction*.go` 整块移植 |
| A6-1 | **部分完成**：识别 + 12h 停靠 + 二次停用已落地；"扣住 dump 不发给客户端并在换号后重试"仍未做——需要流式 hold 缓冲（流式已写出的内容无法撤回）与非流式先收集再决定写回，属流式写入层重构 |
| A7-7 | **已在第四轮修复**（按 UA 大版本选择 utls ClientHello，并纳入缓存键） |
| A7-8 | Web chat 主路径走 mgw WebSocket：与出口 lease 的浏览器 TLS/UA/cookie 复用是同一件事，属出口层重构 |
| A7-10 | **已在第四轮修复**（指数冷却 + 主动探测 + 共享目录持久化 + 只读健康快照） |
| A5-29 | **已在第四轮以加法方式修复**（新增管理面 `/api/media/inputs`，原推理面端点保持兼容） |
| A7-2 | statsig 的**签名**需要外部签名服务（上游由首页 metaContent + 签名器生成），本项目没有该依赖：已改为"配置有效则沿用、否则安全省略"，不再伪造 |
| A2-6 | 原生 Responses 流的 `[DONE]`/重新分帧：项目的 relay 测试明确要求保留该行为（字节透明契约），见"保留差异" |
| A5-42 | 无 `[]` 的 `timestamp_granularities`：本项目明确 400 而不是静默忽略（宁可报错也不静默丢选项），属有意保留 |
| 其余 P2/P3 | 多为文案/字段集/边角校验差异（如流式图片事件 `size` 恒 auto、`/tts/voices` 未归一化、无 `[]` 的 `timestamp_granularities` 语义等），逐条列在 `docs/grok2api-parity-audit.md` 对应章节 |

按批次给出后续方案，工作量从大到小：

1. **A9-1 计费层缺失**（P0）：需要移植价格表 + Key 额度预留/结算 + `usage_source` 列。属新功能，不是修 bug。
2. **A3-1/A3-4/A3-6/A3-7 Responses 兼容层**：事件字段补齐、20+ 种 input 历史项归一化、会话种子识别（Claude Code/Codex 头）、网关侧 compaction（`g2a_compact_v1`）。建议按 `cli/responses_compat.go` / `responses_history.go` / `prompt_cache.go` / `responses_compaction*.go` 逐文件移植，每块独立可测。
3. **A2-2 下游写错误与写超时**：需要把写错误沿 `writeSSEBytes`/`writeSSELog` 全链路返回，并引入写截止时间；改动面大，建议单独一次重构。
4. **A6-2/A6-4/A6-12/A6-13 账号策略**：401 永久出池、429 冷却上限与指数退避、按到期时间调度刷新、失效凭据自动清理。需要引入"需要重新登录"的终态与 auto-clean 任务。
5. **A7-* 出口身份**：Console 通道走代理池、per-credential 亲和、statsig 签名与失效、流内反爬闭环、节点健康探测。属出口层重构。
6. **A5-* 媒体契约差异**：`/images/edits` JSON 形态、媒体读取端点（`/v1/media/{kind}/:id` + HEAD/ETag）、媒体输入管理端契约、上传 TTL、TTS/STT 头白名单与账号级重试。多为对外契约变更，需与客户端一起排期。
7. **A3-13 参数整型规范化**、**A4-5/A4-6 Console 每模型语义**、**A1-7/A1-8/A1-10 Anthropic usage/refusal/id 语义**：独立可做，属下一批。← **已在第二轮修复，见第五节**

## 四、验证

- `go build ./...` 通过
- `go vet ./...` 通过
- `go test ./...`（全量）通过
- 新增测试：SSRF 防护（含元数据地址、loopback、私网、userinfo）、日志脱敏、CDN 匿名下载、nsfw 门控、错误对象形状与状态映射、上游正文不泄漏、doom-loop 阈值与合法重复、私有控制事件、模型级 403/免费额度识别
- 改动规模：36 个文件，+1148/−266

## 五、第二轮修复（19 条）

### Anthropic Messages 语义（5）

| 编号 | 修复 | 位置 |
| --- | --- | --- |
| A1-7 / A9-4 | `anthropicUsageFromOpenAI` 补 `cache_creation_input_tokens`、`output_tokens_details.thinking_tokens`（无条件输出，0 表示"无缓存"而非"字段缺失"），并透传 `cost_in_usd_ticks`/`num_sources_used` | `handler_messages.go` |
| A1-8 | refusal 映射为 Anthropic `stop_reason="refusal"`：`content_filter`/`refusal` finish_reason 直接映射；上游只报 `stop` 但消息含 refusal 时，流式与非流式都改写为 `refusal` | `handler_messages.go` |
| A1-10 | Anthropic 消息 id 归一：内部中继的 `chatcmpl_*` 改写为 `msg_*`，其余上游 id 原样保留（客户端前缀校验不再失败）；`message_start` 补 `created_at` | `handler_messages.go` |
| A2-10 | `message_start` 的 usage 采用完整 Anthropic 形状（cache/thinking 字段齐备），不再只有两个零值 | `handler_messages.go` |
| A1-13 | `writeAnthropicUpstreamError` 不再把上游正文塞进 `error.message`，改走 `apperrors.PublicMessage` 分类文案 | `handler_messages.go` |

### Responses / Build（3）

| 编号 | 修复 | 位置 |
| --- | --- | --- |
| A3-13 | 新增 schema 驱动的参数规范化（`UseNumber` + 按 `$ref`/`allOf`/`anyOf`/`oneOf`/`properties`/`items`/`prefixItems` 递归），把 `60000.0`/`1e3` 这类整数字面量修成 `60000`/`1000`；接入流式 `function_call_arguments.done`、`output_item.done` 与非流式 JSON 路径 | `responses_arguments.go`、`responses_alias.go` |
| A2-5 | idle 超时独立分类：导出 `ErrGrokSemanticIdle`，Responses 返回 `upstream_stream_idle_timeout`，Chat SSE 返回专用 code，不再混入 `stream_read_error`/"parse error" | `grok2api_streamidle.go`、`handler_responses_store.go`、`handler_chat.go` |
| A3-6 / A9-6 | 会话种子识别扩展到 `X-Claude-Code-Session-Id`、`X-Codex-*`、`X-Conversation-Id` 等 9 个头，并用"头名:值"限定种子（避免不同客户端同名 id 冲突） | `session_state.go` |

### 模型目录（2）

| 编号 | 修复 | 位置 |
| --- | --- | --- |
| A4-5 | Console 平面按模型注入 `max_output_tokens`（grok-4.3/4.5/4.20-multi-agent 1_000_000，grok-build-0.1 256_000） | `responses_normalize.go` |
| A4-6 | Console 平面按模型处理 reasoning：非推理模型整段删除、固定推理模型只删 `effort`、可配模型缺省注入 `medium`；未知模型不臆造 | `responses_normalize.go` |

### 媒体 / 音频（6）

| 编号 | 修复 | 位置 |
| --- | --- | --- |
| A5-26 | `/files/{kind}/{name}` 支持 HEAD（存在性探测不再下载整文件）、补 `ETag`/`If-None-Match`→304、`X-Content-Type-Options: nosniff` | `handler_files.go` |
| A5-30 | `ftyp` 嗅探校验 major brand：HEIC/AVIF/MIF1 等静态图不再被判为 `video/mp4`（显式 `video/*` 声明仍优先） | `handler_media_inputs.go` |
| A5-32 | 语音响应头白名单：非音视频/JSON 的 `Content-Type` 不透传，`Content-Disposition` 只保留 disposition 与安全的 ASCII 文件名（去路径分隔符/非 ASCII），补 `nosniff`/`no-store`，透传 `Retry-After` | `handler_voice.go` |
| A5-33 | 语音上游 401/402/403 掩蔽为 503 `upstream_unavailable`（不再让调用方以为自己的 Key 失效） | `handler_voice.go` |
| A5-28 | 媒体输入加容量上限（2 GiB，超限 507 `media_storage_full`）；媒体输入文件改用 `input-` 前缀命名空间，新增按 TTL 回收的后台清扫器（只回收该前缀文件，绝不触碰生成物/视频成品） | `handler_media_inputs.go`、`cmd/server/media_sweeper.go`、`cmd/server/main.go` |
| A5-45 | 视频内容下载改为流式（`os.Open` + `ServeContent`，支持 Range/条件 GET），不再整段读入内存；补 `Content-Disposition`/`nosniff` | `handler_videos.go` |

### 错误分类加固（1）

| 编号 | 修复 | 位置 |
| --- | --- | --- |
| A8-1 补充 | 上游失败判定改为匹配完整标记（`upstream status=`）或 `grok [cli] upstream` 前缀，避免"job status=404"这类本地错误被误判为上游故障（answered 5xx + 通用文案） | `http_helpers.go` |

### 图像响应字段（2）

| 编号 | 修复 | 位置 |
| --- | --- | --- |
| A9-3 | `consoleUsage` 归一化时保留上游的 `cost_in_usd_ticks`/`num_sources_used`/`num_server_side_tools_used`/`context_details`，不再只用 token 计数重建对象（这是下游能看到成本与上下文用量的唯一来源） | `console.go` |
| A5-3 | 图像响应每条 data 补 `mime_type`（data URI 前缀 / URL 扩展名推导，缺省 image/png），`revised_prompt` 由 `null` 改为 `""`（严格反序列化的客户端不再解析失败）；流式 `image_generation.completed` 同样带 `mime_type` | `handler_image_helpers.go`、`handler_images.go` |
| A1-18 | Chat/Messages 转 Responses 的图片 part 未指定时补 `detail:"auto"`（显式值保留），请求不再依赖上游默认值 | `responses_normalize.go` |

第二轮新增回归测试（`fix_regression_test.go`、`cmd/server/media_sweeper_test.go`）：Anthropic usage/cache/thinking 字段、refusal stop reason、id 归一、Console 每模型语义、参数整型规范化（含 number 类型不动、越界不转）、idle 超时分类、agent 会话头识别与命名空间、ftyp 品牌判定、ETag 稳定性、语音响应头过滤/放行、视频内容 URL、媒体输入清扫器只回收自己命名空间的文件。

第二轮改动后 `go build ./...`、`go vet ./...`、`go test ./...` 全量通过。

## 六、第三轮修复（53 条）

本轮按功能面并行推进，仍然坚持"可自证正确、不破坏既有契约"：

### 模型目录（7）

| 编号 | 修复 | 位置 |
| --- | --- | --- |
| A4-1 / A4-2 / A4-3 | 保留既有 `console/`、`build/`、`web/` 前缀 ID 的同时，**新增**无前缀别名、历史兼容别名与 `<model>-<effort>` 档位别名（只在该模型真的支持该档位时才合成），`__models` 表同步暴露这些别名 | `internal/grok/models.go`、`internal/handler/models.go` |
| A4-4 | 模型不存在改为 404 JSON `model_not_found`（OpenAI 面）与 `not_found_error`（Anthropic 面），不再是 400 纯文本 | `handler_chat.go`、`handler_messages.go` |
| A4-7 | Codex 目录补齐 5 个协议字段（`default_service_tier`、`availability_nux`、`upgrade`、`model_messages`、`auto_compact_token_limit`） | `internal/handler/codex_models.go` |
| A4-8 | agent 工具集（`apply_patch`/并行工具）只对 Build+Responses 路由宣告，Web 模型不再被标成支持 | 同上 |
| A4-9 | 同名模型的 Build 池不可用时回退到 Console，而不是直接失败 | `handler_chat.go` |

### Responses 兼容层（2）

| 编号 | 修复 | 位置 |
| --- | --- | --- |
| A3-1 | 新增 `responses_compat.go`：只为**缺字段**的事件补齐 `response.id/object/created_at/model/output`、`item.id`、`item_id`、`output_text.annotations`，已合法的事件保持原始字节 | `responses_compat.go` |
| A3-4 | 新增 `responses_history.go`：Build 请求中有选择地归一化扩展历史项（`agent_message`、`local_shell_call(_output)`、`mcp_tool_call_output`），原生历史与未知字段原样保留 | `responses_history.go` |

### 账号策略（5）

| 编号 | 修复 | 位置 |
| --- | --- | --- |
| A6-2 | 新增持久化 `AuthStatus`：401 标记 `reauthRequired` 并**不再回到候选池**，直到重新登录/改凭据/管理员验证成功 | `store`、`accountpolicy`、`loadbalancer` |
| A6-4 | 429 冷却改为有界指数退避（30s→1m→2m→…→30m），上游 `Retry-After`/reset 一律封顶 30m；LB 与 `AccountHeld` 取两者较晚者 | 同上、`rate_limiter.go` |
| A6-6 | 付费额度耗尽（月额度 ≤0 或周用量 100%）在计费周期结束前不进候选；周期结束后**每次只放行一个原子探针**（15 分钟间隔） | `store`、`loadbalancer` |
| A6-7 | 团队/模型 429 不再在请求内阻塞等待，改为立即返回带 `Retry-After` 的 429，由重试循环换号 | `rate_limiter.go`、`handler.go` |
| A6-15 | 成功请求在缺少权威配额头时对已观测到的本地窗口做原子扣减（每周百分比账单不动） | `store.ConsumeGrokQuota` |

### 用量与计费口径（5，不含 A9-1）

| 编号 | 修复 | 位置 |
| --- | --- | --- |
| A9-2 | journal 增加 `usage_source`（`upstream`/`estimated`/`none`）与 `total_tokens`，旧 Redis Stream 记录无需迁移 | `internal/audit`、`console.go`、`handler.go` |
| A9-5 | Anthropic 流式 `message_start` 用请求侧估算填充 `input_tokens` 与缓存计数器，终态仍以上游 usage 覆盖 | `handler_messages.go` |
| A9-7 | 见 A6-15：成功请求乐观扣减已观测窗口，缺失时绝不臆造额度 | `handler.go`、`quota.go` |
| A9-8 | billing 百分比缺失时用 `monthly_used/monthly_limit` 反推；展示优先级改为月绝对额度优先于周百分比 | `cli_billing.go`、`quota_projection.go` |
| A9-12 | 运营聚合补 cached/reasoning/total 与 priced/unpriced 维度（当前用量统一记未计价，因为 A9-1 未实现） | `internal/opsagg`、`middleware` |

### 流式生命周期（6）

| 编号 | 修复 | 位置 |
| --- | --- | --- |
| A2-2 | `writeSSEBytes` 返回并传播短写/写错误；可用时用 `ResponseController` 设置 30s 写截止；下游失败即中止上游解析 | `http_helpers.go`、`console_stream.go`、`handler_responses_store.go` |
| A2-4 | 语义 idle 只用于 Build，Web/Console 改用字节级 idle；三通道都覆盖成功的 2xx body | `request_helpers.go`、`client.go` |
| A2-8 | Chat 中途错误帧改为 data-only OpenAI 信封（`type=api_error`） | `http_helpers.go` |
| A2-9 | SSE 头补 `charset=utf-8` 与 `X-Accel-Buffering: no`，不再写 hop-by-hop `Connection`；桥接复制内层已提交头 | 同上 |
| A2-11 | 恢复 `semanticIdleReadCloser.TimedOut()` | `grok2api_streamidle.go` |
| A2-12 | idle 改为按通道配置（Web 90s / Console-Build 120s，30–600s 边界，兼容旧单值配置） | `internal/config` |

### 媒体与语音（6）

| 编号 | 修复 | 位置 |
| --- | --- | --- |
| A5-1 | `/images/edits` 同时接受 multipart 与 `application/json`（`image`/`images` 的 `url` 或 `file_id`，二者互斥，复用媒体输入存储与 SSRF 防护） | `handler_image_edits.go` |
| A5-15 | `/videos` JSON 接受 grok2api 拼写 `duration`/`aspect_ratio`/`resolution`（及 `user`/`image`/`reference_images`）并折叠到规范字段 | `types.go`、`handler_videos.go` |
| A5-23 | 单请求附件数量上限 8（原来无上限，每个附件都会被下载并 base64 放大） | `handler_chat.go` |
| A5-31 | 视频任务与一次性上传票据 TTL 1h→2h（接收端仍显式判过期） | `handler_videos.go` |
| A5-34 | Responses/Grok 错误信封 `type` 按状态派生并补 `param`，不再恒为 `invalid_request_error` | `handler_responses_store.go` |
| A5-14 | 视频**创建阶段**失败（401/402/403/429/5xx/配额）改为重新入队换号重试；已拿到上游任务 ID 后的失败仍然判死，避免重复生成 | `handler_videos_console.go` |

### 出口与身份（12）

| 编号 | 修复 |
| --- | --- |
| A7-2 | 不再本地伪造 `x-statsig-id`：配置里有效则沿用，否则安全省略（无外部签名器时的诚实降级） |
| A7-3 | Console DPoP 取号与请求改走 `console` scope lease（带凭据亲和、UA/cookie、健康反馈与释放） |
| A7-4 | Build/CLI 不再被注入浏览器 UA 与 grok.com `cf_clearance` |
| A7-5 | 亲和改为按凭据派生（Web 用 SSO 指纹、Build 用账号身份），不再常量 |
| A7-6 | clearance 指纹纳入节点 URL/求解器/目标/亲和（凭据安全哈希） |
| A7-12 | CF cookie 白名单补 `_cfuvid`/`cf_chl_*`，去重、限长、拒控制字符 |
| A7-13 | 会话身份探测走 app_chat lease，使用专用 GET 浏览器头并支持解压 |
| A7-14 | Build 请求补 `x-authenticateresponse`/`x-grok-agent-id`/`x-grok-req-id`/`traceparent`，会话头规范为 UUID（确定性 UUIDv5） |
| A7-15 | `x-cluster` 只用于 Console `/responses` |
| A7-16 | lease 侧通用 403 也失效 clearance |
| A7-17 | Client Hints 按实际 UA 的 Chrome 版本与平台推导 |
| A7-18 | 连接池键纳入代理 URL 哈希，标准/浏览器客户端缓存封顶 512 并关闭空闲连接 |

### 错误契约与 Chat 校验（4）

| 编号 | 修复 | 位置 |
| --- | --- | --- |
| A8-5 | 限流闸门改为非阻塞准入：过载立即 503 + `Retry-After: 1` + `server_overloaded` JSON（原来是排队 60s 后纯文本 503） | `middleware/concurrency.go` |
| A8-6 | Anthropic 错误 `type`/`code` 按状态派生（401→authentication_error、429→rate_limit_error、5xx→overloaded_error 等） | `handler_messages.go` |
| A8-11 | `/videos` JSON 未知字段显式 400（本项目所用 JSON 库不执行 `DisallowUnknownFields`，改为按白名单显式校验） | `handler_videos.go` |
| A8-15 | 模型白名单 403 改用统一错误信封（含 `param`） | `http_helpers.go` |
| A1-15 | 缺 `tool_call_id` 的 tool 消息不再用 function name 顶替，而是跳过该条（避免生成不存在的 call_id） | `responses_normalize.go` |
| A1-19 | 工具序列校验：`tool_use` 必须配对 `tool_result`，未配对即报错 | `handler_messages.go` |
| A5-11 | Web 免费/基础档凭据的视频时长在发往上游前钳到 6 秒（原来上游直接拒绝整个任务，表现为几分钟后一个含糊失败） | `handler_videos.go` |
| A7-9 | 流内反爬识别：上游流内 `code=7`/"anti-bot" 不再被当成普通失败——分类为 `errGrokWebAntiBot`，账号短冷却让下次重新求解 clearance（自动重签 statsig 仍未实现，见未修清单） | `console_stream.go`、`console.go` |
| A5-12 | 标准 `/videos/generations` 不再硬绑 Console：模型路由到 Web 平面时改走 Web 任务引擎（带图片输入时仍明确报错），仅部署 Web 账号的场景不再必然 503 | `handler_videos_console.go` |

## 七、第四轮修复（5 条 + 1 条部分实现）

### 出口节点健康（2）

| 编号 | 修复 | 位置 |
| --- | --- | --- |
| A7-10 | 节点失败改为**有界指数冷却**（30s→1m→2m→…→10m 上限，成功后计数清零），不再 30s 后无条件回到坏节点；新增**主动探测**：某 scope 全部节点在冷却时，不再直接失败，而是挑选冷却最早结束的节点、在不持锁的情况下探测一次，探通即恢复（坏节点则延长冷却）；健康快照**持久化**到共享媒体目录（多副本挂同一目录即共享，重启不清零）；新增 `HealthSnapshot()`（只含节点名/健康分/失败数/冷却截止，绝不含代理 URL 或凭据） | `internal/grok/egress/manager.go` |
| A7-7 | TLS ClientHello 按 UA 声明的 Chrome 大版本选择（本 utls 版本提供 120/131/133 三档，就近向下取、UA 更新则取最新），不再固定 `HelloChrome_Auto`；连接池与缓存键纳入所选 profile，UA 与指纹不再互相矛盾 | `internal/util/browser_transport.go`、`egress/manager.go` |

### 媒体输入契约（1，加法式）

| 编号 | 修复 | 位置 |
| --- | --- | --- |
| A5-29 | **新增**管理面端点 `POST/GET/DELETE /api/media/inputs[/{id}]`（管理员会话鉴权、`{"data":{fileId,mimeType,sizeBytes,…}}` camelCase 信封与 `{error:{code,message}}`），与 grok2api 管理契约一致；原推理面前缀端点保留不变。管理面对象落在共享 `admin` 命名空间，客户端可直接引用其 `file_id`；调用方自己的命名空间仍然优先、隔离不变 | `internal/grok/handler_media_inputs.go`、`cmd/server/routes.go` |

### 质量降级防护（1，部分）

| 编号 | 修复 | 位置 |
| --- | --- | --- |
| A6-1 | 移植降级判定与处置：新增 `quality_guard.go`，在控制台流式/非流式两条路径上采集信号（是否期待推理、是否真的收到推理、可见/推理/密文字符数、推理 token 数、首次可见延迟、是否终止、工具调用数），判定"期待推理却完全没有推理"与"大额推理账单 + <2s 迟到的短文"两种 dump；首次命中把凭据**停靠 12 小时**（并入选号过滤 `accountUsableForModel`），第二次命中**停用**凭据，成功一轮清零计数 | `internal/grok/quality_guard.go`、`console.go`、`console_stream.go`、`handler.go`、`internal/store/store.go` |

**仍然缺的一半（已记入未修清单）**：流式路径无法撤回已经写出的内容，因此本实现只冷却产生 dump 的凭据，不做"扣住不发给客户端再换号重试"。要做到上游那样，需要在流开头引入 hold 缓冲（可见输出达到阈值/超时/终止三者之一才放行），属流式写入层重构；非流式路径同理需要把 `collectConsoleChat` 改为返回结果再决定是否写回。

### 复核补记（并行实现已落地、此前未逐条登记）

| 编号 | 状态 | 证据 |
| --- | --- | --- |
| A5-17 | 已修 | 官方视频请求结构体包含 `User *string`，不再因 `user` 字段报 400 |
| A5-18 | 已修 | `videoAspectRatioMap` 接受 `4:3`/`3:4`，Web 与 Console 平面一致 |
| A5-19 | 已修 | 1080p 资格判定比较 `spec.UpstreamModel`，不再用公开模型名 |
| A5-20 | 已修 | 兼容别名表覆盖带 Provider 前缀的视频模型名 |
| A5-37 | 已修 | GET `/stt` 非 Upgrade 返回 405 |
| A5-38 | 已修 | STT 不支持的 Content-Type 返回 415 |
| A5-39 | 已修 | STT multipart 接受 `sample_rate_hertz` 并归一化为 `sample_rate` |
| A5-43 | 已修 | voice 请求体上限 32 MiB（与上游一致） |
| A3-10 / A3-11（部分） | 已修 | Build 会话头规范为 UUID、补齐 trace 身份头（A7-14 一并落地）；`x-xai-request-id` 的平面归属仍与上游有差异，属 P2 余项 |

## 八、第五轮修复（7 条）

| 编号 | 修复 | 位置 |
| --- | --- | --- |
| A3-12 | 非流式原生 Build Responses 上限 8 MiB→128 MiB（流式捕获侧缓冲保持 8 MiB 有界，因其只用于用量/模型回收） | `handler_responses_store.go` |
| A5-8 | 流式图像事件的 `size` 不再恒为 `auto`：base64 载荷按图像头解析出真实 `WxH`，URL 载荷仍为 `auto`（不为此抓取远端字节） | `handler_image_helpers.go`、`handler_images.go` |
| A5-16 | 视频失败保留已记录的 `error.code`（账号/模型类失败不再被压成 `internal_error`） | `handler_videos.go` |
| A5-36 | `/tts/voices` 归一化为文档形状：每项 `voice_id`/`name`/`language`（缺失 language 显式 `null`），未知上游字段丢弃，无 id 的条目剔除；非列表载荷原样透传 | `handler_voice.go` |
| A5-44 | 语音响应转发白名单含 `Retry-After`（此前 429 的退避信息丢失） | `handler_voice.go` |
| A6-11 | 上游 5xx 施加 5 秒软隔离；`AccountHeld` 现在也尊重显式 `QuotaResetAt`（不再只对 429/402 生效），且不会因此缩短 401/403 的确定性封禁 | `handler.go`、`accountpolicy/policy.go` |
| A3-11（收尾） | Build 请求头补 `x-grok-client-version`（与既有 trace 身份头一并） | `cli.go` |

复核确认（此前发现已不成立）：**A5-44** 的 `Retry-After` 与 **A5-17/A5-18/A5-19/A5-37/A5-38/A5-39/A5-43** 已在并行实现中修复，详见上一节"复核补记"。

| A8-8 | 请求执行超时默认 600s→7200s（对齐上游 2 小时）：真正的长推理/长工具链不再被 10 分钟截断；停滞的流仍由按通道配置的 stream-idle 看门狗兜住，边界仍允许运维调低 | `internal/config/config.go` |
| A1-9 | `stop` 序列只在本地下发（控制台流/非流各有一个 stopFilter），不再同时写进上游 Responses 载荷：上游一旦自己截断，匹配到的 token 不会回来，客户端就拿不到 `stop_sequence`，而且该字段本不属于 Build/Console 线契约 | `responses_normalize.go` |
| A5-41 | 复核：转录"不支持参数"已返回 `unsupported_parameter`（此前审计基于旧版本） | `handler_voice.go` |
| A5-21 | 复核：Console DPoP 与媒体请求在 403 时都会 `lease.InvalidateClearance()` 并向出口层反馈 challenge，clearance 会重建（此前审计基于旧版本） | `dpop.go`、`client.go` |
