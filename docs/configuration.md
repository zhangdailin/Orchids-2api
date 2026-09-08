# 配置说明

## 1. 加载规则

首次运行先复制安全示例；生成的 `config.json` 不纳入 Git：

```bash
cp config.example.json config.json
```

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
| `inference_auth_enabled` | `true` | 模型和推理接口是否要求管理 API Key |
| `trusted_proxies` | `[]` | 允许提供转发头的反向代理 IP/CIDR；为空时忽略并剥离所有外部 `Forwarded`/`X-Forwarded-*`/`X-Real-IP` |
| `credential_encryption_key_file` | `data/credential.key` | Redis 账号凭据 AES-GCM 主密钥文件；只持久化路径，不持久化密钥内容 |
| `response_store_ttl_hours` | `720` | Build stored Response 账号归属记录的 Redis TTL（小时） |
| `grok_console_base_url` | `https://console.x.ai/v1` | Grok Console Responses、标准视频、TTS、STT 和 Realtime 的 DPoP 上游基址 |
| `grok_cli_fallback_base_url` | `https://api.x.ai/v1` | Build 视频主路由返回确认的 403 后使用的 XAI fallback 基址 |

### 2.2 Redis

| 字段 | 默认值 | 说明 |
|---|---|---|
| `store_mode` | `redis` | 当前仅支持 `redis` |
| `redis_addr` | 空 | Redis 地址，例如 `127.0.0.1:6379` |
| `redis_password` | 空 | Redis 密码 |
| `redis_db` | `0` | Redis DB |
| `redis_prefix` | `orchids:` | Redis key 前缀 |

### 2.3 媒体与多实例

| 字段 | 默认值 | 说明 |
|---|---|---|
| `media_dir` | `data/tmp` | 图片、视频成品和临时媒体输入目录；修改后需要重启 |
| `deployment_replicas` | `1` | 当前部署的服务副本数 |
| `deployment_instance_id` | 空 | 多副本时必填，并且每个副本必须使用不同的稳定值 |
| `deployment_cluster_id` | `orchids` | 同一 Redis 和共享媒体目录所属的集群标识 |
| `shared_media` | `false` | 多副本时必须显式设为 `true`，表示 `media_dir` 已挂载为所有副本共享的可读写目录 |

当 `deployment_replicas > 1` 时，启动检查要求使用 Redis、填写实例 ID 并确认共享媒体。服务会在媒体根目录创建 `.orchids-cluster` 标记，并执行临时文件写入与回读；目录属于其他集群、不可写或无法回读时会拒绝启动。所有副本应使用相同的 `deployment_cluster_id` 和媒体目录布局，但使用不同的 `deployment_instance_id`。

### 2.4 缓存

| 字段 | 默认值 | 说明 |
|---|---|---|
| `cache_token_count` | `false` | 是否缓存 token 计数 |
| `cache_ttl` | `5` | 通用缓存 TTL（分钟） |
| `cache_strategy` | `mix` | 缓存策略 |
| `enable_token_cache` | `false` | 是否启用 token cache |
| `token_cache_ttl` | `300` | token cache TTL（秒） |
| `token_cache_strategy` | `1` | token cache 策略 |

### 2.5 代理

| 字段 | 默认值 | 说明 |
|---|---|---|
| `proxy_http` | 空 | HTTP 代理 |
| `proxy_https` | 空 | HTTPS 代理 |
| `proxy_user` | 空 | 代理用户名 |
| `proxy_pass` | 空 | 代理密码 |
| `proxy_bypass` | 空数组 | 直连域名或网段 |

`grok_egress_enabled=true` 时，HTTP、SOCKS5/SOCKS5H 节点同时用于普通请求和 Console Voice WebSocket；节点选择、UA、Cloudflare cookies 与连接租约保持同一绑定。公网反向代理必须把自身地址加入 `trusted_proxies`，不要填写任意客户端可达的地址段。

### 2.6 上游保真

本网关是 API 中转站，默认**逐字透传**客户端内容、不对报文内容做任何改写。以下字段可显式开启历史改写行为：

| 字段 | 默认值 | 说明 |
|---|---|---|
| `cc_entrypoint_mode` | `keep` | `keep`=系统内容原样透传（默认）；`auto`=仅剥离 `claude-vscode`/`claude-code` 入口值；`strip`=剥离全部 `cc_entrypoint=` 并过滤 Claude Code 系统/环境项 |

这些字段会被持久化，不会被 `ApplyHardcoded()` 覆盖。

## 3. 运行时限制与固定默认值

### 3.1 可配置的重试、超时与 Grok 限速

以下字段可通过配置文件/Redis 持久化；`ApplyHardcoded()` 不再覆盖有效值。整数设置省略或小于等于 0 时使用默认值，超过上限时钳制到上限。配置修改后重启服务，使 HTTP 客户端与入口中间件一致更新。

| 字段 | 默认值 | 上限/含义 |
|---|---|---|
| `max_retries` | `3` | 单次 HTTP 请求最大重试次数，上限 20 |
| `retry_delay` | `1000` | 重试基准延迟，毫秒，上限 60000 |
| `account_switch_count` | `5` | Grok 账号尝试次数，包含首轮，上限 20；不再误作秒数 |
| `request_timeout` | `600` | 通用请求超时，秒，上限 86400 |
| `concurrency_timeout` | 跟随 `request_timeout` | 入口请求执行超时，秒，上限 86400；不是单纯排队等待时间 |
| `retry_429_interval` | `60` | 无精确 reset 信息时的 Web 429 重试间隔，秒，上限 3600 |
| `grok_web_timeout_seconds` | 跟随 `request_timeout` | Web HTTP 总超时，含响应体读取，上限 86400 秒 |
| `grok_console_timeout_seconds` | 跟随 `request_timeout` | Console HTTP 总超时，含响应体读取，上限 86400 秒 |
| `grok_build_timeout_seconds` | 跟随 `request_timeout` | Build HTTP 总超时，含响应体读取，上限 86400 秒 |
| `grok_stream_idle_seconds` | `120` | Build/Console SSE 有效输出空闲超时，上限 3600 秒；keepalive 不重置计时 |
| `grok_web_rps` / `grok_console_rps` / `grok_build_rps` | `0` | 0 关闭主动限速；正数按账号/团队限速，范围 0.01–1000，每个桶 burst=1 |

限流状态按 provider、账号/已知团队、模型隔离；真实 429 冷却不随主动限速关闭。优先使用 `Retry-After`，再使用响应中的 reset 信息；信息缺失时只冷却受影响的账号/模型，不再全局停顿。限流注册表为进程内状态，不是跨副本共享限流。

总超时与空闲超时是不同边界。长回答需要同时满足入口 `concurrency_timeout` 和目标 provider HTTP 超时。中转层已删除思考质量门控、额外质量重试和缺失思考惩罚；历史 `grok_quality_*` / `grok_missing_thinking_cooldown_seconds` 配置不再生效。存储会话仍保留账号绑定。

Grok 直连与托管 egress 均使用以上 provider 超时，不再受 egress 固定 120 秒总超时或共享客户端 HTTP/1 固定 120 秒响应头上限影响；等待响应头仍受 HTTP 总超时约束。其他模型继续使用原有共享客户端策略。

### 3.2 仍然固定的默认值

这些值由 [config.go](../internal/config/config.go) 里的 `ApplyHardcoded()` 强制覆盖，不能指望仅靠配置文件改变。

| 字段 | 当前值 | 说明 |
|---|---|---|
| `output_token_mode` | `final` | 输出 token 统计模式 |
| `context_max_tokens` | 不生效 | 旧兼容字段；不在中转层截断或自动压缩上下文 |
| `context_summary_max_tokens` | 不生效 | 旧兼容字段；不生成中转层摘要 |
| `context_keep_turns` | 不生效 | 旧兼容字段；不按轮数删除请求历史 |
| `grok_api_base_url` | `https://grok.com` | Grok 基础地址 |
| `warp_disable_tools` | `false` | Warp 工具默认开启 |
| `warp_max_tool_results` | `10` | Warp 单轮工具结果上限 |
| `warp_max_history_messages` | `20` | Warp 历史消息上限 |
| `stream` | `true` | Chat 默认流式 |
| `image_nsfw` | `true` | 公共 imagine 默认 NSFW 开启 |
| `public_enabled` | `true` | 公共页面默认开启 |
| `image_final_min_bytes` | `100000` | imagine 最终图阈值 |
| `image_medium_min_bytes` | `30000` | imagine 中间图阈值 |
| `token_refresh_interval` | `1` | token 自动刷新间隔（分钟） |
| `auto_refresh_token` | `true` | 自动刷新账号 token |
| `load_balancer_cache_ttl` | `5` | 负载均衡缓存 TTL（秒） |
| `concurrency_limit` | `100` | 并发上限 |
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
  "inference_auth_enabled": true,
  "credential_encryption_key_file": "data/credential.key",
  "response_store_ttl_hours": 720,
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
- 可用 `ORCHIDS_CREDENTIAL_ENCRYPTION_KEY` 提供 Base64、Hex 或 32 字节原始主密钥；环境变量优先于密钥文件
- 首次启动自动创建主密钥文件，并把已有账号明文凭据迁移为 `enc:v1:` 密文
- 主密钥不会写入 Redis 或管理 API；必须和 Redis 数据共同备份，切勿在已有账号后更换或删除
- `data/tmp`、`debug-logs` 等目录是运行期产物，不是配置项
- 许多历史字段即使仍出现在旧配置里，也不会改变当前运行行为

## 7. 建议清理的历史字段

以下旧字段不建议继续保留在配置文件中：

- `summary_cache_*`
- `tool_call_mode`
- `warp_tool_call_mode`
- `disable_tool_filter`
