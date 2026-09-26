# Qoder 限流治理：方案对比与本次改动

参考项目：`Zhengyuuuui/qoder2api`（Qoder → OpenAI/Claude/Codex 兼容网关，源自 `wangtufly/QCCG`）。
目标：对比我们 `internal/qoder` 的实现，**解决总是出现限流的问题**。范围限定在两处：

1. **错误分类与流稳定性**
2. **设备指纹稳定化**

Checkin/额度、CN 区域支持不在本次范围。

---

## 一、方案对比

### 1. 设备指纹

| 维度 | qoder2api | 改动前的我们 | 结论 |
| --- | --- | --- | --- |
| `cosy-machineid` | `DeriveMachineID(seed)`，32 位十六进制，**由账号 seed 单向派生** | 存库时随机 UUID，之后固定 | 我们已稳定，但格式与真机 CLI 不一致 |
| `cosy-machinetoken` | `DeriveMachineToken(seed)`，43 位 base64url，**与 machineid 不同值** | **等于 machineid**（同一 UUID 复用） | ❌ 三头同值 = 明显伪造特征 |
| `cosy-machinetype` | `DeriveMachineType(seed)`，18 位十六进制 | **硬编码 `"5"`（等于 clientType）** | ❌ 类型位不是设备派生值 |
| 派生稳定性 | 同一账号永远同一台设备 | 同账号稳定，但机器特征形态伪 | 形态伪 → 风控按"非真机设备"归类 |
| 本机盐 | `machine_salt` 首次启动生成并落盘，**明确禁止修改** | 无 | 缺部署级隔离 |
| `business-product` | `ide` | `cli` | 保留我们的（CLI 通道语义） |

派生算法（与 qoder2api 逐字节同构，仅"无盐"时）：

```
seed        = uid，否则 "cred:" + credential
salted      = salt=="" ? seed : seed + "|salt:" + salt
machineId   = md5("machine:"     + salted)              → 32 hex
machineType = md5("machinetype:" + salted)[:18]         → 18 hex
machineToken= b64url(sha512("machinetoken:" + salted))[:43]
```

交叉校验向量（`seed = "test-uid-123"`，无盐）：

```
005b8945c0659064f8d25299980a27b3
66ea01f7983702b088
zzpUYGGMSPEfJVrGQWHj7SBYaRUMwPMK0B4QN_aqKP0
```

### 2. 错误分类与流稳定性

| 维度 | qoder2api | 改动前的我们 |
| --- | --- | --- |
| 分类数 | 四类：内容审核 / 瞬时 / 客户端参数 / 其余上游 | 无显式四类，多数落到 `unknown` |
| 内容审核 | 400 + `content_policy_rejected`，**永不重试** | 落到 `unknown` → `Retryable+SwitchAccount=true` |
| 瞬时错误 | 418 / 5xx / 4xx+`provider_error` / 传输层，同账号退避重试，上限 `TransientMaxRetries=2` | 500+ 直接上抛给共享循环 → 换账号 |
| 客户端参数 | 永不重试 | 同上，被当成可重试 |
| 空流 | `ErrEmptyStream`，**不重试** | `"qoder stream produced no usable events"`，落到 `unknown` → 换账号重试 |
| 流内错误帧 | 立即停读 | 已停读，但未分类 |
| usage | 与 choices 合并同一帧 | finish 帧带 usage（保留） |

**限流的真正成因（改动前）**：内容审核拒绝、参数错、空流这三类"与账号无关"的失败，
在分类里落到 `unknown` → `{Retryable: true, SwitchAccount: true}`。共享循环于是拿
本次请求**逐个账号刷一遍**，把整池账号依次标成 429 冷却 —— 一个被拒的 prompt
就能把池打空。这正是"总是出现限流"的放大路径：上游只拒绝了一次，我们把它复制了 N 次。

---

## 二、本次改动

### 设备指纹稳定化

- 新增 `internal/qoder/fingerprint.go`
  - `FingerprintSeed / DeriveMachineID / DeriveMachineType / DeriveMachineToken`
  - 本机盐：`SetInstallSalt / InstallSalt / EnsureInstallSalt(ctx, SettingsStore)`，
    写入 Redis setting `qoder.machine_salt`，**生成一次，永不轮换**（轮换 = 全池指纹漂移 = 集体换设备）
  - `FingerprintFor(machineID, uid, credential)`：
    - **`Cosy-MachineId` 原样透传**（它是登录时被授权的绑定身份，改它会打断凭证↔设备绑定）
    - `Cosy-MachineToken`、`Cosy-MachineType` **由账号派生**，与 machineid 不同值
    - seed 优先级：uid → 已存 machineid → access token
  - `cmd/server/main.go` 启动时 `EnsureInstallSalt`
  - `internal/qoder/quota.go` 控制面调用同样发派生 token，与签名请求呈现同一台设备

### 错误分类与流稳定性

- 新增 `internal/qoder/errors.go`
  - `ErrEmptyStream` / `ErrContentPolicy` / `ErrClientFault` / `ErrTransientUpstream`
  - `IsContentPolicy / IsClientFault / IsTransientUpstreamStatus / IsTransientTransport`
  - `TransientMaxRetries = 2`，`TransientBackoff(attempt) = attempt` 秒
- `internal/qoder/request.go`
  - `classifyStatus`：内容审核 / 参数错 → 不可重试；418、5xx、4xx+`provider_error` → 有界重试（带哨兵）
  - 传输层失败：只有连接级抖动才重试；坏 URL、不受信证书、`context.Canceled` 一律不重试
- `internal/qoder/stream.go`
  - 流内 `error` 帧按四类分类，命中即停读
  - 信封非 200 分支新增内容审核 / 参数错 / 瞬时三类
  - `streamResult.emitFinish`：finishReason + usage 合一帧
- `internal/qoder/client.go`
  - 空流 → `ErrEmptyStream`（不重试）
  - 瞬时错误**在同账号本地有界重试**（`TransientMaxRetries`），不再一上来就换账号
  - 401 刷新仍只有一次，且不计入瞬时预算
- `internal/errors/classify.go`
  - 内容审核 / 参数错 → `client`（不可重试、不换账号）
  - `empty upstream stream` → `protocol`（不可重试、不换账号）
- `internal/accountpolicy/policy.go`
  - `isClientRefusal` 与空流：**不写账号状态、不冷却、不换账号**（`ScopeNone`）
  - 这直接切断"一个被拒 prompt 把整池标 429"的链路

---

## 三、保持不变的既有行为

- `isGlobalUpstreamRefusal`（10605 / isQueued / "qoder gateway is busy"）：仍是不换账号、
  不写冷却、按 `sharedRefusalWait` 的 1/16 → 1/4 → 3/4 阶梯等待
- `RuntimeFields` 派生与 `runtime_test.go` 固定夹具
- `accountClientFingerprint` 未新增字段（`QoderMachineID` 未变，缓存键不受影响）
- 默认账号并发上限 10、`MaxRetries` 默认 3、`RetryDelay` 默认 1000ms

## 四、验证

```
GOCACHE=/tmp/gocache-orchids go build ./...
GOCACHE=/tmp/gocache-orchids go test ./internal/qoder/ ./internal/errors/ \
  ./internal/handler/ ./internal/accountpolicy/ ./internal/api/ \
  ./internal/store/ ./internal/loadbalancer/ ./cmd/... -count=1
```

新增测试：

- `internal/qoder/fingerprint_test.go`：参考向量交叉校验、稳定性、账号隔离、
  盐的一次性生成与不可轮换、绑定 machineid 不被改写
- `internal/qoder/errors_test.go`：四类判定的边界（401/403/429 不走瞬时）、
  传输层判定、空流不重试、内容审核不重放、瞬时错误本地重试有界、busy 仍走共享窗口
- `internal/qoder/client_test.go`：header 契约改为断言派生后的 token/type
