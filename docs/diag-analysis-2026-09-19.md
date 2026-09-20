# orchids-2api 线上诊断日志分析报告

- 目标主机：`us1.daige.tech`（AWS Ubuntu 24.04，`ip-172-31-9-86`）
- 服务：`orchids-2api.service` → `/opt/orchids-2api/orchids-server -config config.json`，监听 `:3002`，Caddy 反代 443
- 存储：`redis 127.0.0.1:6379` db0，前缀 `orchids:`（`dbsize = 60877`）
- 采集时间：2026-09-19（UTC，下同）
- 证据文件（已下载到本地工作区）：
  - `.audit/diaglogs/bundles.jsonl` —— Redis 诊断包（`orchids:diagnostics:*`）的原始 JSON；`order` ZSET 与键表共 512 条，其中 508 条可解析（4 条在读取时已过期/为空）
  - `.audit/diaglogs/journal.log` —— `journalctl -u orchids-2api` 日志（13 508 条，实际覆盖 2026-09-16T10:38:32 → 2026-09-19T13:18:36，即该次服务重启至今）
  - `.audit/diaglogs/requests.csv` —— 508 条请求的一行式汇总（本报告所有数字均可复算）
  - `.audit/diaglogs/order.txt` / `keys.txt` —— 诊断保留队列与键名
  - 复现脚本：`.audit/rsh.py`（paramiko 远程执行）、`.audit/analysis/*.py`（离线统计）

---

## 0. 先把「诊断日志」本身讲清楚（这可能是你看到「内容断掉」的第一来源）

诊断包结构（`internal/debug/capture.go`）：

| 键 | 内容 |
|---|---|
| `orchids:diagnostics:<sha256(request_id)>` | 完整 bundle JSON（sections 数组） |
| `...:index` | 轻量索引（sections 名、bytes、truncated、retention） |
| `orchids:diagnostics:order` | ZSET，按写入时间排序，用于淘汰 |

每个 bundle 里的 section：`1_http_request.json`（客户端原始请求体）、`1_claude_request.json`、`2_converted_prompt.md`、`upstream_001_request.json`、`upstream_001_response.txt`、`5_http_response.txt`（回给客户端的内容）、`6_summary.json`、`6_request_events.jsonl`、`6_input_token_breakdown.json`。

**采集上限是硬编码的**（`capture.go`）：单 section `maxCaptureBytes = 64 KiB`、单 bundle `maxBundleBytes = 1 MiB`、最多 96 个 section、`maxDiagnosticBundles = 512`、TTL 24 小时。

实测结果：

- **254 / 508 个 bundle 至少有一个 section 被截断**。其中 `1_http_request.json`、`1_claude_request.json`、`upstream_001_request.json` 各 254 次，`upstream_001_response.txt` 126 次，`5_http_response.txt` 111 次。
- 被截断最典型的原因就是：这个客户端每轮请求体 > 64 KiB（长会话），section 顶部保留 64 KiB，剩下丢弃并置 `"truncated": true`。
- **这是诊断侧截断，不影响回给客户端的内容**：例如 `6_http_summary.json` 里 `bytes = 715083`，说明真实响应 715 KB，而 `5_http_response.txt` 只留了 64 KiB。所以看 Redis 里的日志「内容断掉」是设计使然，不能据此判断线上响应被截断。
- 另一个坑：**保留窗口不是 24 小时**。`order` ZSET 上限 512，实测最新 13:23:56、最旧 07:42:21，**只有 5 小时 41 分**。也就是流量一大，旧包就被淘汰，24 小时只是理论上限。512 个包占 Redis 约 **72 MB**。

> 结论：想在 Redis 里看「完整的一次会话」目前不可能，只能看到最近 ~6 小时、每段 64 KiB 的片段。要做长会话复盘必须先把 `maxCaptureBytes` / `maxDiagnosticBundles` 调大，或把 bundle 落盘到对象存储。

---

## 1. 结论速览（TL;DR）

你描述的两个症状是**两个互相独立的根因**，都成立：

| 症状 | 根因 | 证据强度 |
|---|---|---|
| A. `本轮运行失败 pi-ai detected context overflow for model "qwen3.8-flash"` | 客户端（DSH + pi-ai）不知道这些模型的真实上下文窗口，回退用默认值 **262144**；而实际模型窗口是 **180000（qwen3.8-flash）**、而长会话已到 **30 万~40 万 tokens**。窗口数字对不上 → pi-ai 的「静默溢出」判定把成功的请求判成本轮失败 | 强（代码 + 日志双向对齐） |
| B. 内容断掉 / 任务半途终止 | ① 账号额度耗尽/选号失败 → 网关直接 503/429 终止本轮（warp 4 天 188 次失败、workbuddy 144 次选号失败）；② 上游中途断流（partial output）后不重试 → 客户端拿到半截（15 次）；③ 请求超出模型真实窗口，上游侧截断历史 → 模型「忘事」 | 强 |
| C. Redis 诊断日志「内容断掉」 | 采集上限 64 KiB/section + 512 包上限，属设计行为，非故障 | 强 |

---

## 2. 症状 A：`pi-ai detected context overflow` 的完整链路

### 2.1 这句话是谁打印的

`pi-ai` 是 DSH 的 LLM 适配层依赖（`@deepseek-ai/dsh-llm-pi-ai` → `@earendil-works/pi-ai`），**不是** orchids-2api 打印的：

- 服务器二进制里 `strings orchids-server | grep -i "pi-ai"` → 无
- 4 天 journal 里 `grep -iE "overflow|pi-ai|context length"` → 无
- 前端串 `message.turnError = "本轮运行失败"` 位于 `@deepseek-ai/dsh-client-ui-chat/lib/client.js:2697`

产生该句的代码路径：

```
@deepseek-ai/dsh-llm-pi-ai/lib/index.js:1388  mapStopReason(message, contextWindow)
   → piAiOverflow = isContextOverflow(message, contextWindow)
   → 命中则 failure.message = message.errorMessage ?? `pi-ai detected context overflow for model "${message.model}"`   // :1394
```

注意用的是 `??`：**只有上游错误文本为空（`errorMessage === undefined`）时才会显示这句兜底话术**。你看到的正是兜底话术 → 说明**不是**代理返回了某个错误文本被正则命中，而是 pi-ai 自己算出来的「静默溢出」。

### 2.2 pi-ai 的溢出判定三种情形

`@earendil-works/pi-ai/dist/utils/overflow.js` → `isContextOverflow(message, contextWindow)`：

1. **Case 1 显式报错**：`stopReason === "error"` 且错误文本命中 `OVERFLOW_PATTERNS`（`prompt is too long` / `exceeds the context window` / `maximum context length is N tokens` / `Range of input length should be` …）。→ 我们日志里没有这类文本，排除。
2. **Case 2 静默溢出**：`stopReason === "stop"` 且 `usage.input + usage.cacheRead > contextWindow`。→ **命中的就是这一条**。
3. **Case 3 长度截断**：`stopReason === "length"` 且 `usage.output === 0` 且 input ≥ 99% window。→ 观测到的 qoder 请求 `stop_reason = end_turn`，排除。

即：**模型正常答完（stop），但上报的输入 token 数超过了客户端以为的窗口 → 判溢出 → 本轮失败。**

### 2.3 contextWindow 从哪来：回退到了 262144

`@deepseek-ai/dsh-llm-pi-ai/lib/index.js`：

- `:893  const DEFAULT_CONTEXT_WINDOW = 262144;`
- `:895  const DEFAULT_MAX_TOKENS = 32768;`
- `:670  const contextWindow = entry.contextWindow ?? base?.contextWindow ?? request.defaultContextWindow;`
- `:2238 readListing(): contextWindow = capacity(entry.contextWindow, entry.context_window, entry.context_length, entry.max_input_tokens, entry.limit?.context)`

两处来源都取不到值：

1. 客户端配置：`~/.dsh/settings.yaml` 里 provider `daige`（`baseURL: https://api.chinablog.xyz/`）的 7 个 model 条目**只写了 id/name**，没有 `contextWindow`，provider 也没有 `defaultContextWindow` → 落到 262144。
2. 代理侧模型清单：`GET /v1/models`（以及 `/qoder/v1/models`）只返回

   ```json
   {"id":"qwen3.8-flash","object":"model","created":1677610602,"owned_by":"Qoder"}
   ```

   **没有** `context_length` / `context_window` / `max_input_tokens` / `limit.context` 任何一个字段（`internal/handler/models.go` 的 `PublicModelResponse` 只有 id/object/created/owned_by/capabilities/provider/upstream_model）。

   所以 pi-ai 的自动发现也填不上 → 一律 262144。

### 2.4 真实窗口是多少：代理其实知道，只是没对外说

Qoder 账号快照（`orchids:accounts:id:221` → `qoder_model_ids`，由 Qoder 网关下发）中的上下文窗口：

| Qoder 内部 key | 名称 | max_input_tokens |
|---|---|---|
| `qfmodel` | **Qwen3.8-Flash** | **180 000** |
| `qmodel_38max` | Qwen3.8-Max | 180 000 |
| `efficient` | Efficient | 200 000 |
| `cmodel` | Cantus | 200 000 |
| `ultimate` / `performance` | Ultimate / Performance | 1 000 000 |
| `dfmodel` | DeepSeek-Flash | 1 000 000 |

代理在做 Qoder 请求时会把窗口塞进上游参数（`internal/qoder/request.go:171` → `parameters["context_length"] = model.MaxInputTokens`），也就是说**代理自己知道 qwen3.8-flash 是 18 万，但对外 `/v1/models` 一个窗口字段都不吐**。

`qwen3.8-flash` 真实窗口 180 000 < 客户端假设 262 144 —— 方向是「客户端以为自己能装 26 万，其实模型只能装 18 万」。

### 2.5 会话到底有多大：30 万~40 万 tokens（有实测）

同一客户端（`161.118.140.32`，WARP 出口）在同一时段打到 `hy4-preview-f`（workbuddy 渠道）的请求，诊断包里的 `6_summary.json.input_tokens`：

- 378 条有 summary 的请求中，**224 条 > 262 144**，最大 **400 904**
- 分位数：min 68 / median 297 356 / p90 357 375 / max 400 904
- 这个数字**不是代理估的**：代理把上游 `usage.prompt_tokens` 原样转发（核对了 12 条样本，`6_summary.input_tokens` 与 `upstream_001_response.txt` 里的 `prompt_tokens` 完全一致，例：399 060 / 335 194 / 326 961）。同一路径下客户端也确实收到了 usage（例：`prompt_tokens: 68` 的探测包，客户端响应与 summary 一致）。

也就是说：**这个客户端的长会话稳定跑到 30 万~40 万输入 tokens，远超 qwen3.8-flash 的 18 万，也超过客户端的 262 144 假设。**

### 2.6 于是形成死锁（这是「不会持续完成任务」的关键）

- 会话一旦超过 262 144，pi-ai 会把**每一轮成功返回（finish_reason = stop）的请求**判为 context overflow → `message.turnError = "本轮运行失败"` → 任务中断。
- 更糟的是压缩也救不回来：DSH 的自动压缩（`dsh-compaction-basic`）依赖**同一个 `contextWindow`** 算阈值（`resolveCompactSpec`：`thresholdTokens = contextWindow * thresholdRatio`，`:898` 缺窗口时直接报 `no context capacity for targetKey`），而压缩请求本身也要把超长会话再喂一遍模型；实测 113 条压缩请求（`2_converted_prompt.md` 含 compaction engine 提示词）里 **112 条输入 > 262 144**。
- 同时在代理侧，超过 18 万的 Qoder 请求已经超出上游窗口，`context_length=180000` 只能祈祷网关老实报错；一旦网关选择截断历史，模型就会「忘记前文」，表现同样是任务做不下去、内容断掉。

### 2.7 修复（按优先级）

1. **客户端显式声明每个模型的真实窗口**（立刻见效）。`~/.dsh/settings.yaml`：

   ```yaml
   llm-pi-ai:
     providers:
       daige:
         displayName: daige
         apiKeyEnv: DAIGE_API_KEY
         api: anthropic-messages
         baseURL: https://api.chinablog.xyz/
         defaultContextWindow: 180000      # 兜底别再用 262144
         defaultMaxTokens: 32768
         models:
           - id: qwen3.8-flash
             name: qwen3.8-flash
             contextWindow: 180000         # = Qoder qfmodel max_input_tokens
             maxTokens: 32768
           - id: hy4-preview-f
             name: hy4-preview-f
             contextWindow: 256000         # 以 WorkBuddy 实际上限为准；实测会话已到 40 万，需要压缩兜底
             maxTokens: 32768
           - id: grok-4.6
             name: grok-4.6
             contextWindow: 256000
   ```

   注：`contextWindow` 用错方向两边都会出事 —— 报小了会误判溢出（当前症状），报大了会在上游被截断/报错。必须按各渠道真实值填。

2. **代理侧把窗口暴露出去**（治本，一次改动让所有客户端受益）：在 `internal/handler/models.go` 的 `PublicModelResponse` 增加 `context_length`（pi-ai 的 `capacity()` 会读 `context_length` / `max_input_tokens` / `limit.context` 任一字段），值直接取渠道目录：Qoder 取 `modelEntry.MaxInputTokens`，WorkBuddy 取 `maxInputTokens`，Warp/Grok 取各自模型元数据。这样 DSH 的「Models 页面探测」就能自动拿到正确窗口，不必手工维护 settings.yaml。

3. **提前触发压缩**：把压缩阈值降到窗口的 ~60%（180k → 约 108k 就压缩），不要等到溢出才压；同时把会话级工具输出（大文件、长日志）做截断/摘要，别让单轮正文顶到 30 万 tokens。

---

## 3. 症状 B：内容断掉 / 任务半途终止（代理与上游侧）

### 3.1 权威统计：4 天请求量与失败率（Redis ops 聚合）

数据源：`orchids:ops:agg:<分钟>:<渠道>` 哈希（`internal/opsagg`，每分钟每渠道一份，含 `requests`/`success`/`failed`/`model:<id>:*`）。按渠道聚合最近 4 天：

| 渠道 | requests | success | failed | 失败率 |
|---|---|---|---|---|
| http（通用 `/v1/*` 入口，含扫描/看板噪声） | 8 569 | 5 916 | 2 653 | 31.0% |
| workbuddy | 3 876 | 3 732 | 144 | 3.7% |
| probe（合成探测） | 1 159 | 1 132 | 27 | 2.3% |
| grok | 950 | 930 | 20 | 2.1% |
| warp | 937 | 749 | 188 | **20.1%** |
| qoder | 927 | 926 | 1 | 0.1% |
| puter | 295 | 285 | 10 | 3.4% |
| **合计** | **16 713** | **13 670** | **3 043** | **18.2%** |

按模型（4 天，主要项）：

| 渠道 | 模型 | requests | failed | 失败率 |
|---|---|---|---|---|
| workbuddy | `hy4-preview-f` | 2 666 | 0 | 0% |
| workbuddy | `deepseek-v4.1-flash` | 980 | 0 | 0% |
| qoder | **`qwen3.8-flash`** | **881** | **0** | **0%** |
| grok | `grok-4.6` | 929 | 10 | 1.1% |
| warp | `gpt-5-6-sol-low` | 709 | 116 | **16.4%** |

**两个关键读法：**

1. **`qwen3.8-flash` 4 天 881 次请求、0 失败** → 你看到的 `pi-ai detected context overflow for model "qwen3.8-flash"` **绝对不是代理返回的错误**（否则这里必然有 failed），只能是客户端 pi-ai 自己算出来的判定，与第 2 章的推导完全一致。
2. 真正拖垮任务的是：`warp`（20.1%，集中在 `gpt-5-6-sol-low`）+ 选号阶段就失败的通用入口（`workbuddy` 的 144 次失败**没有模型归属**，即卡在选号、根本没到模型；`http` 的 2 653 次里含 1 654 条 404 扫描噪声，其余主要是 401/402/429/502 与池空 503）。

⚠️ 重要口径提醒：`internal/middleware/trace.go:258` 的 `Request completed` 只在 **4xx/5xx**（或 verbose 诊断下的 2xx、或 `/warp/` 路径）时打印，非 warp 的成功请求根本不进 journal。**所以用 journal 行数算成功率会得到「workbuddy 100% 失败」这种假结论**；上面这张表来自 Redis 聚合，才是准的。

另外两个窗口差异也解释得通，别被对不上吓到：

- ops 统计覆盖完整 4 天，而 journal 只从 **2026-09-16T10:38:32**（服务重启）开始，所以 ops `failed = 3043` 大于 journal 里 4xx/5xx 的 **2040** 条 —— 差额是重启前的历史。
- journal 的 2040 条失败构成为：`404 × 1654`（扫描器探路）、`503 × 152`（无可用账号）、`429 × 150`（额度/限流）、`401 × 30`、`400 × 22`、`500 × 14`、`502 × 10`、`402 × 6`、`405/409 各 1`。**真正影响推理的是 503/429/402/502 这 318 条**，其余基本是噪声。

### 3.2 失败原因分布（journal 计数，4 天）

典型错误（journal，截断计数）：

| 次数 | 错误 |
|---|---|
| 228 | `warp stream request failed: HTTP 429 [OUT_OF_CREDITS]: {"error":"No AI credits remaining"...}` |
| 195 | `workbuddy API error: status=429, message={"error":{"data":{"code":14018,"msg":"Credits exhausted..."}}}` |
| 188 | `no enabled accounts available for channel: warp` |
| 144 | `no enabled accounts available for channel: workbuddy` |
| 86 | `puter API error: status=402, body={"error":"No usage left for request.","code":"insufficient_funds"}` |
| 24 | `workbuddy API error: status=400, code=11128, message=Illegal API invocation from an unapproved channel (...)` |
| 15 | `workbuddy API error: status=429, code=14003, message=too many requests` |
| 4 | `grok cli upstream status=402 body={"error":"Grok Build usage balance exhausted"}` |
| 3 | `warp refresh token failed: HTTP 400` |

**账号池快照**（`orchids:accounts:id:*`，共 38 个 enabled）：

| 渠道 | 账号数 | 额度状态 |
|---|---|---|
| warp | 1 | `admin@uq.edu.rs` **1500/1500（已用满）** → 所有 warp 请求 429 |
| workbuddy | 7 | 4 个 `status_code=402`；2 个 `usage 65761/440`、`64005/350`（严重超额）；仅少数可用 |
| puter | 5 | 3 个 `status_code=402`；1 个 1000/1000；1 个 78/1000 |
| qoder | 2 | 51/300、267/300（重置 09-19 19:47 / 09-20 07:31）→ **qwen3.8-flash 也快耗尽** |
| grok | 23 | 多数 7/7、25/25 已用满，大量 0/0 |

时间分布也很说明问题：`2026-09-16T18` 一小时 50 次 workbuddy 503；`09-17T09~12` 每小时 18~27 次；`09-18T20 ~ 09-19T06` 每小时稳定 11 次 warp 429（客户端在每 5 分钟重试 `gpt-5-6-sol`）。

→ warp 这条线路基本处于**持续不可用**状态（709 次 `gpt-5-6-sol-low` 里 116 次直接 429，另有 72 次连选号都没过），`workbuddy` 的 144 次失败全部发生在选号阶段。**客户端撞上这些时段时，网关只能回 503/429，任务直接终止** —— 这是「内容断掉」里最直接的一条。而 qoder/qwen3.8-flash 与 workbuddy 主力模型本身是健康的，说明卡点在账号额度与选号，不在模型链路。

### 3.3 上游中途断流，客户端拿到半截且不会重试

- journal 中 `Upstream failed after partial output, skip retry to avoid duplicated token billing` **15 次**（其中 14 次是 `failed to read workbuddy stream: context canceled`，1 次 `connection reset by peer`）。
  这是设计取舍：已经有部分输出就不再重试（避免重复计费），代价是**客户端只拿到半句话**，且代理这里没有任何补偿（无续写、无 finish_reason 标记）。
- `failed to read workbuddy stream: context canceled` 共 16 次：客户端主动断开（超时/中止）。实测客户端可见耗时 p90 = **167 s**、最长 **264 s**（上游首包耗时只有 7~15 s，慢在长答案的逐字回传），长回合很容易触发客户端侧超时后取消 → 表现依旧是「内容断掉」。
- 一个可留意的观测盲点：这 508 个诊断包的 `6_http_summary.json.stream_failed` **全部为 false**，即中途断流没有被计入 `stream_failed`，看板会误判为「全部正常」。值得加一条：上游在写出部分内容后失败时，把该请求标记出来。

### 3.4 上游按输出上限截断（这条是正常行为，但容易被误读）

`upstream_001_response.txt` 里出现 128 次：

```json
"incomplete_details":{"reason":"max_output_tokens"},"status":"incomplete"
```

对应的是客户端自己发的探测请求（`{"model":"grok-4.6","messages":[{"role":"user","content":"hi"}],"max_tokens":16}`），返回 `finish_reason = length` 属正确行为，不是 bug。客户端可见的 `finish_reason` 分布：`stop` 195、`tool_calls` 131、`null` 53、`length` 4。

### 3.5 workbuddy「未授权渠道」风控（会稳定中断任务）

24 次 `workbuddy API error: status=400, code=11128, message=Illegal API invocation from an unapproved channel (workbuddy blocked the request by security policy (unapproved channel); the prompt may carry a client identity marker)`，被归类为 `client`、**不可重试**，直接终止本轮。

上游（CodeBuddy/WorkBuddy）在提示词里检测到「非官方客户端身份标记」就会拒绝。当前客户端的 system prompt 明写 `You are an AI agent powered by DeepSeek Harness.` 之类字样，很容易被识别为此类标记。**缓解**：在发给 workbuddy 渠道前对 system prompt 里的客户端自述做改写/剥离（代理侧 `internal/workbuddy` 加一层 prompt 归一化），或把这些回合切到 grok/qoder 渠道。

### 3.6 Qoder 侧另有一条隐患

`journal`: `Qoder quota sync failed; leaving the allowance unknown: qoder authorization endpoint is unreachable: Get "https://openapi.qoder.sh/api/v2/quota/usage"...`（1 次）。额度同步失败时段内，负载均衡只能靠「未知额度」放行，很可能在耗尽后仍然继续派发 → 触发上游 429/400。

---

## 4. 数据复核清单（便于你自己验证）

```bash
# 1. 诊断包数量、保留窗口
redis-cli zcard orchids:diagnostics:order
redis-cli zrange orchids:diagnostics:order -1 -1 withscores

# 2. 取单个请求的诊断（需要知道 request_id）
redis-cli --raw get "orchids:diagnostics:$(printf %s "$RID" | sha256sum | cut -d' ' -f1)"
redis-cli get "orchids:diagnostics:$(printf %s "$RID" | sha256sum | cut -d' ' -f1):index"

# 3. 上游额度/账号
redis-cli --raw get orchids:accounts:id:221 | jq '{email,usage_current,usage_limit,qoder_quota}'
redis-cli --raw get orchids:accounts:id:221 | jq -r '.qoder_model_ids[]' | jq -c '{key,display_name,max_input_tokens}'

# 4. 权威成功/失败统计（按渠道，最近 4 天；比 journal 靠谱）
#    orchids:ops:agg:<epoch分钟>:<渠道> 哈希里 requests/success/failed/model:<id>:*
#    现成脚本见 .audit/opsagg.py（纯 socket RESP，无需装 redis 客户端）
python3 /tmp/opsagg.py   # 部署在服务器上执行
redis-cli --scan --pattern "orchids:ops:agg:*" | grep -vE ':dur$|:ttft$|:model:' | sed 's/.*://' | sort | uniq -c

# 5. 失败聚合（journal，用于看错误文本）
journalctl -u orchids-2api --since "4 days ago" -o cat \
  | grep -oE '"error":"[^"]{0,80}' | sort | uniq -c | sort -rn | head -20
```

⚠️ 注意两点环境事实：

- **`/opt/orchids-2api/config.json` 不是运行时的真值**：运行时配置存在 `orchids:settings:config`（`debug_enabled: true`、`admin_user/admin_pass` 等都以 Redis 为准）。用 config.json 里的 `admin/admin123` 登录 `/api/login` 会得到 `Invalid credentials`，排障时请读 Redis 里的配置。
- 诊断键名是 **request_id 的 sha256**，`index` 里不含 request_id，所以没有 request_id 就无法反查某个包；`bundle` 内部的 `request_id` 字段是唯一线索，建议在 index 里补上 `request_id` / 时间 / 渠道，便于按条件检索。

---

## 5. 行动清单（按投入产出排序）

1. **改客户端 `contextWindow`**（10 分钟，直接消除「本轮运行失败」）：`qwen3.8-flash: 180000`，其余模型按真实窗口填；provider 加 `defaultContextWindow`。
2. **补齐账号池**：warp 需购买 credits（唯一账号已 1500/1500）；workbuddy 4 个号 402、2 个严重超额；qoder 两个号剩 51/300、267/300；puter 3 个 402。**在不补号之前，任何客户端都会持续 503/429。**
3. **代理 `/v1/models` 输出 `context_length`**（`internal/handler/models.go`），让所有客户端自动拿到正确窗口。
4. **压缩策略**：阈值降到窗口的 ~60%，并对超长工具输出做摘要；避免把 30 万 tokens 的会话整轮发给 18 万窗口的模型。
5. **workbuddy 身份标记**：剥离/改写 system prompt 中的客户端自述，降低 11128 风控命中率。
6. **诊断保留**：`maxDiagnosticBundles` 512 → 保留窗口只有约 6 小时；如要真正 24 小时 + 完整会话，需要提高上限并把 bundle 落盘（当前 512 包 = 72 MB Redis）。
7. **可观测性补丁**：把「上游已输出后失败」「客户端取消」计入 `stream_failed`，并对 `pool-empty:*` / 429 突增配置告警（现有 `Alert firing` 已有 `pool-empty:warp` 之类，但 4 天里触发了 110 次，说明只是通知、没人处理）。

---

## 附录 A：把「上下文窗口」暴露给客户端的代理侧最小改法

目标：让 `GET /v1/models` 带上窗口字段，pi-ai 的 `readListing()`（`:2238`）就会自动填 `contextWindow`，不用再手工维护客户端 settings.yaml。

1. `internal/handler/models.go`

   ```go
   type PublicModelResponse struct {
       ID            string   `json:"id"`
       Object        string   `json:"object"`
       Created       int64    `json:"created"`
       OwnedBy       string   `json:"owned_by"`
       Capabilities  []string `json:"capabilities,omitempty"`
       Provider      string   `json:"provider,omitempty"`
       UpstreamModel string   `json:"upstream_model,omitempty"`
       ContextLength int      `json:"context_length,omitempty"` // 新增：pi-ai / OpenAI 兼容客户端可读
       MaxOutput     int      `json:"max_output_tokens,omitempty"`
   }
   ```

2. `HandleModels()`（同文件 `:143` 附近）填值，`mChannel` / `m.ModelID` 都是现成的：

   ```go
   entry := publicModelResponse(m.ModelID, mChannel)
   entry.ContextLength = modelContextWindow(mChannel, m.ModelID) // 新增查表
   ```

3. `modelContextWindow` 的数据来源：

   | 渠道 | 来源 | 备注 |
   |---|---|---|
   | Qoder | 账号快照 `orchids:accounts:id:*` → `qoder_model_ids[]` 的 `max_input_tokens` | key→公开 id：`qfmodel`=Qwen3.8-Flash(180k)、`qmodel_38max`=Qwen3.8-Max(180k)、`cmodel`=Cantus(200k)、`efficient`=Efficient(200k)、`ultimate`/`performance`=1M、`dfmodel`=DeepSeek-Flash(1M) |
   | WorkBuddy | `internal/workbuddy/auth.go:734` 的 `MaxInputTokens`（模型元数据已有，但当前未持久化到账号记录，需要加字段或用静态表兜底） | 长会话实测已到 40 万，务必确认真实上限后再下发 |
   | Warp / Grok / Puter | 静态表兜底（`internal/handler/codex_models.go` 已有 context window 概念的实现可参考） | 目录同步能力具备后再动态化 |

4. 别忘了 `HandleModelByID`（同文件 `:247`）也走 `publicModelResponse`，一并填上以保持一致。

短期兜底：即使不做上面的改动，**只改客户端 settings.yaml 的 `contextWindow` 也能立刻消除 `pi-ai detected context overflow`**（见 2.7 第 1 条）。
