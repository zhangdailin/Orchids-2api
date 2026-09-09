# Grok 工具页修复后复核

本轮只审计，不修改业务代码或部署。当前侧为工作区现有代码；上一次线上文件哈希与本地构建一致，本轮未重新核验正在运行的进程。上游本轮抓取并核对 HEAD：`ed9e0883006e57bcff7db40e485852beda0cf421`。其创作控制台两份源文件相对上次 `44a390b8` 没有变化，后端新增多轮 reasoning 桥接及工具 schema 归一化等修改。

## 修复后仍存在的聊天问题

| 项目 | 当前行为与影响 | 对照 | 当前源码 |
|---|---|---|---|
| 历史仍截断 | 5 条改为 40 条，只扩大阈值；超过后仍删除早期消息，未按轮次或 token 管理 | 对方没有对应40条裁切 | grok-tools.js:91、599 |
| 思考重绘丢失 | 保存 reasoning，但 rerenderChatThread 只传 content/attachment；刷新、切换会话后不显示思考 | 对方独立存储并渲染 reasoning | grok-tools.js:1571、1655 |
| 多段思考仍污染正文 | 正则只剥离开头第一段 think；正文后再次出现的 think 留在 content，下轮回传 | 对方按流字段维护正文/思考 | grok-tools.js:1571 |
| 旧历史未迁移 | 旧 assistant.content 内的 think 未在加载时拆开，依然原样回传 | 对方历史模型独立字段 | grok-tools.js:664、1865 |
| 失败回复未持久化 | catch 只更新 DOM，没有将部分回复加入 session.messages；刷新后消失，后续请求也不带它 | 对方非主动取消的错误会保留有效快照 | grok-tools.js:1584–1605 |
| 相同提问编辑错位 | startEditChatMessage 按 role+content 找第一条；两次同文提问可能编辑第一条而不是选中条 | 对方按 message.id 定位 | grok-tools.js:1305 |
| 工具活动仍缺失 | Web/X 开关存在，流解析未维护 tools 状态 | 对方显示执行状态与详情 | grok-tools.js:1529 |
| 模型能力仍按名称猜测 | has capability 与聊天下拉筛选规则不一致；voice/stt 名称可能进入下拉 | 对方依据路由 capability | grok-tools.js:1739 |

本轮直接执行当前保存逻辑的合成检查：

```text
输入：<think>first</think>answer
保存：content=answer, reasoning=first

输入：<think>first</think>answer<think>second</think>end
保存：content=answer<think>second</think>end, reasoning=first

rerenderChatThread 是否读取 msg.reasoning：false
```

因此“思考与正文已分离”“失败时保留内容”此前的描述只覆盖部分场景，不能视为完整修复。

## 图片、视频、语音功能差异

以下是页面可用功能差异，不等于后端没有对应 API，也不保证上游每个账号都能调用所有选项。

| 项目 | 当前工具页 | grok2api 创作控制台 |
|---|---|---|
| 图片批量 | 每批6张、并发2，可连续生成 | 单次可选1–4张 |
| 图片质量与模型 | 标准、快速均映射 lite；高质量映射 pro；标准走 images API，另外两档走管理任务/WS | 路由模型选择，独立分辨率1k/2k，符合条件才提供 quality |
| 视频模型 | 硬编码 grok-imagine-video | 从可用 video 路由选择 |
| 视频分辨率 | 480p/720p | 480p/720p/1080p，并在参考模式限制选项 |
| 视频编辑与延长 | 当前视频标签页只有生成流程 | 页面提供生成、编辑、延长 |
| 视频参考素材 | 单个图片 URL 或文件作为 input_references | 区分首帧、参考图片和参考音色，支持 file_id/URL 素材处理 |
| 语音产品形态 | 通过管理 token 接口连接 LiveKit，进行实时会话 | 文本转语音与上传音频转文字两个子模式 |
| 语音设置 | 固定音色/自定义ID、人格、语速；自定义指令禁用 | 查询音色列表，提供语言选择及TTS/STT模型选择 |

当前来源：`web/static/js/grok-imagine.js:2–8,307,366`；`web/static/js/grok-tools.js:2386,2879`；`web/templates/pages/grok-tools.html:282,286,374`。

上游来源（固定提交 ed9e0883）：

- `frontend/src/features/creative-console/creative-console-page.tsx:91,872,919,956,983,1024,1364`
- `frontend/src/features/creative-console/creative-console-api.ts:85,118,159,187`

图片连续生成、实时语音是当前侧的不同功能，不能简单列为缺陷或删除。视频1080p和参考素材也必须结合提供方、账号等级和后端限制评估。

## 后端仍需对齐的边界

当前 `internal/grok/console.go:452` 仍只有去密文后一次重试；没有完整的安全会话重置步骤。`session_state.go:271` 现在保留原 reasoning 对象及摘要，但与对方将可读信息转换为可携带输入的恢复处理仍不完全一样；仅仅删除 encrypted_content 的上游兼容性尚未通过故障注入或真实400恢复验证。

密文前置校验、恢复物理调用诊断、固定推理模型控件和 summary 默认值的差异仍存在。客户端原生 compaction 透传和 effort 不自动降级属于既有策略，继续保留其决策边界。

最新上游还新增 Chat/Messages 工具调用多轮 reasoning 缓存桥接。当前只有会话密文和已有消息转换，是否对各工具调用序列实现相同行为，需要专项协议回归；不能从“有回放缓存”推断已经对齐，也不能只凭新文件名就断言当前完全不支持。

建议顺序：先修思考重绘、多段思考、错误持久化及编辑定位；随后处理模型能力和工具活动；最后按产品需要对齐媒体入口，单独补密文恢复故障测试。
