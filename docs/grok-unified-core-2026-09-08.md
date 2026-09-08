# Grok 请求核心强制合并

本轮直接合并重复执行逻辑，不以移动文件或增加抽象层作为精简成果。比较基线是本轮开始时的工作区，包含此前尚未提交的修改，不是 Git HEAD。

## 已合并

| 范围 | 原实现 | 合并后 |
| --- | --- | --- |
| HTTP 响应生命周期 | Web、Console、Build 分别执行请求和解压 | 共用 `doUpstreamHTTP`，统一解压失败关闭响应；原生 Responses 共用 SSE idle 包装 |
| Build 鉴权请求 | 聊天、资源、模型、账号校验、账单、订阅、fallback 各自获取 token 和构建请求 | 全部调用 `CLIClient.request`，共用认证、身份头、托管出口和响应处理 |
| 原生聊天编排 | `serveCLIChat` / `serveConsoleChat` 各自维护流程 | 共用 `serveNativeChat` 的 payload、账号切换、质量重试及收尾；仅协议差异保留条件分支 |
| 视频 fallback | POST 与 GET 各自维护完整客户端逻辑 | `doFallbackRequest` 接收方法、路径及可选 payload |
| 管理批处理 | NSFW、额度刷新分别创建逐项 goroutine 和信号量 | 共用固定 worker 池，取消后仍逐项报告结果 |
| 资产批处理 | 远端资产删除单独维护队列与结果通道 | 与缓存查询、缓存清理共用同一 worker 池 |
| 旧消息表示 | 未被构造的 Console 消息结构和对应类型分支 | 删除，统一处理实际使用的 map 消息 |

## 保留的差异

- Web SSO、Console DPoP、Build OAuth 的认证协议没有互相替换。
- 保留各端点原有状态码和重试语义：资源接口透传非 2xx；fallback 接受任意 2xx；资源 401 最多强刷一次；未增加创建视频的自动重放。
- 保留托管出口 fail-closed、账号隔离、工具别名、思考质量重试和当前回归测试。
- 取消后的额度刷新与 NSFW 批次仍报告每个条目的失败；资产清理不再调度剩余删除请求，未成功项计入失败。
- Build 身份头统一在取得有效 token 后构建，账单的 `x-userid` 与 `x-grok-user-id` 共用当前账号身份。

## 量化与验证

- `internal/grok` 顶层非测试 Go 源码：28,209 → 27,901 行，净减少 **308 行**。按物理行统计，包含空行；未将测试或文档算入生产代码精简。
- 生产文件仍为 76 个；本轮目标是删除重复执行逻辑，不是机械拼接文件。
- 本轮开始时已有的 56 个 Grok 回归测试文件 SHA-256 均未变化；新增 `unified_core_test.go`。
- 新增测试覆盖固定并发上限、恰好一次处理、取消汇报、等待 worker 完成、解压和租约释放、错误透传、fallback 方法与请求体、资源查询参数及 404 透传；连续运行三轮通过。
- `go test ./...`、`go vet ./...`、`git diff --check` 均通过。
- Linux amd64 / CGO disabled 的服务器交叉编译通过。
- Windows 当前 Go 为 386，未运行 race detector；未进行真实上游调用或吞吐量基准测试，因此不宣称性能提升或线上验证完成。

本轮未提交、推送或部署。
