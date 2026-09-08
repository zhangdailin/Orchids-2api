# Puter 官方网页登录试验

账号管理 → Puter → 添加账号 →「使用 Puter 官方网页登录」。手填 Token 方式保留。

实现基于官方 Puter.js 的网页弹窗协议，不是标准 OAuth 授权码/PKCE：

- 打开 `https://puter.com/action/sign-in`，请求显式授权。
- 接收 `puter.token` 消息，严格验证官方 origin、弹窗 source 和本轮随机 `msg_id`。
- 支持官方 `requestOrigin` / `originResponse` 握手；不使用 localhost 回调，不加载第三方 SDK 到管理页。
- 管理会话内通过同源 JSON POST `/api/puter/web-login` 递交授权结果。
- 后端访问官方 `/whoami` 和 `/metering/usage`，二者成功后才创建账号。身份取自上游，不信任前端用户名。
- 使用现有 Puter 账号凭据存储和聊天客户端；本次接口响应只含账号 ID，不回显 Token。现有管理列表/导出的凭据展示规则未改变。
- 不申请完整账号会话，不接收密码，不将 Token 放入回调 URL、localStorage 或日志。

部署要求：远程管理站点必须使用 HTTPS，反向代理保留原始 Host，浏览器允许弹窗。
不支持切断跨源 opener 的隔离页面；此时会提示错误，不能退回不校验来源的消息方式。
授权五分钟超时；取消、关闭弹窗、伪造来源和重复消息均有回归覆盖。

## 验证边界

本地回归覆盖授权消息、服务端验证、凭据去重、失败不入库、错误脱敏、同源限制和现有账号表单。
尚需用户在真实 Puter 官方弹窗完成授权，再验证一条实际聊天请求。
身份和额度校验不产生 AI 推理费用，也不保证所有模型可调用或账号未来持续有效。
浏览器返回的是站点应用级授权，受 Puter 对该应用的权限和额度约束；不能视为完整账号 Token。

测试命令：

```text
go test ./internal/puter ./internal/api ./cmd/server
node --test web/accounts_ui.test.cjs web/puter_auth.test.cjs
```

官方参考：

- [Puter.js Auth 源码](https://github.com/HeyPuter/puter/blob/main/src/puter-js/src/modules/Auth.js)
- [官方网页授权与后端 Token 登录说明](https://developer.puter.com/tutorials/puter-js-node-js/)
- [身份接口](https://github.com/HeyPuter/puter/blob/main/extensions/whoami.ts)
- [额度接口](https://github.com/HeyPuter/puter/blob/main/extensions/metering.ts)
