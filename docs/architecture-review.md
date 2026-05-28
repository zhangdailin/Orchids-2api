# 架构复核

本文件是 2026-03-21 的当前实现复核，不再保留早期三通道时代的历史判断。

## 1. 当前架构结论

Orchids-2api 目前是四通道结构：

- `internal/handler` 统一处理 `orchids`、`warp`、`puter`
- `internal/grok` 单独处理 `grok`
- [routes.go](/D:/Code/Orchids-2api/cmd/server/routes.go) 负责统一注册公开、管理、兼容别名和 public/admin 路由
- Redis 不只保存账号和模型，也承载了会话、去重、审计和 cache 的运行时状态
- 模型刷新走 [model_refresh.go](/D:/Code/Orchids-2api/cmd/server/model_refresh.go) 的来源同步逻辑，不再逐个模型测活

整体上，这已经不是“只能继续硬编码加通道”的状态了。`provider` 注册表已经接入主流程，扩新通道的成本明显低于旧版本。

## 2. 已经完成的关键改进

相对旧版复核，下面这些点已经不是问题或已明显缓解：

- 不再有固定默认密码 `admin123`，`admin_pass` 为空时会自动生成随机密码
- 全局安全响应头已经挂在中间件链上
- 管理认证中的敏感值比较已经改成常量时间比较
- handler 的会话存储、去重存储、审计日志在 Redis 可用时都能落到 Redis
- 账号 client 创建已经通过 `internal/provider` 做注册表派发，而不是全部散落在 handler 里硬编码分支
- Puter 非流式 `tool_use` / `tool_result` 回归已经固化为集成测试

## 3. 当前仍然成立的主要风险

### 3.1 配置层仍然偏“半硬编码”

[config.go](/D:/Code/Orchids-2api/internal/config/config.go) 里 `ApplyHardcoded()` 仍会强制覆盖大量运行时值。结果是：

- 文档需要明确区分“可配置字段”和“最终固定字段”
- 运维会误以为某些历史配置项还能生效
- 后续若要做热更新或按环境差异化部署，约束会比较大

这是当前最明显的“可维护性风险”，不是功能 bug，但会持续影响部署和排障。

### 3.2 管理端 session 仍是进程内存

[auth.go](/D:/Code/Orchids-2api/internal/auth/auth.go) 里的 `session_token` 仍保存在进程内 map 中。当前影响：

- 单实例没问题
- 进程重启后 session 失效
- 多实例部署时 session 不能共享

请求处理链路的会话状态已经可以走 Redis，但管理登录态还没有同步过去。

### 3.3 超大文件仍然是主要维护成本

当前最明显的维护热点仍是：

- `internal/grok/handler.go`
- `internal/grok/client.go`
- `internal/api/api.go`
- `internal/handler/stream_handler.go`

这些文件承担的职责都偏多。它们不是立刻导致错误的点，但会持续降低修改速度，也更容易把回归风险藏进去。

### 3.4 `/metrics` 仍默认公开

主路由里 `/metrics` 直接公开挂载。对内网部署问题不大，但放公网时建议交给外层网关或访问控制，不建议裸露。

### 3.5 Public API 的“空 key 即放开”语义需要被明确理解

[session.go](/D:/Code/Orchids-2api/internal/middleware/session.go) 当前约定是：

- `public_key` 为空时，public API 不做鉴权
- 页面是否显示由 `public_enabled` 控制

这套语义是明确实现出来的，不是偶然行为；但运维如果没看文档，容易误判“页面关闭了就等于 API 也关了”。

## 4. 当前架构的优点

- 路由已经集中在 `routes.go`，比早期 `main.go` 内联注册清晰很多
- handler 与 grok 拆成两条主链路，职责边界清楚
- provider registry 已经把新通道接入成本压下来了
- 模型刷新逻辑变成“来源驱动同步”，行为更可预测，也更适合管理端自动删除
- Puter 的真实工具链回归已经进入测试，不再完全依赖人工点点点验证

## 5. 建议的下一轮清理顺序

1. 把管理端 session 迁到 Redis，补齐多实例一致性
2. 继续压缩 `ApplyHardcoded()`，把真正需要可调的运维项放回配置层
3. 先拆 `internal/grok/handler.go` 和 `internal/api/api.go`
4. 给 `/metrics` 增加外层保护策略说明，至少在部署文档里明确
5. 保持 Puter / Grok 这两条易回归链路的集成测试继续扩充

## 6. 结论

当前架构已经从“快速堆功能”进入“可以持续维护但仍有局部技术债”的阶段。

最值得继续优化的不是再加一层抽象，而是三件具体的事：

- 收敛硬编码配置
- 迁移管理 session
- 拆大文件
