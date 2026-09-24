# docs 索引

| 文档 | 内容 |
| --- | --- |
| `api-reference.md` | 对外推理 API、Grok Build OAuth 登录、工具页与管理端点 |
| `configuration.md` | 配置项、默认值、缓存、限速与安全含义 |
| `architecture.md` | 包结构、请求链路、模型发现与存储 |
| `deployment.md` | 部署、验证、监控与排障 |

Grok 支持面仅为 Build OAuth CLI：设备登录、Claude Messages、OpenAI Chat Completions、Responses 及 stored Response 子资源、模型发现、账单/限速状态，以及 `/api/grok/tools/v1/{models,responses}` 工具页。

## 脱敏约定

本仓库是 **public**，文档不写生产凭据与可识别的部署标识：

- 口令、Key、token、Cookie、邮箱一律不出现；需要说明时使用字段名或占位符。
- 生产主机与域名使用 `<PROD_IP>`、`<PROD_DOMAIN>` 等占位符。
- 本地专属且未纳入版本控制的路径不写进文档。
