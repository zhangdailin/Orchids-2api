# 配置说明

## 1. 加载规则

配置加载顺序：

1. 启动参数 `-config` 指定文件
2. 若未指定，则按顺序查找 `config.json` -> `config.yaml` -> `config.yml`
3. 读取文件并应用默认值
4. 若 Redis 中存在 `settings:config`，则以 Redis 中保存的配置覆盖文件配置
5. 最后始终执行 `config.ApplyHardcoded()`，把一批运行时固定值重新写入

说明：

- YAML 仅支持扁平 `key: value`
- 不是所有历史字段都还能通过配置文件生效

## 2. 配置文件可设置的字段

下面这些字段会从配置文件或管理接口持久化到 Redis。

### 2.1 服务与管理端

| 字段 | 默认值 | 说明 |
|---|---|---|
| `port` | `3002` | 服务监听端口 |
| `debug_enabled` | `false` | 打开调试日志 |
| `verbose_diagnostics` | `false` | 详细诊断日志 |
| `admin_user` | `admin` | 管理端用户名 |
| `admin_pass` | 自动生成 | 管理端密码，建议显式设置 |
| `admin_path` | `/admin` | 管理端路径 |
| `admin_token` | 空 | 管理端静态 token |

### 2.2 Redis

| 字段 | 默认值 | 说明 |
|---|---|---|
| `store_mode` | `redis` | 当前仅支持 `redis` |
| `redis_addr` | 空 | Redis 地址，例如 `127.0.0.1:6379` |
| `redis_password` | 空 | Redis 密码 |
| `redis_db` | `0` | Redis DB |
| `redis_prefix` | `orchids:` | Redis key 前缀 |

### 2.3 缓存

| 字段 | 默认值 | 说明 |
|---|---|---|
| `cache_token_count` | `false` | 是否缓存 token 计数 |
| `cache_ttl` | `5` | 通用缓存 TTL（分钟） |
| `cache_strategy` | `mix` | 缓存策略 |
| `enable_token_cache` | `false` | 是否启用 token cache |
| `token_cache_ttl` | `300` | token cache TTL（秒） |
| `token_cache_strategy` | `1` | token cache 策略 |

### 2.4 代理

| 字段 | 默认值 | 说明 |
|---|---|---|
| `proxy_http` | 空 | HTTP 代理 |
| `proxy_https` | 空 | HTTPS 代理 |
| `proxy_user` | 空 | 代理用户名 |
| `proxy_pass` | 空 | 代理密码 |
| `proxy_bypass` | 空数组 | 直连域名或网段 |

## 3. 运行时硬编码默认值

这些值由 [config.go](/D:/Code/Orchids-2api/internal/config/config.go) 里的 `ApplyHardcoded()` 强制覆盖，不能指望仅靠配置文件改变。

| 字段 | 当前值 | 说明 |
|---|---|---|
| `output_token_mode` | `final` | 输出 token 统计模式 |
| `context_max_tokens` | `100000` | 上下文上限 |
| `context_summary_max_tokens` | `800` | 摘要上限 |
| `context_keep_turns` | `6` | 会话保留轮数 |
| `orchids_api_version` | `2` | Orchids API 版本 |
| `orchids_allow_run_command` | `true` | Orchids 允许命令工具 |
| `orchids_run_allowlist` | `["*"]` | Orchids 命令白名单 |
| `orchids_cc_entrypoint_mode` | `auto` | 入口模式 |
| `orchids_fs_ignore` | `["debug-logs","data",".claude"]` | 忽略目录 |
| `grok_api_base_url` | `https://grok.com` | Grok 基础地址 |
| `warp_disable_tools` | `false` | Warp 工具默认开启 |
| `warp_max_tool_results` | `10` | Warp 单轮工具结果上限 |
| `warp_max_history_messages` | `20` | Warp 历史消息上限 |
| `orchids_max_tool_results` | `10` | Orchids 单轮工具结果上限 |
| `orchids_max_history_messages` | `20` | Orchids 历史消息上限 |
| `stream` | `true` | Chat 默认流式 |
| `image_nsfw` | `true` | 公共 imagine 默认 NSFW 开启 |
| `public_enabled` | `true` | 公共页面默认开启 |
| `image_final_min_bytes` | `100000` | imagine 最终图阈值 |
| `image_medium_min_bytes` | `30000` | imagine 中间图阈值 |
| `max_retries` | `3` | 请求最大重试次数 |
| `retry_delay` | `1000` | 重试基准延迟（毫秒） |
| `request_timeout` | `600` | 请求超时（秒） |
| `retry_429_interval` | `60` | 429 重试间隔（秒） |
| `token_refresh_interval` | `1` | token 自动刷新间隔（分钟） |
| `auto_refresh_token` | `true` | 自动刷新账号 token |
| `load_balancer_cache_ttl` | `5` | 负载均衡缓存 TTL（秒） |
| `concurrency_limit` | `100` | 并发上限 |
| `concurrency_timeout` | `300` | 并发等待超时（秒） |
| `adaptive_timeout` | `true` | 自适应超时 |

## 4. 最小可用配置

```json
{
  "port": "3002",
  "store_mode": "redis",
  "redis_addr": "127.0.0.1:6379",
  "admin_user": "admin",
  "admin_pass": "change-me",
  "admin_path": "/admin",
  "debug_enabled": true
}
```

## 5. 配置保存入口

管理端有两套常用配置接口：

| 路径 | 方法 | 说明 |
|---|---|---|
| `/api/config` | GET/POST | 直接读取 / 覆盖整个配置对象 |
| `/api/config/list` | GET | 读取管理端表单配置 |
| `/api/config/save` | POST | 以 patch 方式保存管理端表单配置 |

## 6. 注意事项

- `admin_pass` 若留空，会在启动时自动生成随机密码并写日志
- 配置保存在 Redis 后，后续重启会优先使用 Redis 版本
- `data/tmp`、`debug-logs` 等目录是运行期产物，不是配置项
- 许多历史字段即使仍出现在旧配置里，也不会改变当前运行行为

## 7. 建议清理的历史字段

以下旧字段不建议继续保留在配置文件中：

- `summary_cache_*`
- `tool_call_mode`
- `warp_tool_call_mode`
- `disable_tool_filter`
- `orchids_impl`
