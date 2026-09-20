# 事故记录：2026-09-20 两条 503（workbuddy 池空 + 面板 CPU 保护）

调查对象：`<PROD_IP>`（t2.small / Ubuntu 24.04，`<PROD_HOSTNAME>`，`<PROD_DOMAIN>`）。
结论：两条报错**来自两层不同的服务**，只有第 1 条出在 `<PROD_IP>`；第 1 条的根因是
**容量耗尽**，以及**初始选号把"池子临时不可用"报成了 503 服务器故障**。

## 0. 线上拓扑（实测）

```
客户端 (DSH provider=<PROVIDER>, baseURL https://<PANEL_DOMAIN>/)
   │
   ▼
New API 面板「<PANEL_NAME>」 v1.0.0-rc.37
  <PANEL_DOMAIN> → <PANEL_IP>:3000   （<CLOUD_REGION>，<CLOUD_ASN>）
   │   匿名白名单调用方（orchids 配置 anonymous_allow_ips）
   ▼
Cloudflare → Caddy (443) → orchids-2api 127.0.0.1:3002   ← <PROD_IP>
   │
   ▼
上游 www.workbuddy.ai / cli-chat-proxy.grok.com / …
```

- `<PROD_IP>` 上只有 `orchids-2api`(`:3002`)、`caddy`、`redis-server`；无 new-api 进程与文件。
- `<PANEL_IP>` = `<PANEL_DOMAIN>`（DNS 实测），其 `:3000/api/status` 返回 New API 的 `system_name/server_address/version`。

## 1. 错误 A — `no enabled accounts available for channel: workbuddy`

`{"error":{"type":"<nil>","message":"no enabled accounts available for channel: workbuddy (request id: 202609200728413268520218268d9d6ENSTeIjy)"},"type":"error"}`

内层 message 是 **orchids 产生的**（`internal/loadbalancer/loadbalancer.go`），`(request id: …)` 后缀是
**New API 加上去的**（其 `common/utils.go:294`：`fmt.Sprintf("%s (request id: %s)", …)`）。

### 1.1 时间线（journal，UTC）

| 时间 | 事件 |
| --- | --- |
| 07:05:45 | 上游 `workbuddy API error: status=429, code=14003, message=too many requests` |
| 07:28:03 / 07:28:09 | 同一 trace 连续两次 429（`code=14003`）→ 写入**模型级**冷却 |
| 07:28:09 | `No more accounts available` → `no enabled accounts available for channel: workbuddy` |
| 07:28:18 – 07:32:40 | **8 次 503**（`/workbuddy/v1/chat/completions`，来源全是 `<PANEL_IP>`，`Go-http-client/2.0`） |
| 07:28:28 | critical 告警 `pool-empty:workbuddy`：“7 个启用账号全部处于冷却/异常状态” |
| 07:33:28 | 告警自愈（模型级冷却到期） |

### 1.2 池子为什么是空的：容量耗尽（不是坏状态）

7 个 workbuddy 账号里，4 个（`167/168/181/182`）带着**真实上游拒绝**留下的账号级 402：

```
status_code=402  quota_reset_at=2026-09-26/27
status_message=workbuddy API error: status=429,
  message={"error":{"data":{"code":14018,"msg":"Credits exhausted. Please visit the link
  below to purchase add-on packs and get more credits: …"}}}
last_attempt=2026-09-17T16:00:0x（四个号在同一秒，一次批量请求）
workbuddy_quota: limit=350 used=350
```

这**是设计行为，不是缺陷**。`git log` 显示两条相反的规则，后者是对前者的实测修正：

- `9b22ca3`「Keep credit-exhausted WorkBuddy accounts in rotation」曾假设"信用包用尽后 free 模型仍可用"，
  于是把 14018 判成模型级；
- `dafe8a9`「Park an account whose allowance is exhausted instead of retrying it forever」**用生产观测推翻了它**：
  “The upstream was refusing every request on every one of the seven accounts with code 14018”，
  即**额度耗尽是对整个账号的判决，对所有模型都拒绝**。保持账号在池里只会让每个请求重试多个死号
  （8.7s/请求）并招来二次 429，且账号表上没有任何原因。

所以 `policy.go` 的账号级 402 + `isAccountAvailable` 尊重 `QuotaResetAt`（停到计费周期重置）是正确的。
**代码不能凭空产生额度**：真正的根因是池子的额度被烧穿了——4 个 350-credit 的免费包在 2026-09-17 一天内用尽，
（该渠道的请求是 30–40 万 input tokens 的长会话），
于是整个渠道只剩 **3 个号**（`159/217/218`）在扛。

剩下的 3 个号一旦被上游限流（`14003`），就各写一条**模型级**冷却；请求的模型在所有候选账号上都被冷却过滤掉，
候选集为 0 → 503。日志窗口内 `/workbuddy/v1/chat/completions` 共 260 次，47 分钟的窗口里 8 次 503。

### 1.3 真正的代码缺陷：初始选号把容量问题报成 503

`internal/handler/handler.go:614`（修复前）：

```go
apperrors.New("overloaded_error", err.Error(), http.StatusServiceUnavailable).WriteResponse(w)
```

两处都错：

1. **状态错**。同一条件在"请求中途重试耗尽"的入口（`InjectNoAvailableAccountError`）早就按**可重试的 429**
   回复（并有测试钉住），而初始选号却回 503 + `overloaded_error`。一个只是需要等冷却的容量问题，
   到了调用方那里变成了服务器故障：面板照原样透传，客户端据此判定为致命错误。
2. **文案错**。客户端拿到的 body 是**选号器的内部说明**。而空池其实有三种原因
   （模型级冷却 / 全池限流 / 额度耗尽），在 `loadbalancer.go:172` 被压成同一句
   `no enabled accounts available for channel: X`，读起来像"这个渠道没有账号"。
   本次事件的报错**不带** `(all matching accounts are rate-limited or cooling down)` 后缀，
   正说明是 `channelMatched == 0` 那条分支——即**请求的模型在所有账号上都在冷却**，
   而这句话在当时的错误里完全没有体现。

### 1.4 本次修复（第一轮）

| 改动 | 文件 | 说明 |
| --- | --- | --- |
| 空池原因分别命名 | `internal/loadbalancer/loadbalancer.go` | 三种原因各自带后缀：`cooling down for the requested model` / `rate-limited or cooling down` / `have exhausted their allowance`；计数器按扫描轮次重置，窗口扫描与全池回退不再相加 |
| 统一"池子接不了这个请求"的应答 | `internal/errors/pool.go`、`internal/handler/no_account.go` | 分类器放在 `internal/errors`（唯一一处规则），文案 + 状态由 `apperrors.StatusForCategory` 决定，因此**状态与文案不可能互相矛盾**：冷却/全池限流 → 429，额度耗尽 → 429，账号都忙 → 429，模型不可路由 → 404，其余（无账号、凭据全废）→ 503 |
| 初始选号改用它 | `internal/handler/handler.go` | 不再硬编码 503 `overloaded_error`，内部说明只进日志（`slog.Error("selectAccount failed", …)`） |
| 重试耗尽入口改用它 | `internal/handler/stream_handler.go` | 与初始选号共用一套规则；原有分支文案逐字保留，仅新增两条来自选号器的原因 |
| 测试 | `internal/errors/pool_test.go`、`no_account_test.go`、`loadbalancer_test.go`、`handler_warp_status_test.go` | 覆盖 9 种原因、状态断言、以及"内部选号说明不得出现在响应里" |

效果：同一状况下客户端拿到的是 `429` + “the requested model is cooling down on this channel.
Please retry after its cooldown or choose another model.”；而额度耗尽时是 `429` + “every account for
this channel has exhausted its allowance. Add credits or accounts, or wait for the quota reset.” ——
后者才是本次事件真正需要运维看到的动作。

### 1.5 同类路径彻底收口（第二轮）

第一轮只改了**通用会话入口**。grok 各处理器自己选号（`openCLIAccountSession` /
`openConsoleAccountSession` …），于是同一条件在那里还有第二种答案。全量审计（对"能拿到池错误的
客户端应答点"逐个读）后逐条修掉：

| 原状 | 文件 | 现在 |
| --- | --- | --- |
| `writeGrokNoAccountError` **收下 err 却不用**，硬编码 503 + 一句话（8 个调用方：chat / images / image-edits ×2 / images-console / video-chat / admin） | `internal/grok/pool_error.go`、`http_helpers.go` | 改为共享分类：冷却/限流/额度/忙 → 429，模型不可路由 → 404，只有真正无账号才 503；信封仍是该平面的 OpenAI 形状 |
| responses 会话打开失败 → 503 + `err.Error()`（池子内部说明直接进响应体） | `handler_responses_store.go` ×2 | `writeGrokAccountUnavailable` → 分类 + 固定文案，内部说明进日志 |
| voice（realtime）取号失败 → 503 + `"no available Grok Console account: " + err.Error()` | `handler_voice_ws.go`、`handler_voice.go` | 同上；语音转发路径的 typed error 也改成预分类（状态/文案随原因） |
| 视频（console）取号失败 → 同上拼接 | `handler_videos_console.go` | 同上 |
| 网关 compaction 取号失败 → 503 + `err.Error()`（该路径的注释写着"客户端看不到上游散文"，但选号错误绕过了净化） | `responses_compaction.go` | 分类后写入，日志保留原文 |
| **异步**视频任务失败把 `err.Error()` 存进 `job.Error.message`，客户端 GET 时以 **200** 读到池子内部说明 | `handler_videos.go`（`videoJobFailureMessage`，覆盖 build/console 全部任务失败路径） | 池子原因存分类后的可重试文案，其它失败保留自身文本，原文进日志 |
| 图片限流换号失败时 `switchErr` 被丢弃，无日志 | `handler_images.go` | 记 `slog.Warn`，原因不再消失 |
| 多 pool 选号时只在错误含 `rate-limited or cooling down` 时才替换 `lastErr`，额度耗尽/并发原因会被更早的空错误压掉 → 客户端收到无法解释的 503 | `internal/grok/handler.go` | 改为"带原因的优先于不带原因的"（`carriesPoolReason`） |
| 测试 | `internal/grok/pool_error_test.go` | 6 种原因的状态/文案/类型 + "内部说明不得进响应"；`videoJobFailureMessage` 的池子/非池子分支 |

### 1.6 上游散文一并净化（第三轮）

按部署方选择，`upstream_error` 路径也不再透传上游自己写的原文。此前各平面并不一致：
chat/images/videos/console 走 `writeGrokUpstreamError`（`apperrors.PublicMessage` + 类别状态 + 保留
`Retry-After`），而 Responses/voice 平面直接写 `err.Error()`，把
`grok cli upstream status=403 body={"error":{"code":7,…}}` 这类内容发给客户端。

| 原状 | 文件 | 现在 |
| --- | --- | --- |
| 上游失败 → `writeResponsesAPIError(…, "upstream_error", err.Error())` | `handler_responses_store.go`（2 处）、`handler_voice_ws.go`、`responses_compaction.go` | `writeGrokUpstreamFailure` / `grokUpstreamFailureMessage`：状态保持调用方算出的值，文案换成共享类别句，原文进日志 |
| voice 转发把上游 error 直接塞进 typed error | `handler_voice.go` | 构造时就净化；`writeConsoleVoiceRequestError` 的兜底分支同样处理 |
| **异步**视频任务把上游原文存进 `job.Error.message`（客户端 GET 200 读到） | `handler_videos.go` | `videoJobFailureMessage` 对上游失败返回类别句；本地失败（部件缺失、任务中断）保留自身文案，`error_code` 仍是机器可读的种类 |
| 测试 | `internal/grok/pool_error_test.go` | 新增 `TestWriteGrokUpstreamFailure_KeepsProseOutOfTheBody`（状态保持 + 散文不得入 body + 本地错误保留文案），并扩展 `videoJobFailureMessage` 的上游/本地分支 |

**仍保留（有意）**：调用方自己的错误（`invalid_request_error`、错的方法、multipart 参数）保留精确文案——
把它压成"上游失败"会藏掉调用方唯一能修的东西；`isUpstreamFailure` 就是这条分界线（与
`writeGrokUpstreamError` 同一判据）。

**待办（等并行改动落地后一行即可）**：`internal/handler/models.go:192` 的
`"Failed to fetch models: " + err.Error()`（500）会把存储层错误发给 `/v1/models` 调用方；
该文件当前有未提交的并行改动，动它会在对方提交时丢失，故留待合并后处理。

## 2. 错误 B — `system cpu overloaded (current: 99.2%, threshold: 90%)`

`{"error":{"type":"new_api_error","message":"system cpu overloaded (current: 99.2%, threshold: 90%)"}}`

- 这条**不是 orchids 产生的**：`strings /opt/orchids-2api/orchids-server | grep -c "system cpu overloaded"` = **0**，
  近 7 天 journal 出现次数 = **0**。
- 字符串与 `new_api_error` 都来自 New API：`middleware/performance.go` 的系统保护
  （`system_cpu_overloaded` / 503），`int(CPUUsage) > monitor_cpu_threshold`（默认 90），
  每 5s 采一次**面板主机**的系统级 CPU（`common/system_monitor.go`），超阈值期间所有请求直接 503。
- 99.2% 是**面板那台机器**（`<PANEL_IP>`，同机还跑 WARP 出口与数据库）的水位：
  `<PROD_IP>` 同期 load average 0.01/0.02、内存 1961MB 用 511MB、CPU 压力 `avg10=1.29%`。
- 时间也对得上：客户端 request id `20260920072841…` = 07:28:41Z，正落在 orchids 那批 503
  （07:28:33 / 07:28:48）之间——同一分钟里面板一边被自己的 CPU 保护挡住，一边在转发 workbuddy 的 503。

处置：面板后台 → 系统设置 → 性能/监控设置，把 `monitor_cpu_threshold` 调到 95–98（或 0 关闭），
并排查面板主机 99.2% 的来源。orchids 侧无需改动。

## 3. 处置与不复用的方案

### 3.1 代码（两轮已改）

- 第一轮：见 1.4（通用会话入口 + 选号器原因）。
- 第二轮：见 1.5（grok 各处理器的同类路径彻底收口）。
- 验收：`go build ./...`、`go vet`、`go test ./...` 全绿后构建 amd64 产物再部署。

### 3.2 容量（运维决策，代码无法替代）

- workbuddy 渠道现有 7 个号，其中 4 个已在本计费周期耗尽（9-26/9-27 才重置），**实际并发容量只有 3 个号**。
  要根治"池空 503"，需要补号或降低消耗。
- 消耗侧：该渠道承载的是 30–40 万 input tokens 的长会话（实测
  378 条请求中 224 条 > 262 144，中位数 297 356）。把长会话路由到不计量额度的渠道、
  或在客户端做上下文压缩，是比补号更根本的手段。
- 面板侧的 CPU 阈值见第 2 节。

### 3.3 上一轮的脚本已删除

上一轮曾把"4 个号停到 9-26"当成僵尸状态，并写了 `scripts/clear-workbuddy-stale-402.sh` 去提前释放。
按第 1.2 节的证据（`dafe8a9` 的生产观测：额度耗尽对所有模型都拒绝），提前释放只会把死号放回池子，
重现"每个请求重试多个死号 + 二次 429"的原始事故。该脚本已删除，不作为方案保留。

## 4. 复核清单

```sh
# 池空是否按原因上报（文案里应出现 cooling down / rate-limited / exhausted their allowance）
journalctl -u orchids-2api --since "-1h" | grep "selectAccount failed"
# 客户端应答是否已是 429（不再是 503 overloaded_error）
journalctl -u orchids-2api --since "-1h" | grep '"status":503'
```

账号侧取数（只读）：

```sh
python3 - <<'PY'
import subprocess, json
rc = lambda *a: subprocess.run(["redis-cli", *a], capture_output=True, text=True).stdout.strip()
for k in rc("--scan", "--pattern", "orchids:accounts:id:*").split():
    d = json.loads(rc("get", k) or "{}")
    if (d.get("account_type") or "").lower() == "workbuddy":
        print(d.get("id"), repr(d.get("status_code")), d.get("quota_reset_at"), d.get("usage_current"), "/", d.get("usage_limit"))
PY
```
