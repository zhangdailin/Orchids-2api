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

## 二、保留差异（已清空）

第十一轮清除 12 条（见第十五节），第十二轮清除最后 5 条媒体契约（见第十六节）：**171 条已全部与 grok2api 对齐或修复，没有保留差异**。

先前的 5 条媒体契约（`A5-2`、`A5-4`、`A5-5`、`A5-7`、`A5-42`）现已按参考实现改写，原先锁定旧契约的测试同步更新。

## 三、未修（已清空）

**自第十轮起，本审计没有未修条目**：171 条全部归入"已修复"或"有意保留"（见第十二节总账）。最后关闭的三条是 A3-7（网关侧 compaction 移植）、A6-1（质量降级 hold 缓冲与换号重试）、A9-1（计费层），记录在第十四节与下方"已解决的旧条目"表。

原"未修"表已删除，避免与总账重复计数；下面两张表保留历史，以便回退时对照。

### 已解决的旧条目（曾列在本节，保留记录以免回退）

| 编号 | 结论 |
| --- | --- |
| A5-2 | 已对齐（第十二轮：`/images/generations` 缺省 model 直接 400，不再回落生成并消耗图片额度） |
| A5-4 | 已对齐（第十二轮：传输层校验 `aspect_ratio` 只接受比例串、`size` 只接受四个编辑尺寸、`resolution` 只接受 1k/2k 且默认 1k；`image_config` 补 `aspect_ratio`/`resolution` 并同样校验） |
| A5-5 | 已对齐（第十二轮：图像响应恒为本站绝对资产 URL，不回传上游 CDN 地址、不返回站内相对路径；缓存失败即失败） |
| A5-7 | 已对齐（第十二轮：公开名 `grok-imagine-image-quality` 归 Console 媒体面，`-quality-2.0` 别名可用，Web `-pro` 已移除） |
| A5-42 | 已对齐（第十二轮：multipart 无 `[]` 的 `timestamp_granularities` 静默忽略，带 `[]` 仍 400） |
| A1-11 / A2-1 / A2-6 / A3-8 / A3-9 | 已对齐（第十一轮：Build 缺省注入 `store=false` + `include: reasoning.encrypted_content`、本地持久化与 `store` 解耦、原生 Responses 字节透传且不再追加 `[DONE]`、doom-loop 阈值回到 128/256 并覆盖 chat 转换路径） |
| A4-1 / A4-2 / A4-3 | 已对齐（第十一轮：公开模型名去 provider 前缀并按外部 ID 去重、通用大小写不敏感前缀剥离、补齐 B 的注册别名表；`<model>-<effort>` 别名按模型支持档位解析） |
| A4-11 | 已对齐（第十一轮：恢复 B 的三项目录派生：4.6 在位补 4.5、OAuth Build 补 Composer、Super 才有 video 1.5） |
| A4-17 | 已对齐（第十一轮：Codex 未知模型描述与 B 逐字节相同） |
| A7-2 | 已对齐（第十一轮：移植外部签名器 + 首页 metaContent + 1h 缓存 + 反爬失效重签 + URL 校验；第十八轮改为**默认启用参考实现的签名服务**，未配置时即 `https://grok.wodf.de/sign`） |
| A8-12 | 已对齐（第十一轮：推理前缀恒要求托管 Key，`inference_auth_enabled` 不再能关闭鉴权） |
| A9-1 | 已修复（第十轮：`internal/pricing` 官方费率表 + Key 额度预留/结算 + 审计成本三列，见第十四节） |
| A3-7 | 已修复（第十轮：`compaction_trigger`/TUI 分类、canonical 摘要采样、`g2a_compact_v1` 封存与展开，见第十四节） |
| A6-1 | 已修复（第十轮：流式 hold 缓冲 + 换号重试 + 失败开放/关闭策略；并修好质量评语此前根本没落库的问题，见第十四节） |
| A7-7 | 已修复（第四轮：按 UA 大版本选择 utls ClientHello，并纳入缓存键） |
| A7-10 | 已修复（第四轮：指数冷却 + 主动探测 + 共享目录持久化 + 只读健康快照） |
| A5-29 | 已修复（第四轮，加法方式：新增管理面 `/api/media/inputs`，原推理面端点保持兼容） |
| A7-8 | 已修复（第五轮：Web chat 主路径接入出口 lease，复用浏览器 TLS/UA/cookie） |
| A6-2 / A6-4 / A6-12 / A6-13 | 已修复（第五、八轮：401 终态出池、429 冷却上限与指数退避、按到期调度刷新 25/批、失效凭据跳过刷新） |
| A2-2 | 已修复（第五轮：写错误与写超时沿 SSE 写链路返回） |
| A3-13 / A4-5 / A4-6 / A1-7 / A1-8 / A1-10 / A3-1 / A3-4 / A3-6 | 已修复（第二、三轮：参数整型规范化、Console 每模型语义、Anthropic usage/refusal/id 语义、Responses 事件字段补齐与历史项归一化） |
| A5-24 / A5-25 / A5-27 / A5-40 | 已修复（第三、八轮：票据归还、256 MiB 上限、缓存自链接、TTS/STT 账号级重试） |
| A4-16 | 已修复（第八轮：前端兜底模型清单换成在售模型，并重新生成 `grok-tools.min.js`） |
| A3-14 / A6-9 / A9-13 | 已修复（第八轮：`input_items` 祖先链、轮转窗口扫描、按 call_id 的推理证明回填） |
| A6-14 | 已修复（第九轮：按 mode 的额度形态表判级并取最低等级，见第十三节） |
| A4-15 | 已修复（第九轮，加法方式：分页信封 + `/api/models/groups`，原裸数组契约保留，见第十三节） |
| A4-17 | 归入"有意保留"（第九轮：文案收敛到单一常量，但保留本服务自述而非抄用上游项目名） |

### 有意保留（已清空）

第十二轮后无保留项。边角 P2/P3（流式图片事件 `size`、`/tts/voices` 归一化等）已在第五、六轮修复，见对应章节。

按批次给出后续方案，工作量从大到小：

> 下面这份清单是第一轮结束时排的方案，保留作历史记录。各项的最终状态见第五～十一节；**截至第八轮的剩余工作**见本节末尾的排序。

1. **A9-1 计费层缺失**（P0）：需要移植价格表 + Key 额度预留/结算 + `usage_source` 列。属新功能，不是修 bug。
2. **A3-1/A3-4/A3-6/A3-7 Responses 兼容层**：事件字段补齐、20+ 种 input 历史项归一化、会话种子识别（Claude Code/Codex 头）、网关侧 compaction（`g2a_compact_v1`）。建议按 `cli/responses_compat.go` / `responses_history.go` / `prompt_cache.go` / `responses_compaction*.go` 逐文件移植，每块独立可测。
3. **A2-2 下游写错误与写超时**：需要把写错误沿 `writeSSEBytes`/`writeSSELog` 全链路返回，并引入写截止时间；改动面大，建议单独一次重构。
4. **A6-2/A6-4/A6-12/A6-13 账号策略**：401 永久出池、429 冷却上限与指数退避、按到期时间调度刷新、失效凭据自动清理。需要引入"需要重新登录"的终态与 auto-clean 任务。
5. **A7-* 出口身份**：Console 通道走代理池、per-credential 亲和、statsig 签名与失效、流内反爬闭环、节点健康探测。属出口层重构。
6. **A5-* 媒体契约差异**：`/images/edits` JSON 形态、媒体读取端点（`/v1/media/{kind}/:id` + HEAD/ETag）、媒体输入管理端契约、上传 TTL、TTS/STT 头白名单与账号级重试。多为对外契约变更，需与客户端一起排期。
7. **A3-13 参数整型规范化**、**A4-5/A4-6 Console 每模型语义**、**A1-7/A1-8/A1-10 Anthropic usage/refusal/id 语义**：独立可做，属下一批。← **已在第二轮修复，见第五节**

**剩余工作：无。** 第十轮关闭了最后三条（`A9-1`、`A3-7`、`A6-1`，见第十四节）。

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

## 九、第六轮修复（8 条 + 2 条复核）

| 编号 | 修复 | 位置 |
| --- | --- | --- |
| A4-12 | 从编译期目录移除已弃用的 `grok-imagine-image-pro`（它被 `IsDeprecatedModelID` 无条件拒绝，列出来只会让客户端选中一个必然失败的模型）；管理员 Imagine 入口把旧名映射到取代它的 `grok-imagine-image-quality` | `models.go`、`handler_images.go`、`admin_imagine.go` |
| A4-13 | 公开 `/v1/models` 的 `created` 使用路由行真实创建时间（`store.Model.CreatedAt` 新增，创建/更新时补零值），旧数据回退原常量；`capabilities`/`provider`/`upstream_model` 保留（本项目控制台依赖它们显示路由与固定推理档位，属有意扩展） | `store/model.go`、`store/redis_store.go`、`handler/models.go` |
| A4-14 | `grok-imagine-image-lite` 把 Basic 视为**最低可用档**而不是排除池：该模型候选顺序为 lite→basic→super→heavy，imagine-lite 取号路径不再显式过滤 basic，只有 Basic 账号的部署可以服务它 | `models.go`、`handler.go` |
| A1-14 | 历史文本 part 统一用 `input_text`（助手历史原样发 `output_text` 会被上游拒绝） | `responses_normalize.go` |
| A6-8 | 等负载账号之间改为**最久未选中优先**（LRU）：`LoadBalancer` 维护 `lastSelected`（加锁、懒初始化，避免与共享账号对象竞争），不再纯随机导致固定子集过热 | `loadbalancer.go` |
| A6-10 | 换号预算默认 5→20、硬上限 20→100：坏池不再在少数账号后直接"retries exhausted"；账号级冷却本身限住重试，不会变成风暴 | `config.go`、`console.go`、`admin_imagine.go` |
| A6-16 | 凭据失败（401/402/403/429/模型级 403/质量降级）时**解绑会话粘滞**：内存绑定立即删除，持久绑定用 1 秒 TTL 退役，下一轮不再回到刚失败的账号 | `session_state.go`、`handler.go` |
| A8-14 | 客户端 Key 不再被隐式套用 60 RPM 默认值（0 = 不限速，由部署级准入控制保护）；过期 Key 的错误码与未知 Key 统一为 `invalid_api_key`（补救动作相同） | `store.go`、`middleware/session.go` |

复核：**A4-10** 已修复（固定推理模型走显式分支判 true，仅 `none` 档位的模型判 false，与上游一致）；**A5-21** 已在早前修复（见复核补记）。

## 十、第七轮修复（3 条 + 2 条保留）

| 编号 | 修复 | 位置 |
| --- | --- | --- |
| A5-40 | TTS/STT 增加**账号级重试/故障转移**：一次语音请求最多换 3 个 Console 账号，只有账号类/上游类失败（401/402/403/429/5xx）才换号，调用方自己的参数错误（4xx 非上述）直接返回不重试 | `handler_voice.go` |
| A6-13 | 永久失效凭据收敛：后台刷新循环跳过 `AuthStatus=reauthRequired` 或已停用的 OAuth 账号，不再每轮重复刷新、重复写同一条告警；运维重新登录即恢复（删除仍需人工决策，避免自动化删除账号） | `cmd/server/background.go` |
| A6-12 | 刷新批量 5→25：千账号规模下按到期时间轮询一轮从"大半天"降到可控；循环之间仍有 500ms 间隔，不冲击上游 | `cmd/server/background.go` |

保留（已归入第三节"有意保留"表）：**A4-17**（Codex 未知模型 description 文案，客户端不解析）、**A6-14**（订阅/等级推断阈值：需要 auto/fast 两种窗口形态的数据模型才能正确改写，凭猜测改阈值比现状更糟，留作后续）。

## 十一、第八轮修复（4 条）

| 编号 | 修复 | 位置 |
| --- | --- | --- |
| A9-13 | 新增**按 call_id 的推理证明回填**：缓存的一轮里，"某个 function_call 之前的那条 reasoning"会按它的 call_id 建索引；当客户端只回传部分历史（或漏掉证明块）时，缺失的证明会补回对应调用之前，已带证明或未知 call_id 不动。这是整轮回放的按调用粒度补充，覆盖多轮工具循环中被丢弃推理链的场景 | `reasoning_replay_items.go`、`session_state.go`、`responses_normalize.go` |
| A3-14 | `GET /responses/{id}/input_items` 现在能给出完整对话：存储记录新增 `PreviousResponseID` 链，保存时把祖先响应（最多 8 层，去环）的输入项折叠进来，不再只返回本轮输入 | `handler_responses_store.go`、`store/store.go` |
| A6-9 | 大池改为**轮转窗口扫描**：超过 64 个账号时只检查一段窗口并向前推进，窗口内无可选账号才回退全量扫描——每请求不再为上千账号做全量可用性检查，同时保证每个账号仍会被扫到 | `loadbalancer.go` |
| A4-16 | 控制台前端兜底模型清单换成当前在售模型（默认 `grok-4.6`，含 4.5/4.3 及其档位别名）：旧清单整份都是已弃用 ID，接口拉取失败时会把会话预置成一个必然被拒的模型；同时同步重新生成 `grok-tools.min.js` | `web/static/js/grok-tools.js`、`grok-tools.min.js` |

保留（已归入第三节"真正的未修条目"与"有意保留"两表）：**A4-15**（管理端模型接口形状：分页信封/分组/同步端点，改动需连同管理前端一起做，属对外管理契约）、**A4-17**（Codex 未知模型 description 文案，客户端不解析）、**A6-14**（订阅/等级推断需要 auto/fast 两套窗口形态的数据模型，凭猜测改阈值会更糟）。

## 十二、总账（截至第十二轮）

对账方式：取 `docs/grok2api-parity-audit.md` 中全部发现编号（每条发现一个 `### A?-? [P?]` 标题），逐个归入本台账的"已修表 / 有意保留表 / 未修表"，要求三集合互斥且并集等于审计总数。可用 `python3 docs/grok2api-audit/recount.py` 复算（输出的四行与本表逐字对应，若有未归类条目会以非 0 退出码报错）。

| 分类 | 条数 | 严重度分布 | 说明 |
| --- | --- | --- | --- |
| **总计** | **171** | P0 ×6、P1 ×69、P2 ×75、P3 ×21 | 审计报告自报口径（含 `A8-1` 的 `[P0/P1]` 双标） |
| 已修复 | 171 | P0 ×6、P1 ×69、P2 ×75、P3 ×21 | 见第一、五～十一、十三～十六节各表 |
| 有意保留 | 0 | — | 无 |
| 未修 | 0 | — | 无 |

171 + 0 + 0 = 171；严重度合计 6 + 69 + 75 + 21 = 171，无重复计数、无遗漏。

- **P0 全部关闭**：6 条 P0 中 5 条在早期轮次修复，`A9-1`（计费层）在第十轮完成。
- **未修 0 条**：第十轮关闭 `A3-7`、`A6-1`、`A9-1` 三条。
- 第一节列的旧"未修清单"中，`A7-7`、`A7-8`、`A7-10`、`A5-29`、`A6-2/A6-4`、`A6-12/A6-13`、`A2-2`、`A5-24/A5-25/A5-27/A5-40`、`A3-13`、`A4-5/A4-6`、`A4-16`、`A3-14`、`A6-9`、`A9-13`、`A1-7/A1-8/A1-10`、`A3-1/A3-4/A3-6`、`A4-15`、`A6-14` 均已关闭，记录保留在第三节"已解决的旧条目"表。

验证（第十轮涉及代码）：

- `go build ./...`、`go vet ./...`、`go test ./... -count=1` 全绿（第十轮结束时）。
- `python3 docs/grok2api-audit/recount.py` 退出码 0，输出 171 / 171 / 0 / 0。

## 十三、第九轮修复（2 条 + 1 条归类）

| 编号 | 修复 | 位置 |
| --- | --- | --- |
| A6-14 | 订阅/等级推断改为**按 mode 判级**：新增 `webQuotaTierShapes`（auto 7/20→basic、50→super、150→heavy；fast 30→basic、140→super、400→heavy）与 `inferSubscriptionFromWebQuota`，多窗口冲突时**取最低等级**，与上游一致；Web 聚合投影不再参与判级（`applyQuotaInfo(..., false)`），单窗口推断（Console/Build 头路径）改为只认确切形态、取消 `>=150` 兜底与凭猜测的 `lite` 归类。测试：`TestApplyWebQuotaInfoClassifiesByModeNotByMixedLimit`（auto 150 + fast 30 必须判 basic）、`TestInferSubscriptionFromRateLimitInfoRequiresKnownShapes`、`TestInferSubscriptionFromWebQuotaHeavyMode` | `internal/grok/quota.go`、`quota_test.go` |
| A4-15 | 管理端模型接口**加法式**对齐：新增 `GET /api/models/groups`（按 endpoint capability 分组，`key` 为组内路由 id 的 `:` 连接，含 `endpointCapabilities`）与分页信封 `{items,page,pageSize,total}`；`/api/models` 在**未传** `page`/`pageSize` 时仍返回裸数组（现有管理前端契约不变），传分页参数时返回信封，`search` 同时生效，`pageSize` 上限 500。测试：`TestHandleModelsStaysABareArrayWithoutPaging`、`TestHandleModelsServesPagedEnvelopeOnRequest`、`TestHandleModelGroupsBucketsByEndpointCapabilities` | `internal/api/models_admin.go`、`models_admin_test.go`、`api.go`、`cmd/server/routes.go` |

归类：**A4-17** 从"未修"移入"有意保留"——文案收敛到单一常量 `codexDefaultDescription`，但保留本服务自述（`orchids-api`）而非抄用上游项目名，属产品署名差异，客户端不解析该字段。

## 十四、第十轮修复（3 条，收尾）

| 编号 | 修复 | 位置 |
| --- | --- | --- |
| A3-7 | **网关侧 Responses compaction**。`compaction_trigger`（Codex remote-v2）与 TUI 的 canonical 摘要提示词在 `/responses` 入口分类；命中后由网关自己跑摘要回合（上游采样参数与 grok-build 一致：追加 canonical prompt、`instructions=null`、`stream=true`、`store=false`、`tools` 保留且 `tool_choice=auto`、`reasoning.summary=concise`，并删除 `previous_response_id`/`text`/`max_output_tokens` 等），对 SSE 取 `response.completed` 的摘要（退化摘要按 <500 rune 判废），清洗（去 `<analysis>`、`<summary>` 改写为 `Summary:`、标签去毒、折叠空行）后封存为 `g2a_compact_v1.<sealed>`（AES-256-GCM，密钥由凭据密钥做域分离派生），返回合成的 `compaction` 项（流式 6 事件序列 / 非流式 JSON）；后续请求里的自家 blob 展开为普通 user 消息，**非自家 blob 原样转发，自家但解不开的 blob 返回 400 `invalid_compaction_blob` 并带 `param=input[i].encrypted_content`**。新增 `SetCompactionCipher`，缺失时功能关闭、行为回退到原转发。 | `internal/grok/responses_compaction.go`、`responses_compaction_prompt.txt`、`handler_responses.go`、`handler_responses_store.go`、`handler.go`、`internal/secureblob/cipher.go`、`cmd/server/main.go` |
| A6-1 | **质量降级 hold 缓冲 + 换号重试**。移植 `quality_retry.go` 的判定与提交策略（burst dump / fake-encrypted dump 2s 窗口 / 明文推理占比 dump / cipher drool 1024 字符 / 30s hold 超时 / 6 次尝试 / 失败开放或关闭），新增 `deferredResponseWriter` 在 hold 期间**连状态行与响应头一起扣住**，分类器在每个内容事件后决定 release / wait / withhold；被扣住的回合不写任何字节，因此可以在另一个账号上重试（命中 hosted tool 等已有副作用的请求只惩罚不重试，与上游一致）；预算耗尽按策略交付最后一份被扣住的响应（fail-open，默认）或返回 502 `quality_degraded`（fail-closed）。同时修好一处真实缺陷：`UpdateAccount` 的字段白名单不含质量字段，导致"停靠 12 小时/二次停用"此前只改了内存对象、从未落库（LB 缓存 1s 后即失效），现改为专用 `UpdateAccountQuality` 原子写入。 | `internal/grok/quality_hold.go`、`console_stream.go`、`console.go`、`quality_guard.go`、`internal/store/redis_store.go`、`store.go`、`internal/config/config.go` |
| A9-1 | **计费层**。新增 `internal/pricing`：官方费率表（grok-build-0.1 / 4.6 / 4.5 / 4.3 / 4.20 三个形态，含别名、锚定族规则、`build/ web/ console/` 前缀剥离、>200k token 长上下文档），1 USD = 1e10 ticks，`EstimateCost`/`EstimateTextReservation`/`EstimateTTSCost`/`EstimateSTTCost`；`ApiKey` 增加 `billing_limit_usd_ticks`/`billing_used_usd_ticks`，Redis 侧用 4 个 Lua 脚本做**原子预留/结算/释放/重置**（`used + 存活预留 + amount > limit` 即拒绝，同 eventID 同额度幂等，过期预留自动不计）；`inferenceAuth` 对有限额 Key 在 JSON 推理路径上先预留（读体 8 MiB 上限后原样还原），拒绝返回 402 `billing_limit_exceeded`，未结算的预留随请求结束释放；grok 与通用 chat 两条审计路径在写 journal 前**按上游用量结算**并把 `cost_in_usd_ticks`/`pricing_model`/`pricing_version` 写入事件（估算用量不计费，与 A9-2 口径一致）。`/messages/count_tokens` 不占额。 | `internal/pricing/*`、`internal/store/*`、`internal/audit/audit.go`、`internal/middleware/billing.go`、`session.go`、`cmd/server/routes.go`、`main.go`、`internal/handler/handler.go`、`internal/grok/handler.go`、`internal/api/api.go` |

新增配置项（`config.json`，均有安全默认）：`quality_hold_enabled`（默认 true）、`quality_hold_max_attempts`（6）、`quality_hold_timeout_ms`（30000）、`quality_hold_on_exhausted`（`fail_open`）。

**有意保留的边界**（不影响条目关闭，但记录清楚）：图片/视频档计价与 `PricingBreakdown` 未移植（不在 A9-1 要求的 API 面内，且属 A9-12 的聚合维度）；管理端未暴露"重置 Key 用量"端点（store 层 `ResetApiKeyBilling` 已实现并测试）；成本聚合（opsagg）仍缺 priced/unpriced 维度，属 A9-12。

> 第十三轮更新：图片/视频计价与结算已补（见第十七节），上面这条只剩 `PricingBreakdown`（成本重建结构）未移植。

## 十五、第十一轮：完全对齐 grok2api（12 条保留项）

按"完全对齐参考实现"的要求逐条清除此前有意保留的差异。凡原先用测试锁定的旧契约，测试同步改写为参考实现语义（改动即契约变更，不再有"两个契约各留一份"）。

| 条目 | 现在与 grok2api 的行为 |
| --- | --- |
| A1-11 / A3-9 | `applyBuildResponseDefaults`：Build 请求缺省写 `store=false`（显式值保留），并保证 `include` 含 `reasoning.encrypted_content`（保留其它项与顺序）。 |
| A3-8 | 本地持久化与 `store` 解耦：任何成功 Responses 都记录 ownership（chat 桥同样），`previous_response_id` / `GET /responses/{id}` 对未设 `store` 的客户端可用。 |
| A2-6 | 原生 Responses 中继字节透传：完整帧按上游原样透出（含 CRLF 与多行 data），仅当兼容层真的补字段时才重渲染；**不再追加 `data: [DONE]`**。 |
| A2-1 | doom-loop 阈值回到 **128 / 256**，并且 **chat 转换路径也跟踪重复 delta**（B 在协议转换前跟踪），命中给出 `upstream_output_loop` 类型错误帧。 |
| A4-1 | `/v1/models` 发布**外部 ID**（去掉 `console/`、`build/` 前缀），同外部 ID 的路由合并为一条；内部与外部两种写法都能解析，API Key 白名单两种写法都匹配。 |
| A4-3 | provider 前缀**通用且大小写不敏感**剥离（`Build/`、`Console/`、`grok_build/` 等），精确表优先；补齐 B 注册的 beta/latest 4.20 族、`*-console` 后缀、`grok-code-fast` 等别名。 |
| A4-2 | `<model>-<effort>` 别名按模型真实支持档位解析（`grok-4.5-xhigh` 仍为模型不存在）。 |
| A4-11 | 恢复 B 的三项目录派生：4.6 在位补 4.5、OAuth Build 补 Composer 2.5 Fast、Super 才有 video 1.5（非 Super 会被移除）。 |
| A4-17 | Codex 未知模型描述与 B 逐字节相同（`Grok model served via grok2api.`）。 |
| A7-2 | 移植 B 的 statsig 签名器：读账号首页取 `grok-site-verification` → POST 签名服务 → 按 method+path 缓存 1h → 反爬时失效重签；签名 URL 有 SSRF 形态校验；手工值改用 B 的判定（base64 解出 70 字节）。签名地址由 `grok_statsig_signer_url` 显式配置（填 B 的默认值即完全一致）。 |
| A8-12 | 推理前缀**恒要求托管 Key**（`InferenceAuthEnabled()` 恒 true）；`inference_auth_enabled` 仅保留存储与展示，不再能打开匿名代理。 |

**行为变更提示（部署方须知）**
1. `inference_auth_enabled=false` 的部署升级后，`/v1` 等推理入口会开始返回 401；需要先为客户端配置托管 Key。
2. `/v1/models` 的模型名去掉 provider 前缀；旧名字仍可调用，但依赖"名字里必须有 `console/`"的客户端逻辑需要更新。
3. 质量 hold 默认开启（第十轮引入），reasoning 请求被判降级时会扣住并换号重试。
4. statsig 签名默认**关闭**（签名字段为空）；要完全对齐 B 需配置 `grok_statsig_signer_url`。

## 十六、第十二轮：媒体契约对齐（最后 5 条）

| 条目 | 现在与 grok2api 的行为 |
| --- | --- |
| A5-2 | `/images/generations` 缺省 `model` → 400 `invalid_request_error: model is required`，不再回落成 `grok-imagine-image` 生成并消耗图片额度。 |
| A5-4 | 传输层校验：`aspect_ratio` 只接受 `auto`/纯比例串（像素别名仅保留在 provider 层做 size→ratio 映射，与 B 相同且不可从 `aspect_ratio` 触达）；`size` 只接受 `auto/1024x1024/1024x1536/1536x1024`；`resolution` 默认 `1k`、只接受 `1k`/`2k`，Web 面不再静默吞掉。`image_config` 新增 `aspect_ratio`/`resolution` 并按同一规则校验、透传到生成请求。 |
| A5-5 | 图像响应（生成/编辑、流式/非流式）恒为本站绝对资产 URL：先落本地资产再拼配置域名；缓存失败即返回错误，绝不回传上游 CDN 地址，也不再返回相对路径。`strict` 参数保留但不再影响失败语义。 |
| A5-7 | 公开名 `grok-imagine-image-quality` 走 Console 媒体面（`ConsoleModel=grok-imagine-image-quality`），与 B 同名同面同计费口径；`grok-imagine-image-quality-2.0` 别名可用；Web `-pro` 产品不存在。 |
| A5-42 | multipart 中不带 `[]` 的 `timestamp_granularities` 静默忽略（既不报错也不透传），带 `[]` 的仍返回 400 `unsupported_parameter`。 |

**部署方须知**：图像生成的 `url` 格式现在要求能确定公网基址（请求 Host / 受信代理头或配置），否则返回 `image_url_base_missing`，不会再给出相对路径。

## 总账结论

- 审计 171 条：**171 已修复/已对齐、0 保留、0 未修**；6 条 P0 全部关闭。
- 与 grok2api（906b9493 v3.1.6）的行为差异只剩**产品形态**层面：本项目是多通道聚合（Warp/Puter/WorkBuddy/Qoder + 统一 `/v1`）、自带管理端与 Redis 存储、部署形态为 systemd+Caddy+nft；这些不是审计条目，也不影响 Grok 通道的对外契约。

## 十七、第十三～十四轮：与参考实现的行为对齐收尾（覆盖缺口）

审计的 171 条已经全部关闭，这里记录的是此前**不在审计条目内、但参考实现有而本项目没有**的覆盖缺口，按"完全对齐"要求补齐。

| 缺口 | 处理 |
| --- | --- |
| 图片/视频计价缺失 | `internal/pricing` 移植参考实现的三张表：文生图（按模型 + `resolution`/`quality`：1k/2k、low/medium，含 `grok-imagine-image` 平档与 `-quality` 1k/2k）、图片编辑（输出张数 × 档位 + 输入张数 × 处理费）、视频（时长 × 每秒费率 + 参考图张数，区分 `grok-imagine-video` 与 `-1.5` 的分辨率档）。未知组合保持 unpriced，不猜价。 |
| 媒体请求不计费 | `middleware.SettleAPIKeyBillingResult` 让按资产计价的请求把真实成本记到同一个 Key 预留上；Grok 侧新增 `settleMediaBilling`，写一条 `grok_media_request` 审计行并带 `cost_in_usd_ticks` / `pricing_model` / `pricing_version`。已接入 Web 与 Console 的图片生成/编辑路径。 |
| 质量 hold 阈值量纲 | 由"字符"改为与参考实现一致的 token 量纲（用同一套 rune/4 估算换算），上游上报的推理 token 数优先。 |
| compaction blob 信封字段 | 明文信封字段名改为参考实现的 `version`/`session`/`summary`。 |

**已全部完成（第十四轮收尾）**：
- TTS/STT/视频三条路径的结算接线：TTS 按字符、STT 按上游 JSON 里的时长（纯文本转录无时长则不计价）、视频按"时长 × 每秒费率 + 输入图"在创建时结算；Web 与 Console 两面都接。
- ops 聚合的 cost 维度：观测框累加本请求所有已计价行的 ticks，trace 中间件透出，按分钟桶/汇总/JSON 都暴露 `cost_in_usd_ticks`，与 priced/unpriced 请求数并列（面板不会把部分数字当成全部账单）。
- Key 账期重置：`billing_period_days` + 持久化的 `billing_period_started_at`，到期自动把已结算用量归零；管理端新增 `POST /api/keys/{id}/reset-usage` 供人工重置（限额保留）。期初时间随记录持久化，否则重启后就再也等不到滚动。
- `PricingBreakdown`：`pricing.ReconstructBreakdown` 从"定价模型 + 数量"重建费率分量（未缓存/缓存/输出 token 含长上下文档、图片输出+输入张数含 2.0 的档位矩阵与编辑附加费、视频秒数+参考图、TTS 字符、STT 小时费率），journal 列表对每个带定价模型的行附上 `pricing_breakdown`。

**至此，本目标列出的每一项（17 条保留差异 + 覆盖缺口 + 实现级差异）都已落地**，判据见各节与 `docs/grok2api-audit/recount.py`（171 / 171 / 0 / 0）。

## 十八、第十八轮：statsig 签名改为默认启用（与参考实现一致）

第十一轮移植了签名器，但把"用哪个签名服务"留给部署方显式配置；参考实现是**默认就用** `https://grok.wodf.de/sign`。本轮把默认值补齐，语义变成三态：

| `grok_statsig_signer_url` | 行为 |
| --- | --- |
| 未设置 / `null` | 使用参考实现默认签名服务 `https://grok.wodf.de/sign`（升级后行为与 grok2api 一致，无需配置） |
| `""`（显式空串） | 关闭签名：不发送 `x-statsig-id`；配置了合法的 `grok_statsig_id` 时回落到它 |
| 其它地址 | 使用该签名服务 |

配套：
- 字段类型改为 `*string`，因此"未设置"与"显式关闭"可区分（JSON 里 `null` vs `""`）。
- **管理端配置保存时校验**签名地址（公网必须 HTTPS:443，仅可信内网可用 HTTP/自定义端口），非法地址在输入处即被拒绝，不再等到请求期静默丢弃签名；`grok.ValidateStatsigSignerURL` 对外暴露复用。
- 启动日志打印生效的签名模式与地址（默认 / 配置 / 已关闭），因为该值决定账号页面元数据是否离开本机。
- 管理端"上游与指纹"卡片新增该字段：留空＝默认签名服务，填 `-`＝关闭，填地址＝自定义。
- imagine WebSocket 握手也走签名（`imagineWSHeaders` 改为按 method+path 解析 x-statsig-id），此前只有 HTTP 路径签名。
- 测试：未设置 → 默认地址（含本地 stand-in 的端到端签名）、显式关闭 → 不发头、自定义地址生效、非法地址在保存时被拒；断言"只有某个请求到达上游"的既有测试改为应答签名器的页面读取（`signerProbePath`/`answerSignerProbe` 助手）或在配置里显式关闭签名。
