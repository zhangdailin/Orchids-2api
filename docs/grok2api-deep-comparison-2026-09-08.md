# Grok 行为级差异复核（2026-09-08）

> 本文保留修复前基线与复现结果。后续实现、配置变化及验证范围见 [修复说明](D:/Code/Orchids-2api/docs/grok-fixes-2026-09-08.md)。

## 结论与范围

用户反馈的体验差距有代码依据。当前主要问题不是“缺少哪个 API 路径”，而是 Build/Console 原生 Responses 转换为 Chat Completions、再转换为 Anthropic Messages 时，工具身份、事件顺序、失败状态和用量语义没有完整保留。

本轮构造了 8 组针对性协议样本：本项目 8 组未通过预期断言，grok2api 的对应转换器 8 组通过。这是选定边界场景的确定性结果，**不是线上请求失败率，也不是模型智力评分**。

复核基线：

- 本项目：`8748709aa6bb260e8efbc29182b23da362e614de`，检查开始时工作区干净。
- grok2api：`44a390b890e7a3e0dd209b95b8c29a9f2b1be8dd`，提交日期 2026-09-04，浅克隆后逐文件检查。
- 旧对齐清单使用的上游基线：`62d2775cb3cd5196cc885dd98e323c90afeda023`，2026-08-25。“接口已支持”不等于下列行为已经对齐。
- 本轮没有进行真实账号的付费生成 A/B，也没有重新核对 us1 的部署 commit。以下是这两个源码版本的结论，不把本地版本直接当作服务器正在运行的版本。
- 本轮未修改生产代码、未提交 GitHub、未部署；只新增本报告。测试通过工作区外的 Go overlay 注入，不向项目添加测试专用运行时代码。

## 一、先明确受影响的调用路径

本项目 `grok-4.6` 的静态定义指向 `UpstreamCLI`，上游模型名称仍是 `grok-4.6`。没有证据说明这里把请求偷偷改成了另一代模型。

相关入口的响应路径如下：

| 客户端入口 | 本项目 Build/Console 响应处理 | 本次结论适用范围 |
|---|---|---|
| Chat Completions，流式 | 上游 Responses SSE → `streamConsoleChat` → Chat SSE | 下文工具聚合、错误、截断问题直接适用 |
| Anthropic Messages，流式 | 上游 Responses SSE → Chat SSE → `translateOpenAIChatStreamToAnthropic` | 同时承受上述问题和第二层转换的信息损失 |
| 原生 Build Responses | 原生 Responses 的独立转发/兼容路径 | 不能把 Chat 转换器的所有问题直接套用到此入口 |
| Web Provider | 另一套上游协议和工具处理 | 不能用 Web 工具正则解析解释全部 Build 故障 |

本地定位：[模型定义](D:/Code/Orchids-2api/internal/grok/models.go:57)、[Build Chat 入口](D:/Code/Orchids-2api/internal/grok/cli_chat.go:19)、[共享响应处理](D:/Code/Orchids-2api/internal/grok/console.go:469)、[Messages 流桥接](D:/Code/Orchids-2api/internal/grok/handler_messages.go:778)。

## 二、已通过双边样本确认的 8 项差异

优先级含义：P0 为工具/失败协议正确性，建议首先修；P1 为输出控制与统计语义。优先级不是安全漏洞评级。

| 编号 | 优先级 | 样本 | 本项目实际输出 | grok2api 实际输出 | 用户影响 |
|---|---|---|---|---|---|
| 1 | P0 | 同一个 function_call 依次出现 output_item.added、output_item.done | 生成两条工具调用，index 不同但 call_id 相同 | 仅一条工具调用 | 重复执行、重复 ID、后续工具结果无法正确关联 |
| 2 | P0 | 两个工具先 added，参数 delta 按 item_id 交错到达 | 参数都拼到“最近工具”；Read 为 `{}`，Search 为两个 JSON 串拼接 | 按 item_id 分别聚合 | 工具拿到空参数、错误参数或非法 JSON |
| 3 | P0 | 上游发送 response.failed，内含具体错误 | 丢失错误，发空内容、finish_reason=stop 和 [DONE] | 发出包含失败原因的流式 error | 上游失败表现成“正常完成但没有内容” |
| 4 | P0 | 流式失败经过 Messages 输出边界 | Chat 顶层 error 被忽略，转成空 message + end_turn + message_stop | 对应原生失败转换成 Messages error，不发成功 message_stop | 使用 Messages 的工具客户端无法取得真实失败原因 |
| 5 | P0 | Messages 历史中有两个缺少 id 的 tool_use | 缺失值被格式化成字符串 `<nil>`，最终报 duplicate tool_use id "<nil>" | 在第一个无效块处明确拒绝，指出 id/name/object input 无效 | 请求校验制造伪 ID，报错误导排查 |
| 6 | P1 | prompt_tokens=100，其中 cached_tokens=80 | Messages input_tokens=100，cache_read_input_tokens=80 | input_tokens=20，cache_read_input_tokens=80 | 缓存输入被重复计入，客户端统计不一致 |
| 7 | P1 | 部分文本后 response.incomplete，原因为 max_output_tokens | finish_reason=stop | finish_reason=length | 被截断的答案被标成正常结束，影响自动续写/重试 |
| 8 | P1 | 客户端 stop=END，上游仍返回 before END after | 下游保留 END 及其后内容 | 在 END 前截断 | 缺少网关侧停止序列保障 |

### 样本边界与证据

- #1、#2：本地 [console.go](D:/Code/Orchids-2api/internal/grok/console.go:890) 对含 function_call item 的事件直接 append；[参数处理](D:/Code/Orchids-2api/internal/grok/console.go:927) 使用 activeToolCall，没有按事件 item_id 定位。#2 特意不提供后续完整 arguments 快照，以检查增量关联；快照有时能覆盖错误参数，但不消除 #1 的重复调用问题。
- #3、#7：本地 [streamConsoleChat](D:/Code/Orchids-2api/internal/grok/console.go:809) 没有对应 failed/incomplete 终态处理，末尾无工具时统一返回 stop。grok2api [原生事件分发](https://github.com/chenyme/grok2api/blob/44a390b890e7a3e0dd209b95b8c29a9f2b1be8dd/backend/internal/infra/provider/conversation/stream.go#L293) 区分 added/done、按 item_id 路由参数，并处理失败和不完整终态。
- #4：本地 [Chat→Messages](D:/Code/Orchids-2api/internal/grok/handler_messages.go:815) 只处理 choices，忽略顶层 error。grok2api 不经过这层 Chat 桥接，直接把原生失败转成 [Messages error](https://github.com/chenyme/grok2api/blob/44a390b890e7a3e0dd209b95b8c29a9f2b1be8dd/backend/internal/infra/provider/conversation/messages_stream.go#L287)。因此本项比较的是两边相同失败语义的输出边界，不是完全相同的内部输入格式。
- #5：本地 [tool_use 解析](D:/Code/Orchids-2api/internal/grok/handler_messages.go:461) 使用 `fmt.Sprint(block["id"])`。上游使用 [结构化 Messages 输入校验](https://github.com/chenyme/grok2api/blob/44a390b890e7a3e0dd209b95b8c29a9f2b1be8dd/backend/internal/infra/provider/conversation/messages_request.go)。这是缺失 ID 的错误处理差异，不意味着应当接受没有 ID 的工具历史。
- #6：本地 [usage 转换](D:/Code/Orchids-2api/internal/grok/handler_messages.go:742) 未减缓存；上游 [usage 转换](https://github.com/chenyme/grok2api/blob/44a390b890e7a3e0dd209b95b8c29a9f2b1be8dd/backend/internal/infra/provider/conversation/messages_response.go#L107) 对缓存值限界后相减。现有本地测试也期待 input_tokens 保持总量，说明“测试通过”可能只是固化了错误预期。
- #8：本地 [请求规范化](D:/Code/Orchids-2api/internal/grok/responses_normalize.go:148) 只向上游传 stop，转换器不执行本地过滤；上游 [Chat 流输出](https://github.com/chenyme/grok2api/blob/44a390b890e7a3e0dd209b95b8c29a9f2b1be8dd/backend/internal/infra/provider/conversation/chat_stream.go#L17) 使用 stopFilter。该测试证明“上游不执行 stop 时”的保障差异，不宣称上游一定忽略 stop。

两个关键实测输出：

```text
单工具 added + done：
本项目：call_1/index=0 + call_1/index=1
上游：  call_1/index=0

两个工具交错增量：
本项目：Read={}；Search={"path":"a"}{"query":"b"}
上游：  Read={"path":"a"}；Search={"query":"b"}
```

## 三、源码确认、但尚未做真实账号 A/B 的行为差异

| 编号 | 差异 | 本项目 | grok2api | 影响与判断 |
|---|---|---|---|---|
| 9 | 工具调用的流式时机 | 收集完上游流后才一次发送工具调用 | 工具开始时发身份，后续连续发参数 delta | 工具出现更晚；不能据此单独解释某次 150 秒 TTFT |
| 10 | 思考摘要的实时性和状态粒度 | summary 暂存至正文或流结束；raw/summary 选择是整条流的全局状态 | 按 reasoning item 维护状态、选择内容来源并处理 signature | 思考显示时机和多块思考的保留行为不同，不只是 UI 标签问题 |
| 11 | Messages 内建搜索结果 | 能识别部分声明/历史，但输出桥接仅处理文本、思考、普通 tool_calls，未完整输出搜索块和引用 | 生成 server_tool_use、web_search_tool_result，处理结果、引用和去重 | 可能“说去搜了”却缺少客户端可见的搜索过程/引用；不代表每次都没有实际搜索 |
| 12 | 客户端工具与内建搜索的边界 | 普通 function 名为 web_search/x_search 时改成内建工具，schema 不再保留；Console 另有自动注入搜索 | 普通 function 保留身份；按显式工具类型规范化内建搜索 | 客户端原本要自己执行的工具可能被改为服务端执行；Console 默认能力也不同 |
| 13 | 思考档位的模型级约束 | 通用字符串归一化：minimal→low、max→xhigh，没有等价的完整模型/Provider 档位表 | 4.5、4.6、Console 固定思考模型分别校验，按策略生成 effort 别名 | 同一参数在不同模型上的拒绝/透传行为不同；不是设置 high 就一定等价 |
| 14 | 加密思考回放失败恢复 | 有租户/模型隔离缓存及会话粘性，但未找到等价的 invalid_encrypted_content 恢复流程 | 特定 400 后清理失效回放，同账号/同传输路径重试，保留可读摘要，必要时重建会话键并记录恢复 | 长对话或缓存漂移后恢复能力不同 |
| 15 | compact 的实现语义 | 已支持原生 Build /responses/compact 转发 | 另有网关摘要压缩、加密封装和恢复/展开机制 | 不是“本项目没有 compact”，而是长上下文维护策略不同 |
| 16 | 思考质量重试的判定 | 默认 hold 30 秒；超时放行；较宽松地认可加密内容/usage 中的思考证据；默认冷却 10 分钟 | 可选质量门控；加密内容长度/占比、突发返回等判据更细，缺思考与空闲使用不同冷却规则 | 策略不等价；实际优劣依赖启用配置和账号状态，不能只比 attempts 数量 |
| 17 | 完成时审计 | Build/Console Chat 在读流前以 HTTP 状态记 request，usage 传 nil | 具有流完成/失败回调、首 token、总耗时和最终 usage 的记录路径 | 本地日志 200 不能证明生成成功，usage 账本在该路径不完整 |

对应证据：

- #9–10：本地 [console.go](D:/Code/Orchids-2api/internal/grok/console.go:820)；上游 [工具增量](https://github.com/chenyme/grok2api/blob/44a390b890e7a3e0dd209b95b8c29a9f2b1be8dd/backend/internal/infra/provider/conversation/chat_stream.go#L54)、[reasoning item 状态](https://github.com/chenyme/grok2api/blob/44a390b890e7a3e0dd209b95b8c29a9f2b1be8dd/backend/internal/infra/provider/conversation/stream.go#L438)。
- #11：本地 [Messages 输出桥接](D:/Code/Orchids-2api/internal/grok/handler_messages.go:815)；上游 [搜索结果输出](https://github.com/chenyme/grok2api/blob/44a390b890e7a3e0dd209b95b8c29a9f2b1be8dd/backend/internal/infra/provider/conversation/server_web_search.go#L296)。本地已有部分搜索历史保留，不应误写成完全不支持搜索。
- #12：本地 [function→内建工具](D:/Code/Orchids-2api/internal/grok/console.go:114)、[Console 注入](D:/Code/Orchids-2api/internal/grok/responses_normalize.go:189)；上游 [工具输入转换](https://github.com/chenyme/grok2api/blob/44a390b890e7a3e0dd209b95b8c29a9f2b1be8dd/backend/internal/infra/provider/conversation/chat_request.go#L263)。
- #13：本地 [档位归一化](D:/Code/Orchids-2api/internal/grok/responses_normalize.go:339)；上游 [模型/Provider 思考能力表](https://github.com/chenyme/grok2api/blob/44a390b890e7a3e0dd209b95b8c29a9f2b1be8dd/backend/internal/domain/model/reasoning.go)。
- #14–15：本地 [session_state.go](D:/Code/Orchids-2api/internal/grok/session_state.go:154)、[compact 入口](D:/Code/Orchids-2api/internal/grok/handler_responses_store.go:195)；上游 [回放失败恢复](https://github.com/chenyme/grok2api/blob/44a390b890e7a3e0dd209b95b8c29a9f2b1be8dd/backend/internal/infra/provider/cli/responses_reasoning_recovery.go)、[网关 compact](https://github.com/chenyme/grok2api/blob/44a390b890e7a3e0dd209b95b8c29a9f2b1be8dd/backend/internal/infra/provider/cli/responses_compaction.go)。上游也明确允许加密思考跨账号复用，不能把“没有账号级回放隔离”本身认定为本项目独有缺陷。
- #16：本地 [质量重试](D:/Code/Orchids-2api/internal/grok/quality_retry.go:138)；上游 [质量门控判据](https://github.com/chenyme/grok2api/blob/44a390b890e7a3e0dd209b95b8c29a9f2b1be8dd/backend/internal/application/gateway/quality_retry.go)。两边看到真实 reasoning delta 都可能提前放行；不能声称上游保证杜绝所有“只思考、无最终正文”的回复。
- #17：本地 [读流前审计](D:/Code/Orchids-2api/internal/grok/console.go:485)、[审计字段赋值](D:/Code/Orchids-2api/internal/grok/handler.go:133)；上游 [完成回调及计时](https://github.com/chenyme/grok2api/blob/44a390b890e7a3e0dd209b95b8c29a9f2b1be8dd/backend/internal/application/gateway/service.go#L1069)。此外，本地 Redis 审计默认约 10000 条、异步缓冲满时丢弃，不能直接当不可丢失的财务账本。

## 四、仍存在的架构差异：不要与“模型变笨”混为一谈

这些不是本轮才新增发现，也不应优先于上述协议问题重构。现有项目已具备相当多的媒体、鉴权、并发和路由能力。

| 范畴 | 本项目当前状态 | 与 grok2api 的差距 |
|---|---|---|
| 模型路由 | 已有 Provider 前缀、动态 Build 模型、持久化 Route/Capability/AccountBinding | 同一 public ID 的多 Provider 候选聚合及能力选路尚不等价 |
| 客户端密钥 | 已有模型白名单、RPM、过期时间、并发限制 | 上游另有 Provider/账号等级范围和金额预算/预留/扣账模型 |
| 数据和账本 | Redis 持久化业务状态、Stream 审计 | 与上游关系型数据模型、完成时账本和管理查询能力不同；不是 Redis 本身导致工具调用出错 |
| 出口管理 | HTTP/SOCKS5 静态池、权重、冷却、粘滞、FlareSolverr、Voice WebSocket | 订阅导入、节点编排、账号分配、多协议隧道及 Quality Guard 等仍未等价 |
| 媒体资产与管理台 | 图片/视频/音频、多实例视频租约和共享媒体目录已有实现 | 完整资产元数据/图库、对象存储形态与管理体验仍有差距；这与文本流协议是不同层问题 |

本地状态参见 [原有对齐清单](D:/Code/Orchids-2api/docs/grok2api-parity-checklist.md)。上游可核对 [多路由候选入口](https://github.com/chenyme/grok2api/blob/44a390b890e7a3e0dd209b95b8c29a9f2b1be8dd/backend/internal/application/gateway/service.go#L548)、[Key 的范围与金额字段](https://github.com/chenyme/grok2api/blob/44a390b890e7a3e0dd209b95b8c29a9f2b1be8dd/backend/internal/domain/clientkey/client_key.go#L220)。架构表不是全量功能验收，本轮测试重点仍是影响用户报错的对话链路。

## 五、如何解释用户之前的实际报错

| 用户现象 | 本次能确定什么 | 仍不能确定什么 |
|---|---|---|
| completed response with no content | #3/#4 能确定性地把失败转换成成功空回复，是应优先修复的候选原因 | 没有当次原始 SSE，无法断言历史那次就是 response.failed |
| duplicate tool_use id "<nil>" | #5 复现完全相同错误；#1 还会制造真实 call_id 的重复 | `<nil>` 与真实重复 ID 是两条机制；不知道客户端最初为什么缺失 ID |
| unknown tool "" | 工具状态机存在明确错误，且缺少严谨的工具身份校验 | 本轮样本没有复现空工具名的完整产生链，不能给出已定位的假结论 |
| 思考显示异常、只说要搜索 | 思考摘要发送、工具流式时机和搜索结果映射确实不同 | 不能仅据此断言模型未搜索或上游生成质量一定更差 |
| 首 token 150 秒 | #9/#10 及质量门控均可能影响客户端可见时间 | 需要账号等待、重试、上游首事件/首思考/首正文和下游发送时间，不能归因给一个固定 30 秒门控 |

需要修正此前过强的推断：**HTTP 200 只是 HTTP 层状态，quality accepted 只是门控判断，均不等价于完整生成成功。** 没有负载证据时，不应把“上游成功完成但没有正文”写成已证实根因。

另外，单纯缺少显式终态后 EOF 的情况，不能笼统当成本项目独有差异：上游转换器也有 EOF 收尾行为。这里已经明确复现的差异是显式 failed/incomplete 事件如何处理。

## 六、修复顺序与验收标准

1. 先修协议正确性：用 item_id/index 的状态表替换 activeToolCall；区分 added/delta/done；验证工具名、call_id；让错误和不完整终态穿透 Chat、Messages 两层。验收：#1–5、#7 同时通过，工具执行副作用不重复。
2. 再修实时性与输出语义：工具身份/参数及时发送；按 reasoning item 管理摘要与签名；恢复搜索结果块/引用；实现 stop 和正确 usage。验收：#6、#8 通过，并补多工具、多思考块、搜索和跨 delta stop 测试。
3. 再修多轮稳定性与可观测性：模型级 effort 校验、失效 reasoning 恢复、compact 语义；审计在流终态记录 outcome/usage/TTFT/总耗时。验收：不能再出现“流内失败却只记录成功 200”。
4. 最后才考虑多 Provider 调度、关系数据库、出口平台和图库等大改造。它们不能替代前三步。

线上验证应使用固定提示词、相同模型/effort/工具 schema、尽可能一致的账号条件，分别测 Chat、Messages、原生 Responses。记录脱敏后的原始事件和下游事件；不得在日志里保存账号 token、API Key 或无关用户对话。

## 七、复现记录

临时检查目录（不在 Git 工作区内）：

```text
C:\Users\zhangdailin\AppData\Local\Temp\grok2api-review-e611c4f0dc3f4d5aa125863e08be605c
```

`probes/orchids_probe_test.go` 和 `probes/upstream_probe_test.go` 分别测试两边转换器；`overlay.json`、`upstream-overlay.json` 将探针映射为包内临时测试。仅使用合成内容，不调用真实账号，不发送真实搜索，不修改服务器。

在本项目执行：

```powershell
go test -overlay C:\Users\zhangdailin\AppData\Local\Temp\grok2api-review-e611c4f0dc3f4d5aa125863e08be605c\probes\overlay.json ./internal/grok -run '^TestReview' -v -count=1
```

在临时克隆的 `backend` 目录执行：

```powershell
go test -overlay ../probes/upstream-overlay.json ./internal/infra/provider/conversation -run '^TestReview' -v -count=1
```

结果：本项目 8 FAIL，上游 8 PASS；这里 FAIL 指预期正确行为断言未满足，不是编译失败。本轮没有运行全仓库测试，也没有用这 8 个样本声称所有能力已经穷尽。临时目录可能被系统清理，以上精简样本描述和源码定位用于保留报告的可核查性。
