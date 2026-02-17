# 配置说明

## 1. 加载规则

1. 启动参数 `-config` 指定配置文件（`.json` / `.yaml` / `.yml`）
2. 若未指定，按顺序查找：`config.json` -> `config.yaml` -> `config.yml`
3. 先读取文件，再应用默认值
4. 若 Redis 中存在 `settings: config`，会覆盖文件配置并再次应用默认值

说明：YAML 仅支持扁平 `key: value`（不支持嵌套对象）。

## 2. 核心配置项

### 2.1 服务与管理端

| 字段 | 默认值 | 说明 |
|---|---|---|
| `port` | `3002` | 服务监听端口 |
| `debug_enabled` | `false` | 开启调试日志与调试行为 |
| `debug_log_sse` | `false` | 记录 SSE 明细 |
| `admin_user` | `admin` | 管理端用户名 |
| `admin_pass` | `admin123` | 管理端密码 |
| `admin_path` | `/admin` | 管理界面路径 |
| `admin_token` | 空 | 管理 API 静态 token（可选） |

### 2.2 Redis 存储

| 字段 | 默认值 | 说明 |
|---|---|---|
| `store_mode` | `redis` | 当前仅支持 `redis` |
| `redis_addr` | 空 | Redis 地址，例如 `127.0.0.1:6379` |
| `redis_password` | 空 | Redis 密码 |
| `redis_db` | `0` | Redis DB |
| `redis_prefix` | `orchids:` | Key 前缀 |

### 2.3 请求与重试

| 字段 | 默认值 | 说明 |
|---|---|---|
| `max_retries` | `3` | 单请求最大重试次数 |
| `retry_delay` | `1000` | 重试基准延迟（毫秒） |
| `request_timeout` | `600` | 请求超时（秒） |
| `retry_429_interval` | `60` | 429 重试间隔（秒） |
| `account_switch_count` | `5` | 账号切换上限（历史兼容字段） |
| `concurrency_limit` | `100` | 并发上限 |
| `concurrency_timeout` | `300` | 并发等待超时（秒） |
| `adaptive_timeout` | `false` | 自适应超时 |

### 2.4 Token/缓存

| 字段 | 默认值 | 说明 |
|---|---|---|
| `auto_refresh_token` | `false` | 是否自动刷新账号 token |
| `token_refresh_interval` | `1` | 自动刷新间隔（分钟） |
| `output_token_mode` | `final` | 输出 token 统计策略 |
| `output_token_count` | `false` | 是否输出 token 数 |
| `cache_token_count` | `false` | 是否缓存 token 计数 |
| `cache_ttl` | `5` | token 缓存 TTL（分钟） |
| `cache_strategy` | `mixed` | 上下文裁剪策略 |
| `load_balancer_cache_ttl` | `5` | 负载均衡缓存 TTL（秒） |

### 2.5 上下文

| 字段 | 默认值 | 说明 |
|---|---|---|
| `context_max_tokens` | `8000` | 上下文 token 上限 |
| `context_summary_max_tokens` | `800` | 摘要 token 上限 |
| `context_keep_turns` | `6` | 会话保留轮数 |
| `suppress_thinking` | `false` | 抑制 thinking 输出 |

## 3. 通道相关配置

### 3.1 Orchids

| 字段 | 默认值 | 说明 |
|---|---|---|
| `upstream_mode` | `sse` | 上游模式（历史字段） |
| `orchids_api_base_url` | 代码内默认值 | Orchids API 地址 |
| `orchids_ws_url` | 代码内默认值 | Orchids WS 地址 |
| `orchids_api_version` | `2` | Orchids API 版本 |
| `orchids_allow_run_command` | `false` | 是否允许 run_command |
| `orchids_run_allowlist` | `["pwd","ls","find"]` | run_command 白名单 |
| `orchids_cc_entrypoint_mode` | `auto` | cc_entrypoint 处理策略 |
| `orchids_fs_ignore` | `["debug-logs","data",".claude"]` | 忽略路径段 |
| `orchids_max_tool_results` | `10` | 每轮工具结果上限 |
| `orchids_max_history_messages` | `20` | 历史消息上限 |

### 3.2 Warp

| 字段 | 默认值 | 说明 |
|---|---|---|
| `warp_disable_tools` | `false` | 是否禁用 Warp 工具 |
| `warp_max_tool_results` | `10` | 每轮工具结果上限 |
| `warp_max_history_messages` | `20` | 历史消息上限 |
| `warp_split_tool_results` | `false` | 是否拆分工具结果分批发送 |

### 3.3 Grok

| 字段 | 默认值 | 说明 |
|---|---|---|
| `grok_api_base_url` | `https://grok.com` | Grok 站点地址 |
| `grok_user_agent` | 代码内默认 UA | 请求 UA |
| `grok_cf_clearance` | 空 | Cloudflare clearance（可选） |
| `grok_cf_bm` | 空 | Cloudflare bm（可选） |
| `grok_base_proxy_url` | 空 | Grok 基础请求代理 |
| `grok_asset_proxy_url` | 空 | Grok 资源代理 |
| `grok_use_utls` | `false` | 是否启用 uTLS |

### 3.4 Grok2API 兼容键

| 字段 | 默认值 | 说明 |
|---|---|---|
| `stream` | `true` | chat/completions 默认是否流式 |
| `app_stream` | 空 | 覆盖 `stream` |
| `app.stream` | 空 | 最高优先级覆盖 `app_stream/stream` |
| `image_nsfw` | `true` | public imagine 默认 NSFW 开关 |
| `image.nsfw` | 空 | 覆盖 `image_nsfw` |
| `image_final_min_bytes` | `100000` | public imagine 最终图过滤阈值 |
| `image.final_min_bytes` | 空 | 覆盖 `image_final_min_bytes` |
| `image_medium_min_bytes` | `30000` | public imagine 中等图过滤阈值 |
| `image.medium_min_bytes` | 空 | 覆盖 `image_medium_min_bytes` |
| `public_key` | 空 | public API 鉴权 key；为空时不做 public API 鉴权 |
| `app_public_key` | 空 | 覆盖 `public_key` |
| `app.public_key` | 空 | 最高优先级覆盖 `app_public_key/public_key` |
| `public_enabled` | `false` | 是否公开 `/login` `/imagine` `/voice` `/video` 页面（仅页面开关） |
| `app_public_enabled` | 空 | 覆盖 `public_enabled` |
| `app.public_enabled` | 空 | 最高优先级覆盖 `app_public_enabled/public_enabled` |

## 4. 代理配置

| 字段 | 默认值 | 说明 |
|---|---|---|
| `proxy_http` | 空 | HTTP 代理 |
| `proxy_https` | 空 | HTTPS 代理 |
| `proxy_user` | 空 | 代理用户名 |
| `proxy_pass` | 空 | 代理密码 |
| `proxy_bypass` | 空数组 | 直连域名/网段 |

## 5. 自动注册（可选）

| 字段 | 默认值 | 说明 |
|---|---|---|
| `auto_reg_enabled` | `false` | 启用自动注册 |
| `auto_reg_threshold` | `5` | 阈值 |
| `auto_reg_script` | `scripts/autoreg.py` | 注册脚本路径 |

## 6. 账号默认值字段（可选）

这些字段一般用于单账号直配或初始化：

- `session_id`
- `client_cookie`
- `session_cookie`
- `client_uat`
- `project_id`
- `user_id`
- `agent_mode`
- `email`

## 7. 最小可用配置示例

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

## 8. 已废弃/忽略字段说明

下列旧字段可能仍出现在历史 `config.json` 中，但当前版本不会实际生效或已被替代：

- `summary_cache_*`（摘要缓存相关）
- `tool_call_mode`、`warp_tool_call_mode`、`disable_tool_filter`
- `orchids_impl`

建议清理旧字段，避免误判配置是否生效。
