# 按需预览环境生命周期管理（本地模拟）

用本地资源模拟「命名空间槽位 + 配额单位」，为多人按需开启的预览环境提供一致、可解释的
生命周期管理：创建 / 领取 / 使用心跳 / 销毁 / 到期回收，并对并发提交、失败回滚、
配额与权限冲突、回收-重建竞态给出确定性结果。

无任何外部依赖（仅标准库），可直接离线运行。

## 核心概念与生命周期状态

| 状态        | 含义                                                                 |
|-------------|----------------------------------------------------------------------|
| `reserved`  | 已占用本地资源，等待所有者在领取超时（默认 30s）内 `Claim`           |
| `active`    | 已被领取、处于可用状态；通过 `Touch`/会话刷新空闲到期时间（默认 TTL 1h） |
| `destroyed` | 终态。资源已归还，命名空间与配额单位全部释放，之后只能同名重建       |

状态流转：

```
Create ──► reserved ──Claim(所有者, 未超时)──► active ──Destroy/Reclaim──► destroyed
              │                                      │
              └── 超过 ClaimDeadline 被回收 ──► destroyed ◄── 空闲超过 TTL 且无活跃会话 ──┘
```

辅助 API：`BeginSession` / `EndSession` 登记活跃会话（进行中的环境不会被回收）；
`Touch` 刷新最后访问时间与到期时间（刚被重新激活的环境不会被误回收）。

## 资源模型（本地模拟）

- `Store`（`store.go`）：模拟一个容量为 `maxUnits` 的资源池。
  - `namespace`：同名环境映射到固定槽位（`ns-<name>`），同一槽位全局只能被一个环境占用；
  - `units`：整数配额单位，占用之和不得超过容量。
  - `Allocate` 先做全部前置检查再一次性落账，失败时不存在部分占用；
    `Release` 对重复释放返回 `not_found`，使回收天然幂等。
- 环境 ID 由名称确定性派生（`env-<name>`），配合名称索引完成同名去重。

## 配额与权限规则

- **配额**：按使用者配置 `Quota{MaxEnvironments, MaxUnits}`。
  - 未配置配额的使用者：创建一律拒绝；
  - `MaxEnvironments` 限制同时存活的环境数量；
  - `MaxUnits` 限制存活环境占用的单位总数；
  - 容量是全局资源池，超过总容量返回资源不可用。
- **权限**：`Claim` / `Touch` / `BeginSession` / `EndSession` / `Destroy`
  均只允许环境**所有者**执行，其他人被拒绝。
- **拒绝原因严格可区分**（见下表），所有错误均为 `*LifecycleError`，
  可用 `previewenv.ReasonOf(err)` 或 `previewenv.AsLifecycleError(err)` 判断。

| Reason                  | 触发场景                                       |
|-------------------------|------------------------------------------------|
| `already_exists`        | 同名环境并发/重复提交（同时返回已存在的环境）  |
| `not_found`             | 环境不存在、已回收、重复销毁                   |
| `permission_denied`     | 非所有者操作                                   |
| `quota_exceeded`        | 无配额、环境数超限、单位数超限（消息中含依据） |
| `resource_unavailable`  | 命名空间被占、全局容量不足、故障注入后回滚     |
| `claim_expired`         | 超过领取截止时间仍未领取                       |
| `expired`               | active 环境空闲超过 TTL 被回收                 |
| `in_use`                | 存在活跃会话，回收被跳过                       |
| `recently_active`       | 处于最近激活宽限窗口（默认 15s），回收被跳过   |
| `still_active`          | 未到领取截止 / 未到空闲到期，回收被跳过        |
| `invalid_argument`      | 入参非法                                       |

## 到期回收规则（`ReclaimExpired(now)`）

对每个环境在同一把锁内串行判定，返回每个环境的 `ReclaimResult{Reclaimed, Reason}`：

1. `reserved`：`now > ClaimDeadline` 才回收（原因 `claim_expired`），否则 `still_active`；
2. `active` 且 `ActiveSessions > 0`：跳过（`in_use`）；
3. `active` 且距 `LastAccessAt` 小于「最近激活宽限」：跳过（`recently_active`）；
4. `active` 且 `now < ExpiresAt`：跳过（`still_active`）；否则回收（`expired`）；
5. 已销毁环境：结果为 `not_found`，重复回收不报错、不重复释放。

## 并发与一致性设计

- 所有生命周期状态变更都在 `Manager.mu` 下串行执行；
- 访问资源后端时锁顺序恒为 **Manager → Store**，不存在反向获取，因此不会死锁；
- 创建与回收互斥：回收释放槽位与重建占用槽位不会交错，竞态下也不会出现
  泄漏（槽位无人占用）或重复占用（一个槽位两份环境）；
- `WithFailHook` 可在「资源已占用、环境尚未登记」的步骤注入失败，
  管理器会立即 `Release` 回滚，之后重试可以成功。

## 本地验证

```bash
go build ./...
go vet ./...

# 常规测试
go test ./... -v

# 竞态检测（并发创建 / 回收-重建竞态用例建议必跑）
go test -race -count=3 ./...

# 覆盖率
go test -cover ./...
```

测试入口与覆盖场景：

| 测试                                        | 场景                                                         |
|---------------------------------------------|--------------------------------------------------------------|
| `TestStore_AllocateAndRelease`              | 命名空间冲突、容量超限、重复释放、释放后账实一致             |
| `TestManager_CreateClaimDestroyLifecycle`   | 创建/领取/销毁主流程、重复提交、越权、重复回收               |
| `TestManager_CreateRollbackOnFailure`       | 资源分配后注入失败 → 回滚 → 立即重试成功                     |
| `TestManager_QuotaDeniedDistinctFromPermission` | 无配额、环境数超限与权限拒绝的原因可区分                 |
| `TestReclaim_ReservedClaimExpiry`           | 保留态领取超时回收                                          |
| `TestReclaim_ActiveIdleRules`               | 使用中不回收、宽限保护、心跳续期、空闲到期回收               |
| `TestReclaim_DuplicateIsIdempotent`         | 连续回收幂等、超时领取与回收后领取的原因区分                 |
| `TestConcurrentCreate_SameNameProducesOne`  | 50 个 goroutine 并发同名创建：恰好 1 份成功、1 份占用        |
| `TestConcurrentCreate_DifferentOwnersQuotaContention` | 多用户并发下配额拒绝数与占用一致                   |
| `TestReclaimAndRecreateRace`                | 40 回收者 + 40 重建者竞争：回收恰 1 次、最终恰 1 份占用      |

所有用例均使用 `t.Logf` 打印关键输入（用户、名称、单位数、时间点、会话数）
与判定依据（比较的配额/时间阈值与最终 reason），`-v` 运行可见完整决策过程。

## 快速上手

```go
store := previewenv.NewStore(100)
m := previewenv.NewManager(store, time.Hour /* TTL */)
m.SetQuota("alice", previewenv.Quota{MaxEnvironments: 3, MaxUnits: 20})

env, err := m.Create(ctx, previewenv.CreateRequest{Name: "feat-x", Owner: "alice", Units: 5})
// 重复/并发提交同名环境 -> 返回同一 env，err 原因为 already_exists

if _, err := m.Claim(ctx, env.ID, "alice"); err != nil { ... }
m.Touch(ctx, env.ID, "alice")                     // 续期
results, _ := m.ReclaimExpired(ctx, time.Now())   // 批量到期回收
```
