# Grok2api 实现替换记录（2026-09-08）

本轮针对请求质量与 SSE/终态链路复核。上游基线为
`chenyme/grok2api@44a390b890e7a3e0dd209b95b8c29a9f2b1be8dd`，
已通过远端 HEAD 核对；不是一次覆盖所有媒体、存储、控制台功能的全量审计。

## 已替换与修复

| 差异/问题 | 现在的处理 |
| --- | --- |
| 本地独立质量算法、JSON/SSE 两套质量逻辑 | 直接移植上游 ClassifyQualityHold、密文比例门槛、burst 分类；JSON 与 SSE 共用 signals |
| 本地以最后一个 delta 减首个 delta 判断快速输出 | 改成上游从首个可见输出到当前判断时刻的 elapsed time |
| 使用 usage 覆盖已观察到的可见输出，终帧快照可能未计入 | 采用上游 max(delta、snapshot、usage-derived visible)，不重复相加 |
| 固定 4×hold 绝对计时器，持续有效输出也会被打断 | 移植 semanticIdleReadCloser；有效生成事件刷新，只在读取上游期间计时，保活/控制事件不刷新 |
| 三条链路使用不同 SSE 解析 | 移植 consumeCompatibleSSE，质量检查、readResponseSSE 调用方与原生转发共用；支持 BOM、CRLF、多行 data、event/id/retry/comment |
| 原生转发按单行检查终态，并在终态后继续等 EOF | 按完整帧检查终态，立即结束；提前 DONE 先输出 failure，再发且仅发一个 DONE |
| 原生审计仅按事件名判断成功 | 与 Chat 转换共用 responseTerminalFinish，优先检查 response.status；失败保持错误，异步 JSON 的 queued/in_progress 不冒充完成 |
| JSON 超出质量检查上限被静默截断 | 退出质量检查但拼回已读前缀与原始剩余流，保留完整响应 |
| 审计未处理短写、EOF 无换行及多行累计上限 | 识别 io.ErrShortWrite、处理末尾未换行 data、限制完整帧累计大小 |

## 删除范围

- 删除旧 JSON 递归证据路径：qualityPayloadNeedsRetryWithMin、visibleTextLength、payloadHasThinkingEvidence、valueHasThinkingEvidence、sseData。
- 删除 qualitySignals.verdict 内自研判定分支；该方法只做配置单位与空输出适配，调用上游分类器。
- 删除本地质量帧扫描循环及 readResponseSSE 内旧扫描器，替换为上游 codec。
- 删除原生逐行转发/解析路径，以及 nativeResponseTerminalFromSSELine、nativeResponseLoopDeltas、responseIDFromSSELine、已无调用的 boundedResponseCapture.Reset。
- 当前回归测试保留。依赖已删除内部函数的测试迁移到当前 signals/真实转发入口；新增上游原有分类器与语义空闲测试，以及集成回归。

这些删除均为源码局部修改，可从工作区 diff/版本历史恢复；未删除测试目录、线上数据或部署文件。

## 仍保留的适配与差异

- 账号池、Redis、请求所有权、路由与审计仍用本项目结构，没有把上游整个 backend 服务并入。
- 质量开关仍默认关闭；配置仍以可见字符数表示，在边界转换为约四字符一个 token。现有 hold/重试次数/耗尽策略不擅自改默认值。
- 本地质量检查缓冲上限仍为 8 MiB，上游 peek 是 4 MiB；单 SSE 事件上限双方为 8 MiB。超限 JSON 跳过质量分类，但不跳过后续协议处理。
- 本地保留可读 reasoning 最终快照与 refusal 的兼容处理。未照搬上游 quality_retry_scan 的逐行 JSON 观察器，因为它不能正确处理多行 data。
- 本地仍保守地对所有含工具的请求跳过质量重试；上游将“是否检查质量”和“能否跨账号重放”分开，仅允许安全的客户端工具重放。此处仍有策略差异，不能直接放开以免重复执行 hosted 工具。
- 质量门控接入语义空闲包装器后，释放到客户端的余流继续使用它；未宣称所有不经过质量门控的请求都已使用同一空闲策略。
- SSE 使用上游 codec 规范化字段顺序与换行，不承诺原始字节级透传。
- 本轮未重审媒体、模型聚合、关系数据库或管理界面等其余差异，不宣称完全等价。

## 来源与验证

上游 MIT 许可、版本及逐文件映射见根目录 THIRD_PARTY_NOTICES.md 与 licenses/grok2api-MIT.txt。

验证使用本地合成输入，不调用真实付费账号；不包含 us1 线上复测。
本轮未提交、推送 GitHub 或部署。

- 上游分类器/语义空闲测试及本轮集成回归连续运行 3 次通过。
- `go test ./... -count=1` 通过。
- `go vet ./...` 通过。
- Linux amd64、CGO=0 的 server 构建通过，产物仅放在系统临时目录。
- `git diff --check`（仓库现有换行配置）通过。
- 已逐函数核对 encryptedThinkingFloor、qualityIsBurstDump、ClassifyQualityHold，除格式外与锁定的上游源码相同。
- 当前环境为 Windows/386 Go，未执行需要 C 工具链的 race 检测。
