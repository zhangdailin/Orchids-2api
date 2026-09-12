# Grok 与 grok2api 对齐清单

本文以当前实现为准，用于跟踪与 `chenyme/grok2api` 的主要能力差异。

**最近一次复核基线（2026-09-12）**：上游 `chenyme/grok2api` **v3.1.5**，提交 `8913b53`（2026-09-09），`git fetch` 后与 `origin/main` 一致。该轮覆盖对话/推理回放、图片、视频、语音四条链路，未做真实上游端到端调用。逐项代码证据与修复过程随
Git 历史保留，本清单汇总能力状态与未完成项。

2026-09-08 补充：接口覆盖不代表行为完全等价。质量重试现需显式设置 `grok_quality_enabled=true`；Console 不再无条件注入搜索工具。

> 注意：描述**修复前**状态的差异报告已归档，不应作为当前缺陷证据。


## 已完成

2026-09-08 实现替换补充：质量分类器、语义空闲计时器及共享 SSE codec 已采用上游 `44a390b8` 对应实现；该轮仅覆盖记录中的模块，不表示全量能力等价。

- [x] Grok Build、Web、Console 凭据边界与账号级模型快照
- [x] Chat Completions、Responses、Anthropic Messages JSON/SSE
- [x] Web fast/auto/expert/heavy、Console 文本目录、Provider 前缀和账号发现的动态 Build 文本模型
- [x] 非 Build Responses 的逐事件实时转换（不再完整缓冲 Chat 流）及 `max_output_tokens` 映射
- [x] Messages adaptive/enabled thinking、预算、stop sequences、JSON Schema output_config、MCP、strict/function/web-search 工具和 encrypted signature 回放
- [x] Build/Console 流式响应缺失思考质量门控、最长等待、跨账号重试、可选 503/fail-open，以及首次冷却/二次持久禁用
- [x] Build Responses 工具兼容层：nullable 根 schema、namespace 展平、defer_loading、client/server tool_search、apply_patch、hosted/MCP 工具和 tool_choice
- [x] 原生 Build Responses 的 namespace/基础特殊工具身份回写，JSON 与 SSE 均不再泄漏 namespace 扁平工具名
- [x] Console 无状态请求规范化：清理 state hints，保留标准 text/工具/多模态输入并映射 response_format
- [x] 原生 Build SSE 与 Chat→Responses 的终态检查；空流、上游错误和提前 EOF 输出协议安全的 `response.failed`
- [x] 租户/模型隔离的 Prompt Cache identity、Provider 级账号粘性和 Redis encrypted reasoning replay
- [x] Build Responses compact、stored Response 查询/删除、`previous_response_id` 账号固定与 API Key 所有权隔离
- [x] Messages 文本、多模态、`tool_use`、`tool_result`、原生 `web_search_call` 历史与工具选择转换
- [x] 图片生成、图片编辑、`grok-imagine-image-2.0`、完整图片比例、`partial_images`、异步视频生成和本地媒体读取
- [x] Console 三个官方图片模型的 Provider 前缀路由、`resolution/quality` 校验、DPoP 请求和可信图片本地化
- [x] Build `grok-composer-2.5-fast`、4.6→4.5 兼容能力和 Super 专属视频能力归一化
- [x] Web 当前 `mediaGenInput.textToVideo` 协议（1–15 秒、无需合成媒体帖子）以及 Console 标准 `/videos/generations`、`/videos/edits`、`/videos/extensions`
- [x] `grok-imagine-video-1.5` 生成能力、标准视频任务 API Key 所有权隔离和可信媒体下载
- [x] 视频任务元数据 Redis 持久化，已完成任务可在进程重启后恢复查询和本地内容读取
- [x] 已取得上游 `request_id` 的标准 Console 视频任务在服务重启后固定原账号续跑
- [x] 视频任务 Redis 原子租约、心跳续期、租约丢失取消和多实例过期接管
- [x] 可配置共享媒体目录、多副本实例/集群身份、集群标记和启动读写探针
- [x] 临时图片/视频输入上传、24 小时 Redis 元数据、API Key 隔离及标准视频 `file_id` 解析
- [x] Console 原生 TTS、Voice 查询、HTTP/流式 STT 和 Realtime WebSocket
- [x] OpenAI 风格 `/audio/speech` 与 `/audio/tasks` 到 Console TTS 的转换
- [x] OpenAI `/audio/transcriptions` 的 `json`、`verbose_json` 与 `text` 兼容转换
- [x] Build Device OAuth、Token 刷新、Billing 与 Web quota 同步
- [x] 推理 API Key 默认鉴权，可显式关闭以接入可信上游网关
- [x] API Key 模型白名单、Redis 原子 RPM 限流、到期时间与管理端策略编辑
- [x] Redis 账号敏感字段 AES-256-GCM 加密和旧明文启动迁移
- [x] HTTP/SOCKS5 出口池、健康反馈、粘滞与 FlareSolverr
- [x] 可信代理链解析；默认剥离未受信任的转发头，登录限流和访问日志使用解析后的客户端地址
- [x] 客户端密钥与账号级并发策略；密钥支持模型、RPM、到期和 `max_concurrent`
- [x] 持久化模型 Route/Capability/AccountBinding，并在请求选路与 capability 校验中执行策略
- [x] 请求审计、跨账号 attempt/质量诊断和 token usage 账本；管理端 `/api/audit` 支持游标分页
- [x] Build 视频确认 403 后的 XAI fallback、一次性 HTTPS `upload_url` 回调和可信结果下载
- [x] 托管 egress 覆盖 Console Voice WebSocket，并复用节点 UA、clearance 和健康反馈
- [x] Prometheus 指标与受保护的 pprof

## 部分完成

- [~] Provider 前缀、动态 Build 路由、持久化 route/capability/account binding 已完成；同一公开模型的多 Provider 自动聚合仍待策略层
- [~] Web 视频已按当前捕获协议对齐为纯文本生成；图片/参考图视频需显式走 Console/Build，尚未做同一公开模型的 capability 自动选路
- [~] Console 标准视频链路完整；Build `/videos/generations` 已支持 OAuth 创建、轮询、可信下载、中断恢复、最多 8 张公共参考图、1.5 纯文本 1080p、确认 403 后 XAI fallback 和一次性上传回调；本地参考图仍需先转为公共 HTTPS URL
- [~] 标准视频生成、编辑、延长、查询、内容读取、`file_id`、重启续跑、多实例任务租约及共享文件系统已完成；提交前中断及旧 Web 分段任务不重放，尚无 S3 等对象存储后端
- [~] 出口支持静态配置、权重、健康冷却和 Voice WebSocket 租约拨号，尚无订阅导入、账号绑定和管理端节点编排

## 尚未实现

- [ ] 同名模型多 Provider 路由自动聚合
- [ ] API Key 的账号范围与金额额度（模型白名单、RPM、到期限制已完成）
- [ ] PostgreSQL 持久化及正式多实例拓扑
- [ ] 正式金额计费和完整媒体图库/结果资产元数据数据库（请求/attempt/token usage 审计已使用 Redis Stream）
- [ ] Trojan、VLESS、Shadowsocks、VMess、Resin 等隧道和出口 Quality Guard
- [ ] React 运维控制台、媒体画廊和在线 Swagger 文档

## 安全基线

- 不要关闭 `inference_auth_enabled`，除非服务只暴露给已完成认证的可信网关。
- 关闭推理鉴权后，stored Responses 和视频任务会进入共享的 `anonymous` 所有权空间；多租户部署应保持鉴权开启。
- `data/credential.key` 或 `ORCHIDS_CREDENTIAL_ENCRYPTION_KEY` 必须和 Redis 数据一起备份。
- API Key 只在创建时返回完整值；Redis 仅保存 SHA-256 哈希索引。
