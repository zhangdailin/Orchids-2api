# 当前 Grok 与 grok2api 差异复核（2026-09-09）

## 范围与证据边界

本轮只审计，不修改业务代码、不部署。当前侧以工作区现有代码（包含此前修复的未提交修改）为准；不使用旧对比报告作为当前缺陷证据。对照 chenyme/grok2api HEAD `44a390b890e7a3e0dd209b95b8c29a9f2b1be8dd`，本轮通过 git ls-remote 再次确认。

以下是代码路径确认的差异，不表示每项都已在生产环境复现。此前正常两轮请求成功，不能证明解码失败后的所有恢复分支都有效。本轮没有重新触发线上 400，也没有完成全部图片、视频、语音功能的端到端对比。

上游路径均相对于上述固定提交：https://github.com/chenyme/grok2api/tree/44a390b890e7a3e0dd209b95b8c29a9f2b1be8dd 。

## 已确认的 12 项实现差异

| # | 项目 | 当前 Orchids Grok | grok2api | 用户影响与定性 |
|---|---|---|---|---|
| 1 | 聊天历史长度 | MAX_CHAT_MESSAGES=5，发送后及加载历史时直接 slice(-5) | ChatPanel 保留会话消息，没有对应的 5 条消息裁切 | 高：丢失早期上下文；5 条是消息数，不是5轮，可能留下孤立 assistant。缓存键不能代替完整可见历史。 |
| 2 | 思考与正文回传 | 将 reasoning_content 包进 `<think>`，与正文一起存入 assistant.content；下轮原样提交 | content、reasoning、tools 分开存；toRequestMessages 只取正文 content | 高：思考被当普通回复再次输入，增加 token 并改变上下文语义。 |
| 3 | 页面请求协议 | 调 Chat Completions，再由后端转换 Responses；搜索使用 x_responses_tools 扩展 | 页面直接发 Responses input/tools | 协议差异：当前多一层转换，不能仅凭按钮相同就判断事件与参数等价。不是说当前后端不支持 Responses。 |
| 4 | 工具执行展示 | 流读取主要处理 content、reasoning_content、refusal；没有保存、展示 tools 活动模型 | Responses 事件维护 tools 快照，展示搜索等工具的状态及详情 | 中：能打开 Web/X 搜索，但不能同样观察搜索进度；只有工具输出时，当前还可能报“无可显示内容”。 |
| 5 | 默认思考摘要 | Build Chat 转换默认 summary=concise | 页面 auto/有 effort 时发送 summary=auto；none 只发送 effort=none | 行为差异：摘要策略并未一致。不能由此断言哪一边一定更快。 |
| 6 | 固定推理模型的控件 | 所有聊天模型共用 effort 选项 | isFixedReasoningConsoleModel 对应模型强制有效值 auto，并禁用选择 | 中：当前页面可以提供上游未必支持的选择。应以具体路由能力决定，而非全局强制。 |
| 7 | 模型能力识别 | 通过模型 ID 的 imagine-image、imagine-video、voice、stt 等字符串判断；聊天下拉只排除 imagine | 使用路由 capability，聊天限定 chat/responses，语音区分 tts/stt/realtime | 高：自定义别名易误判；如果列表含 voice/stt，它们可能进入当前聊天下拉。存在模型条目不等于对应操作实际可用。 |
| 8 | 流中途出错 | 非 AbortError 时将已显示内容替换为 error，未写入 assistant 历史 | 出错时存在有效文本/思考/工具快照则保留部分结果 | 中：长回复后段失败时，当前可能把前面已收到的内容一起丢掉。 |
| 9 | 回放密文校验 | 捕获并存储字符串，没有上游对应的格式、解码长度和熵校验 | normalizeReasoningItem 调 validGrokReplayEncryptedContent，校验后才回放 | 高：移除 content:null 只对齐了输入形状，未对齐无效密文的前置过滤。 |
| 10 | 400 恢复步骤 | Build 兼容 Chat 路径，在启用回放且 input 可移除密文时，移除后重试一次 | 有条件地去密文重试；仍失败且安全时重置会话身份再试 | 高：只有服务端会话损坏、或去密文仍失败时，当前恢复覆盖不足。原生 Responses 另受透传策略约束，见后文。 |
| 11 | 恢复时保留语义和 compaction 保护 | stripInjectedReasoningReplay 删除所有含 encrypted_content 的 reasoning 项，不检查注入来源，不保留其中可读摘要，也没有真实 compaction 项保护判断 | stripReasoningEncryptedContent 保留可读信息；遇 compaction 解码措辞且请求真含 compaction 时不改写 | 高：恢复函数的边界尚未对齐。普通页面未必构造这种混合输入；这是可达性需进一步测试的底层差异，不能宣称已在线上丢数据。 |
| 12 | 恢复诊断粒度 | 内部补记固定 attempt=1 的失败，再由外层账号重试记录；未分别保存完整恢复物理调用序列 | RecoveredAttempt 分阶段记录 URL、开始时间、耗时、状态和结果 | 中：更难准确区分原始400、密文重试、会话重置及其分别耗时。 |

## 源码定位

当前前端：`web/static/js/grok-tools.js`。

- #1：91、599–605、667、1572 行；上游 `frontend/src/features/creative-console/creative-console-page.tsx` 的 ChatPanel、parseChatSession。
- #2：1530–1570、1849–1874 行；上游同文件 342 行 toRequestMessages、399 行结果存储。
- #3/#5：1849 行 buildChatPayload；`internal/grok/responses_normalize.go` 186–205 行；上游 `frontend/src/features/creative-console/creative-console-api.ts` 54–82 行。
- #4：当前 1520–1570 行；上游 API 文件 426 行开始的流快照逻辑、页面 1698 行 ToolActivityItem。
- #6/#7：当前 1730–1748、1883 行及 HTML effort 选择器；上游页面 134–141、281–283、807 行。
- #8：当前 1586–1593 行；上游页面 399–415 行。
- #9：`internal/grok/session_state.go` 的 storeReasoningReplay、responseEncryptedReasoning；上游 `backend/internal/pkg/reasoningreplay/normalize.go` 100–131 行。
- #10/#11/#12：`internal/grok/console.go` 448–465 行、`internal/grok/session_state.go` 271–302 行；上游 `backend/internal/infra/provider/cli/responses_reasoning_recovery.go` 的 recoverReasoningDecodeFailure、stripReasoningEncryptedContent、recordHidden。

## 另有 2 项明确的策略差异，不应直接当作缺陷修复

1. **原生 Responses/compact 的错误恢复边界**：当前 `internal/grok/relay_policy_test.go:37` 要求保留客户端原始 context/compaction 及上游拒绝，不合成恢复；grok2api 的适配器包含受条件限制的恢复。要区分客户端持有的压缩状态与网关注入的回放，不能把后者修复扩大成改写前者。
2. **reasoning effort 归一化**：当前 `relay_policy_test.go:141` 明确保留 max、xhigh、未来 effort；上游 `backend/internal/infra/provider/cli/normalize.go:210` 根据模型映射 effort 别名。当前策略符合此前不自动降级的要求，不应为了“相同”直接照搬。

## 当前侧额外发现，暂不计入对照差异

- saveChatSessions 只持久化附件 name/type，不保存 dataUrl；刷新后旧附件不会再随请求发出。上游本轮所读聊天路径是纯文本，不应把这项写成“对方已实现附件持久化”。
- clearReasoningReplay 在释放 sessionMu 后删除 Redis，而存储路径在锁内持久化。并发清理/保存存在内存与持久化状态不一致的窗口；需要并发测试后评估实际影响。

## 不再列为缺失的部分

当前已有会话 prompt_cache_key、effort 和 Web/X 开关、requestAnimationFrame 合并渲染、Build Chat 默认摘要、effort/summary 诊断字段，以及回放项移除 content:null。这些均是部分对齐，不等于相关功能链路全部一致。

建议先处理 #1/#2 的上下文语义，再补 #9–#12 的恢复边界与故障注入测试，然后对齐 #4/#7/#8 的可见行为。#3/#5/#6 和两项策略差异需要结合产品目标选择，不宜机械复制。
