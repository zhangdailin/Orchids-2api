# Grok 修复后剩余差异复查

修复状态：本文保留发现时的证据与失败结果。后续已根据用户授权修复 A1—A5、B1—B5、两项共同边界及 compact 隐藏尝试用量；实现与验证说明见 [集中修复说明](grok-fixes-2026-09-08.md#剩余差异集中修复)。不要将下文的历史失败结果当作当前测试结果。

日期：2026-09-08。检查对象是包含上一轮未提交修复的本地工作区，不是 us1 当前运行版本。

本地 HEAD：`8748709aa6bb260e8efbc29182b23da362e614de`。grok2api 比较基线：`44a390b890e7a3e0dd209b95b8c29a9f2b1be8dd`；本轮通过 `git ls-remote` 确认其仍为远端 HEAD。

本轮仅检查，新增本分析文档。验证探针放在系统临时目录，通过 Go overlay 注入，没有修改运行时代码、加入仓库测试、提交、部署或调用真实 Grok 账号。

## 一、明确的项目间差异

| 编号 | 优先级 | 本项目行为 | grok2api 行为 | 影响及证据 |
| --- | --- | --- | --- | --- |
| A1 | 高 | `response.refusal.delta` 没有分支，只有拒绝内容的完成被判为空响应；非流式正文提取也忽略 refusal | 流式 Chat 输出 refusal，Messages 转为拒绝文本；非流式解析 refusal | 正常拒绝变为网关故障，可能导致客户端无效重试。流式同输入 A/B：本地失败、上游通过 |
| A2 | 高 | Anthropic hosted search 的 `allowed_domains`、`blocked_domains`、`excluded_domains` 在请求反序列化时被丢弃，最终只剩 `type:web_search` | 验证域名列表与互斥规则，转换为 `filters`，blocked 映射为 excluded | 用户指定的搜索来源限制失效。allowed_domains 同输入 A/B：本地失败、上游通过 |
| A3 | 高 | 思考密文只在 `output_item.done` 等部分时机输出；若签名仅在 `output_item.added`，Messages 收不到 signature | Messages 开启 thinking 时，added 中的签名可以输出 | 思考展示与后续携带签名回放不完整。Messages 同输入 A/B：本地失败、上游通过 |
| A4 | 中 | 解密恢复只匹配 HTTP 400 的 `invalid_encrypted_content`；请求只要包含任意 compaction item 就直接跳过恢复 | 还识别 `could not decrypt the provided encrypted_content`；特定 compaction decode 文案也按是否真的携带 compaction 区分 | 部分上游已经明确拒绝的失效思考状态，仍不能进入安全恢复。双方源码确认，未做真实账号验证 |
| A5 | 中 | attempt 主要记录账号、耗时、状态、阶段和错误类别 | recovered attempt 保留受控截断/脱敏的响应诊断、状态、错误链等 | 仍难从本地 attempt 还原“哪次物理请求以什么响应被替换”。属于诊断能力差距，不等于完全没有审计 |

本地定位：

- A1：`internal/grok/console_stream.go` 的事件分发；`internal/grok/console.go` 的 `consoleExtractMessageText`。
- A2：`internal/grok/handler_messages.go:41`、`:140`。
- A3：`internal/grok/console_stream.go:325`；终帧补发另见下文。
- A4：`internal/grok/reasoning_recovery.go:13`、`:53`。
- A5：`internal/grok/handler.go:94`。

上游固定版本源码：

- [拒绝与思考事件转换](https://github.com/chenyme/grok2api/blob/44a390b890e7a3e0dd209b95b8c29a9f2b1be8dd/backend/internal/infra/provider/conversation/stream.go#L312)
- [搜索域名参数转换](https://github.com/chenyme/grok2api/blob/44a390b890e7a3e0dd209b95b8c29a9f2b1be8dd/backend/internal/infra/provider/conversation/messages_request.go#L696)
- [失效思考状态恢复](https://github.com/chenyme/grok2api/blob/44a390b890e7a3e0dd209b95b8c29a9f2b1be8dd/backend/internal/infra/provider/cli/responses_reasoning_recovery.go#L15)
- [恢复 attempt 诊断](https://github.com/chenyme/grok2api/blob/44a390b890e7a3e0dd209b95b8c29a9f2b1be8dd/backend/internal/application/gateway/attempt.go#L179)

注意：`max_uses`、`user_location`、`search_context_size` 在 grok2api 的这条 Anthropic→Build 搜索转换中也被主动略去，因此本轮不把它们列成“对方已支持而本项目未支持”。

## 二、本项目尚未闭合的转换链路

以下均有本地合成输入复现，但不能直接表述成“grok2api 的同一转换器通过”：两项目管线结构不同。

其中 B1—B4 限定为走 Chat→Responses 兼容转换的非原生路线。`HandleResponses` 对 Build CLI 原生路线提前返回，因此不能声称所有 grok-4.6 请求都会触发这些问题。

| 编号 | 优先级 | 遗漏 | 复现结果 | 定位 |
| --- | --- | --- | --- | --- |
| B1 | 高 | 结束状态与坏帧验证不完整 | Chat 的 `finish_reason:length` 变为 Responses `status:completed`；插入坏 JSON 且没有 finish_reason，只要有文本和 `[DONE]` 仍成功 | `handler_responses.go:891`、`:1065`；非流式固定 completed 在 `:692` |
| B2 | 高 | 多思考块没有独立身份与缓冲 | thinking → text → thinking 后，两个 reasoning item 复用同一 ID；第二段 summary 混入第一段，`first ` + `second` 成为 `firstsecond` | `handler_responses.go:818`、`:851`、`:925` |
| B3 | 中 | usage 细分字段在转换中丢失 | 输入 cached_tokens=80、reasoning_tokens=10，输出只有 input/output/total 三项 | `handler_responses.go:773` |
| B4 | 中 | 搜索与引用的转换覆盖不对称 | 源码确认流式转换未消费 `x_grok_search` 和 annotations，最终 output_text 的 annotations 固定空数组；非流式则能带部分 annotations | `handler_responses.go:908`、`:1048`；此行是源码确认，不计入探针复现数 |
| B5 | 中 | 已有可读思考时，终帧密文补发被跳过 | reasoning delta 已发送，密文只在 completed.output 到达时，密文未输出；内部缓存和客户端收到的状态不一致 | `console_stream.go:379` 附近，签名发送嵌套在 `state.text == ""` 中 |

补充审计边界：网关 compact 最多三次摘要尝试，但 `compaction.go:93` 在读取和校验正文之前记录 attempt，且返回的 usage 只来自最终成功摘要。前面过短或 incomplete 的尝试未记录自身 usage，不能据此声称账本覆盖了所有实际生成消耗。这是源码发现，未进行计费实测，也未断言 grok2api 对此已完整聚合。

## 三、不应误算为上游优势的共同边界

| 项目 | 本项目验证 | 上游核对 | 结论 |
| --- | --- | --- | --- |
| 仅在 completed.output 出现的 Chat URL citation | 已有文本 delta，引用只在终帧出现时丢失；`consoleFlatAnnotations` 不遍历 response 键 | 相同 Chat 合成输入也丢失终帧引用 | 可以改进，但不是已证实的双方能力差距 |
| 合法多行 data SSE 的质量识别 | 等价单行思考事件通过；多行事件被误判需重试 | `quality_retry_scan.go` 同样逐行交给 JSON 解析，未聚合同一 SSE frame 的 data 行 | 本地缺陷已复现；上游仅做源码核对，不声称完成相同入口 A/B。仅在本项目质量门控开启时影响请求，默认关闭 |

另：上游 Chat 不直接公开 opaque signature，Messages thinking 才公开。因此不能用“Chat 中有没有原始密文字符串”简单判定谁兼容。A3 使用的是 Messages 对照测试；B5 是本项目自身内部扩展的完整性检查。

## 四、验证与限制

本地正常测试：`go test ./internal/grok -count=1` 通过。

隔离契约探针：10 个顶层用例在当前实现中均触发了预期的缺陷断言；其中 multiline 用例的 single 对照子用例通过、multi 失败。这里的失败是有意用期望行为暴露遗漏，不是把测试写入仓库后导致现有测试套件变红。

同输入上游对照：拒绝回答、搜索域名限制、Messages added 签名 3 项通过，本地对应 3 项失败。终帧引用双方失败，已独立分类。

复现文件位于临时目录：

`C:/Users/zhangdailin/AppData/Local/Temp/grok2api-review-e611c4f0dc3f4d5aa125863e08be605c/probes/`

- `review2_orchids_test.go`、`review2-overlay.json`
- `review2_upstream_test.go`、`review2-upstream-overlay.json`

本地复现命令（工作目录为本项目）：

```powershell
go test -overlay C:/Users/zhangdailin/AppData/Local/Temp/grok2api-review-e611c4f0dc3f4d5aa125863e08be605c/probes/review2-overlay.json ./internal/grok -run '^TestReview2' -count=1 -v
```

这些探针覆盖特定边界，不代表线上失败率，也不能唯一归因用户之前的 150 秒首 token、空工具名或重复 ID 历史事故。未读取本轮远端实时 SSE，未验证 us1 已安装哪些修复。

## 五、建议顺序

先修 A1/A2/A3 与 B1/B2，处理会改变客户端执行、结束判断及思考块结构的问题；再补 A4、B3/B4/B5、attempt 完整诊断和 compact 隐藏尝试 usage。共同边界按兼容性增强处理。

动态 effort 别名、同名模型跨 Provider 聚合、关系数据库、金额账本、代理管理台、媒体图库等此前声明的架构差异仍需单独规划；本轮没有重新完整审计这些模块，不把旧列表冒充新增发现，也不宣称已经穷尽所有差异。
