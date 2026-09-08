# Grok 冗余分析与精简（2026-09-08）

## 本轮结果

统计基线为本轮开始时的工作区，包含此前尚未提交的修复，不是与 Git HEAD 的累计差异。
范围为 `internal/grok/*.go`，排除 `*_test.go`，不包含 egress 子包。

| 生产源码 | 修改前行数 | 修改后行数 | 处理 |
| --- | ---: | ---: | --- |
| responses_alias.go | 279 | 220 | 删除旧分块/单行 data 解析和仅包一层的状态类型，使用共享 SSE codec |
| session_state.go | 317 | 308 | 删除反向逐行解析与任意对象递归搜索，复用 SSE/协议 reasoning 提取 |
| native_outcome.go | 159 | 0 | 删除独立审计 writer、第二份 SSE 缓冲/解析及短写检测 |
| handler_responses_store.go | 425 | 449 | 同一次解码同时完成转发、终态、usage 和首 token 审计，统一错误返回 |
| handler_messages.go | 1156 | 1116 | 提取共用内部 Chat 流管道 |
| handler_responses.go | 844 | 830 | 使用同一管道生命周期管理 |
| chat_stream_bridge.go | 0 | 51 | 集中管理状态、响应头快照、管道、取消与关闭 |
| 本包全部生产源码 | 28323 | 28117 | **净减少 206 行** |

没有通过压缩空白、改成难读的短变量名或删除测试来制造行数下降；所有修改后 Go 文件均经过 gofmt。

## 深入分析：为什么合并这些路径

1. **工具别名解析与主协议解析重复且行为不一致。** 原实现使用 ReadString 构造无整体上限的 block，只取第一条 data 行。主链路已有完整 SSE codec，继续维护第二套分块没有收益。现在保留 namespace/tool_search 的业务转换，复用上游 codec 的 BOM、CRLF、多行 data 和大小限制。
2. **原生转发与审计重复解码。** 原来转发函数已经解码事件，写入 nativeOutcomeWriter 后又重新累计 SSE、解析 JSON、提取 usage。现在直接复用转发处的事件对象，并将 chatOutcome 返回给调用方。全响应回放缓冲超过 8 MiB 后，逐事件审计仍可获得最后的 usage。
3. **两个公开协议重复创建内部 Chat 请求管道。** Messages 与 Responses 均自行建立 context、io.Pipe、goroutine、状态通知及关闭逻辑。现在共用 withChatStream，保留各自错误协议和响应转换。
4. **思考回放混用了通用递归与协议解析。** 旧代码反向遍历文本行，再递归搜索任何 reasoning 对象；这会漏掉多行 SSE，也可能取到工具参数或 metadata 中伪装的对象。现在只认 response.output、event.item 等协议位置，并按出现顺序保留最近的有效签名。
5. 对本包普通私有函数进行了名称引用候选扫描；合并后发现的 responseIDFromJSON 已确认无调用并删除。引用计数不是完整死代码证明，不能据此删除接口方法、导出入口或把测试覆盖到的生产逻辑视为冗余。

## 合并时一并修复的边界

- tool_search 的 output_item.done 再次携带工具名时，不再清空前面累计的参数；同时兼容 item_id 和 call_id 定位。
- 工具搜索参数累计超过 8 MiB 会失败，避免把单帧限制绕成无限累计。
- 多行 SSE 可以正确恢复命名空间工具名和 opaque reasoning 签名。
- 原生 JSON 在有效 JSON 后发生传输读取错误时，不再仅因前缀可以解析而审计为成功。
- 内部 Chat 的响应头在 WriteHeader 时克隆，后续修改不改变已提交的响应。
- Responses 的内部流等待与 Messages 一样监听请求取消，不再无条件等待 ready。

## 有意没有合并的部分

- Build OAuth、Console SSO、Web Cookie 的凭据提取、账号可用性与能力过滤有实际差异，保留 provider 分支。
- Chat、Messages、Responses 的终态、工具块、签名和引用结构不同，只合并公共 IO/解析，不把三种输出协议强行合成一套分支众多的万能转换器。
- Web Chat 的增量 JSON 编码器和媒体/上传/重试状态机仍有生产调用，不因文件大或有专门测试就删除。
- 上轮移植的质量分类器和语义空闲实现保持不变，以便继续与 grok2api 对照。
- 本轮不是整个 Grok 子系统的重写，也未做吞吐/延迟基准对照；减少重复解码不等于已测得某个性能提升比例。

## 验证与恢复

- 原有 54 个 Grok 测试文件全部保留，其中 4 个文件迁移到合并后的生产入口，其原有 Test 函数未删除；其余文件哈希不变。
- 新增 refactor_regression_test.go，包含 9 项回归测试。
- 本轮精简与相关上轮回归定向测试连续运行 3 次通过。
- go test ./... -count=1、go vet ./...、git diff --check 通过。
- Linux amd64、CGO=0 的 server 构建通过，产物在系统临时目录。
- 当前 Windows/386 环境未执行 race 检测；未调用真实付费账号或服务器。

native_outcome.go 在本轮开始时尚未纳入 Git，因此另存了删除前的临时备份：
`C:/Users/zhangdailin/AppData/Local/Temp/orchids-grok-refactor-20260908-7c293b1e/native_outcome.go.bak`。
该备份位于仓库之外，不参与编译、提交或部署，可在临时目录被清理前恢复。

本轮未提交、推送 GitHub 或部署 us1。
