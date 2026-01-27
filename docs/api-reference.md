# API 接口文档

## 端点概览

| 端点 | 方法 | 描述 | 认证 |
|------|------|------|------|
| `/v1/messages` | POST | Claude API 代理端点 | 无 |
| `/api/accounts` | GET | 获取所有账号列表 | Basic Auth |
| `/api/accounts` | POST | 创建新账号 | Basic Auth |
| `/api/accounts/{id}` | GET | 获取单个账号 | Basic Auth |
| `/api/accounts/{id}` | PUT | 更新账号 | Basic Auth |
| `/api/accounts/{id}` | DELETE | 删除账号 | Basic Auth |
| `/api/export` | GET | 导出账号数据 (JSON) | Basic Auth |
| `/api/import` | POST | 导入账号数据 (JSON) | Basic Auth |
| `/health` | GET | 健康检查 | 无 |
| `{ADMIN_PATH}/*` | GET | 管理界面 | Basic Auth |

## 认证

### 管理接口认证

- **类型**: HTTP Basic Authentication
- **保护端点**: `/api/*`, 管理界面
- **凭据**: `ADMIN_USER` / `ADMIN_PASS`

### 上游 API 认证

- **类型**: Bearer Token (JWT)
- **流程**:
  1. 使用账号的 ClientCookie 调用 Clerk API
  2. 获取 SessionID
  3. 生成 JWT Token
  4. 作为 Authorization Header 发送到上游

## /v1/messages 端点

### 请求格式

兼容 Claude API 格式：

```json
{
  "model": "claude-opus-4-5-20251001",
  "messages": [
    {
      "role": "user",
      "content": "Hello"
    }
  ],
  "stream": true,
  "tools": []
}
```

### 响应格式

- `stream: true`：SSE 流式响应，兼容 Claude/Anthropic Messages 流式格式  
- `stream: false`：返回 Anthropic Messages 非流式 JSON（`type: "message"`，`content` 数组，`stop_reason`，`usage`）

### 模型映射

| 请求模型 | 上游模型 |
|----------|----------|
| `claude-opus-4-5-*` | `claude-opus-4.5` |
| `claude-haiku-4-5-*` | `gemini-3-flash` |
