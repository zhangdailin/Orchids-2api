# 配置说明

## 配置文件

应用只读取配置文件（不读取环境变量）。默认查找当前目录的 `config.json` / `config.yaml` / `config.yml`，也可以用 `-config` 指定路径。YAML 仅支持扁平的 `key: value` 结构，不支持嵌套。

## 配置项

| 变量名 | 默认值 | 描述 |
|--------|--------|------|
| `port` | 3002 | 服务端口 |
| `debug_enabled` | false | 启用调试日志 |
| `admin_user` | admin | 管理员用户名 |
| `admin_pass` | admin123 | 管理员密码 |
| `admin_path` | /admin | 管理界面路径 |
| `store_mode` | redis | 存储模式（仅支持 redis） |
| `redis_addr` |  | Redis 地址（如 127.0.0.1:6379） |
| `redis_password` |  | Redis 密码 |
| `redis_db` | 0 | Redis DB |
| `redis_prefix` | orchids: | Redis key 前缀 |
| `summary_cache_mode` | redis | 会话摘要缓存模式（memory/redis/off） |
| `summary_cache_size` | 256 | 内存摘要缓存容量 |
| `summary_cache_ttl_seconds` | 3600 | 摘要缓存 TTL（秒） |
| `summary_cache_log` | true | 输出摘要缓存命中日志 |
| `summary_cache_redis_addr` |  | 摘要缓存 Redis 地址 |
| `summary_cache_redis_password` |  | 摘要缓存 Redis 密码 |
| `summary_cache_redis_db` | 0 | 摘要缓存 Redis DB |
| `summary_cache_redis_prefix` | orchids:summary: | 摘要缓存 Redis key 前缀 |
| `output_token_mode` | final | 输出 Token 统计模式 |
| `context_max_tokens` | 8000 | 最大上下文 Tokens |
| `context_summary_max_tokens` | 800 | 摘要最大 Tokens |
| `context_keep_turns` | 6 | 保留最近对话轮数 |
| `upstream_url` |  | 上游 API 地址（可选） |
| `upstream_token` |  | 上游 token（可选） |
| `upstream_mode` | sse | 上游模式（sse/ws） |
| `orchids_api_base_url` | https://orchids-server.calmstone-6964e08a.westeurope.azurecontainerapps.io | Orchids API Base URL |
| `orchids_ws_url` | wss://orchids-v2-alpha-108292236521.europe-west1.run.app/agent/ws/coding-agent | Orchids WebSocket URL |
| `orchids_api_version` | 2 | Orchids API 版本 |
| `orchids_local_workdir` |  | 本地工作目录（WS 模式下用于 fs_operation） |
| `orchids_allow_run_command` | false | 是否允许 Orchids run_command |
| `orchids_run_allowlist` | ["pwd","ls","find"] | run_command 允许的命令白名单 |
| `orchids_cc_entrypoint_mode` | auto | 系统提示 cc_entrypoint 处理策略（auto/keep/strip） |
| `orchids_fs_ignore` | ["debug-logs","data",".claude"] | 忽略的路径段 |
| `session_id` |  | 默认账号 Session ID（可选） |
| `client_cookie` |  | 默认账号 Cookie（可选） |
| `client_uat` |  | 默认账号 UAT（可选） |
| `project_id` |  | 默认账号项目 ID（可选） |
| `user_id` |  | 默认账号 User ID（可选） |
| `agent_mode` |  | 默认账号 Agent Mode（可选） |
| `email` |  | 默认账号 Email（可选） |

## 示例

`config.json`
```json
{
  "port": "3002",
  "store_mode": "redis",
  "redis_addr": "127.0.0.1:6379",
  "redis_db": 0,
  "redis_prefix": "orchids:",
  "admin_user": "admin",
  "admin_pass": "admin123",
  "admin_path": "/admin",
  "upstream_mode": "ws",
  "orchids_ws_url": "wss://orchids-v2-alpha-108292236521.europe-west1.run.app/agent/ws/coding-agent",
  "orchids_local_workdir": "/path/to/project",
  "orchids_allow_run_command": false,
  "orchids_run_allowlist": ["pwd","ls","find"],
  "orchids_cc_entrypoint_mode": "auto",
  "orchids_fs_ignore": ["debug-logs","data",".claude"],
  "summary_cache_mode": "redis",
  "summary_cache_redis_addr": "127.0.0.1:6379"
}
```

`config.yaml`
```yaml
port: "3002"
store_mode: "redis"
redis_addr: "127.0.0.1:6379"
redis_db: 0
redis_prefix: "orchids:"
admin_user: "admin"
admin_pass: "admin123"
admin_path: "/admin"
upstream_mode: "ws"
orchids_ws_url: "wss://orchids-v2-alpha-108292236521.europe-west1.run.app/agent/ws/coding-agent"
orchids_local_workdir: "/path/to/project"
orchids_allow_run_command: false
orchids_run_allowlist: ["pwd","ls","find"]
orchids_cc_entrypoint_mode: "auto"
orchids_fs_ignore: ["debug-logs","data",".claude"]
summary_cache_mode: "redis"
summary_cache_redis_addr: "127.0.0.1:6379"
```

## 安全建议

- 生产环境务必修改 `admin_user` 和 `admin_pass`
- 使用随机字符串作为 `admin_path`
- 不要将 `config.json` / `config.yaml` 提交到版本控制
