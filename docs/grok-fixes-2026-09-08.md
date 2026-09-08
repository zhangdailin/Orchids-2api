# Grok 对话链路修复说明（2026-09-08）

本次对应 `grok2api-deep-comparison-2026-09-08.md` 的行为级问题，基于本项目 `8748709` 实施。修改尚未提交或部署，不代表 us1 已经生效。

## 本次修改

后续剩余差异修复及验证记录见本文末尾的“剩余差异集中修复”。以下表格保留第一轮修改说明。

| 原报告编号 | 处理结果 |
|---|---|
| 1、2、9 | 重建 Responses→Chat 工具状态管理：按 item_id 跟踪，output_index 仅在缺少 item_id 时兜底；同一 added/done 不重复创建工具；立即发送身份和参数增量，保留参数空白；拒绝身份冲突、未知引用及完整响应中的非法 JSON |
| 3、4 | 显式 failed/error 穿透 Chat 和 Messages；异常 EOF、缺失终态、空完成不再输出正常成功；客户端写失败会终止转换并取消上游/关闭管道 |
| 5 | Messages 的 tool_use 必须有非空字符串 id/name 及 object input；tool_result 必须有有效引用 ID，禁止制造 `<nil>` 伪 ID |
| 6 | 修正 Responses→Chat 缓存详情保留，以及 Chat→Messages 的非缓存输入扣减；对缓存值进行上下界约束 |
| 7 | 将 incomplete 映射为 Chat length、Messages max_tokens；不再把截断标成正常完成 |
| 8 | 增加跨分片、本地停止序列过滤；保留实际 stop_sequence，支持 Unicode 和空正文命中停止词 |
| 10 | 思考摘要实时发送，按 reasoning item 选择首先出现的可读来源，避免 raw/summary 重复；传递思考块边界与签名；非流式思考按块保留，回放选择最新密文 |
| 11 | Messages 增加 server_tool_use、web_search_tool_result 和 citations_delta/非流式 citations；搜索身份去重、结果 URL 限定 HTTP(S)、失败结果显式输出 |
| 12 | 不再把名为 web_search/x_search 的普通 function 改成服务端搜索；Console 不再无条件注入搜索工具 |
| 13 | 增加模型和 Provider 级 effort 校验；4.5 不接受 none/xhigh，4.6 接受 xhigh，但不把 max 静默降级；Console 固定思考模型拒绝 effort 参数 |
| 14 | 明确 invalid_encrypted_content 的 HTTP 400 后，清除内存/持久化回放；同账号、同 endpoint 去除密文但保留可读摘要；必要时移除会话键，最多两次额外恢复；previous_response_id 不重置会话，其他 400 不触发该恢复 |
| 15 | 保留原生 compact；只有明确 404/405/501 才启用网关摘要兜底，最多 3 次，不执行工具；摘要使用持久化密钥及租户/模型作用域加密，可在后续请求中展开 |
| 16 | 质量重试改为显式开启；增加 256 字节/思考 token 比例的密文门槛、薄密文快速输出检测；usage 中的思考数量不单独作为证据；空闲与缺思考分别冷却，空流不因 hold 到期直接放行 |
| 17 | Chat 和原生 Responses 在完成/失败后审计；记录最终 usage、首内容/工具事件时间和总耗时；原生审计不再依赖可能截断的正文捕获；恢复 attempt 带 stage 标签 |

同时修复了非流式响应只保留第一条正文的问题，防止只有 reasoning output 时被误提取为可见正文。删除了被新转换器替代的旧工具聚合、搜索自动注入和旧审计提取辅助函数，没有保留双套运行时实现。

## 配置与兼容性变化

- `grok_quality_enabled` 默认 `false`。这是有意的默认行为变化：如需质量重试，须明确设置 `true`；工具校验、错误透传和空完成校验始终开启。
- 开启质量重试后，默认 hold 为 30 秒，空闲上限为 hold 的 4 倍；缺失思考默认冷却 12 小时，空闲账号冷却 15 分钟。已有显式配置的缺思考冷却值继续生效。
- 额外质量重试不用于包含工具的请求，以免重复服务端搜索或其他副作用。
- Console 调用内建搜索需要显式声明服务端工具，普通客户端 function 保持其原始身份。
- 无效的工具身份、JSON、终态和不支持的 effort 现在会明确失败；旧版接受或伪装成功的请求可能因此看到正确的错误。
- 网关 compact 使用 `orchids_compact_v1.` 封装，不冒充 grok2api 的密文格式；自身损坏/跨租户/跨模型状态被拒绝，其他供应方的不透明 compaction 内容原样转发。无持久化加密密钥时禁止退化成明文。
- 恢复过程通过 `X-Grok2API-Reasoning-Recovery` 标明去除失效密文或会话重置；网关 compact 通过兼容性警告头标明兜底。

## 验证覆盖

新增测试位于：

- `internal/grok/conversation_parity_test.go`：重复/并行工具、空白参数、未知引用、流中错误、空完成、提前 EOF、Messages 写失败、实时工具、停止序列、thinking/search/citation、多模型 effort、最终审计及质量门控。
- `internal/grok/compaction_recovery_test.go`：同账号有界恢复、非目标 400 不重试、previous_response_id 保护、持久化回放清除、compact 真实入口 fallback、加密状态回放、损坏状态拒绝及尝试上限。
- `internal/store/grok_state_cipher_test.go`：密钥重建后的解密、跨租户/模型拒绝、错误密钥、截断状态及禁止明文兜底。

现有测试中两项旧预期已纠正：缓存输入不再重复计入；实时思考按每个 item 的首个可读来源发送，而不是为了等待 raw 内容缓存全部 summary。

全仓库复测还发现审计测试依赖固定 100ms 睡眠导致偶发少读事件；已改为调用现有 Close 排空写入队列后再断言，并关闭测试 Redis 客户端。

验收命令：

```text
go test ./... -count=1
go vet ./...
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build ./cmd/server
git diff --check
```

本地验证使用合成事件、httptest 和临时 Redis；没有使用真实账号进行付费生成或对 us1 做改动。本机缺少可用 C 编译器，未执行 race detector；不能把单元/集成测试通过等同于线上真实上游 A/B 已完成。

## 边界

本轮是对话协议及稳定性修复，不是将两个项目整体合并。动态 effort 模型别名的发现/密钥开关、同名模型多 Provider 聚合、关系数据库、金额账本、代理管理台和媒体图库等仍不在本次修改内。质量策略的单位/配置形态、网关压缩封装也不要求与 grok2api 二进制或配置完全兼容。

上线后仍应针对用户之前的原始客户端做一次脱敏端到端复测，重点确认工具名称/ID、首工具事件延迟和多轮工具结果回传；没有当次原始 SSE，不能追溯性地宣称历史 150 秒延迟与空工具名已被唯一归因。

## 剩余差异集中修复

对应 `grok-remaining-review-2026-09-08.md`，本轮集中修复该报告的协议、恢复、诊断问题，包括其中单独列出的共同边界和 compact 隐藏尝试用量；不是关系数据库或管理台架构迁移。

| 范围 | 实现 |
| --- | --- |
| 拒绝回答 | 流式／非流式保留 refusal；Messages 显示拒绝文本，Responses 输出 refusal part；质量门控不再把拒绝回答当成缺思考重试 |
| 搜索域名约束 | 保留 allowed/blocked/excluded，校验互斥、别名冲突、空值、类型和每列表最多 5 个域名；blocked 转换为 filters.excluded_domains，并验证 Build/Console 请求中仍存在 |
| 思考签名 | 接收 added、done、terminal snapshot 的签名；已有可读思考不再阻止终帧补发。Messages 对具名、尚未收到签名的思考块暂缓 block_stop，迟到签名回到原索引；所有剩余块在 message 结束时关闭 |
| Chat→Responses | 替换旧转换器为按 item 独立状态实现，工具／思考 ID 稳定、参数增量不裁剪、思考不串段；坏 JSON、缺 finish_reason、缺 DONE、工具身份变化和无效完整参数明确失败；长度／过滤终态映射 incomplete |
| 引用与搜索展示 | Chat 支持终帧引用，去重区分引用位置；Responses 保留搜索 item、扁平 citation 与对应 annotation 事件，流式与非流式均覆盖 |
| 用量 | Responses 保留 cached/reasoning 等细分用量，不仅返回三项总数 |
| 解密恢复 | 支持三种明确的 decode 错误文案；只有实际 compaction decode 失败并携带 compaction 才保护原状态、跳过恢复；继续保留同账号、有界重试和 previous_response_id 的限制 |
| attempt 诊断 | 增加阶段、开始时间、HTTP 状态、少量白名单响应头、错误链、结构化错误字段、正文长度／摘要指纹／截断标记；响应 URL 去除用户信息与查询参数。账号凭据、Bearer、常见 credential/JWT 字段脱敏；不保存请求正文、模型输出或密文正文 |
| compact 重试用量 | 每次摘要在读取、校验之后记录 accepted／incomplete／summary_too_short／error 和其 reported usage；最终返回所有摘要尝试的已报告用量之和；三次全部失败时仍保留逐次记录 |
| 多行 SSE | 质量门控按完整 SSE frame 分类，支持多行 data、event 名补充 type；增加 frame 大小上限与坏帧错误处理，保留取消和关闭机制 |

新增生产实现分布于 `responses_stream.go`、`response_semantics.go`、`attempt_diagnostics.go` 及既有 Grok 管线，没有保留旧 Responses 转换器作为第二套运行时逻辑。非流式 Chat 写失败也会反馈至最终审计，失败或未送达响应不更新思考回放缓存。

验证：新增 `remaining_parity_test.go`、`remaining_recovery_test.go`，共 20 个顶层回归测试（另含子用例）。上一轮 10 个失败探针全部转绿；新增覆盖约束拒绝、Build/Console 实际 payload、refusal 质量门控、搜索与引用、迟到签名原索引、写失败退出、恢复文案、真实 compaction 保护、诊断脱敏、compact 成功与耗尽的逐次用量。

本地验收通过：`go test ./... -count=1`、`go vet ./...`、Linux amd64/CGO=0 构建。没有使用真实 Grok 账号生成，没有提交 GitHub 或部署 us1；本机未配置 C 编译器，未执行 race detector。

审计口径：`grok_upstream_attempt` 是尝试明细，`grok_request` 是逻辑请求结果；compact 的请求 usage 已聚合摘要尝试，统计时不要将两类事件直接相加。上游未报告 usage 时不推算金额或虚构 token。现有异步审计队列的持久化／容量语义不变，本次不是金融级金额账本改造。
