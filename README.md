# preview-env-lifecycle-go

按需预览环境的生命周期管理。用本地内存资源（`LocalProvisioner`）模拟命名空间与配额，
保证多人同时开环境、到期回收、失败重试时占用状态一致且失败原因可区分。

本轮在原有“谁开谁用”之上扩了两层：**全局容量上限**与**共享池复用**，
原 `NewManager` / `Create` / `Bind` / `Destroy` / `Touch` / `Reap` 的签名与对外行为保持不变，
不传全局上限时（`NewManager`）与旧行为完全一致。

## 生命周期状态

```
                Create
                  │
            provisioning ──Provision 失败──▶ failed（占用回滚，从占用表摘除）
                  │ Provision 成功
                  ▼
                ready ──Bind──▶ in_use
                  │  │ ReleaseToPool（让出占用，仍占全局容量）
                  │  ▼
                  │ pooled（共享池，待复用）
                  │  │ Acquire：先弹出切片、置 resetting（锁外清洗，别人拿不到/动不了）
                  │  ▼
                  │ resetting ──Clean 成功──▶ ready（Owner 过户给新用户，无残留）
                  │     │ Clean 失败 / 期间额度满
                  │     └──补偿回 pooled（账面与底层都不动）
                  ▼ 到期且无人使用
                reaped（Reap 回收，占用释放）
```

- `Create` 幂等：同名环境已活跃时直接返回已有环境，并发提交不会产生两份。
- 创建先在占用表占位再调底层资源；失败时仅回滚自己写入的占位，同名可立即重建。
- `Destroy` 先释放占用再销毁底层资源，避免重建竞态造成泄漏或重复占用。

## 全局容量

- 用 `NewManagerWithConfig(Config{Quota: ..., MaxActiveEnvs: N}, ttl, prov)` 设置全局活跃上限；
  `N <= 0` 表示不限。计数口径是“底层真实存在的资源”：`provisioning/ready/in_use/pooled/resetting`
  全部计入（入表即计数、回滚或回收即出表），因此账面占用与底层命名空间数始终一致。
- 到顶后新申请（`Create` 或池空时的 `Acquire` 新建分支）返回
  `ErrGlobalCapacity`（`errors.Is(err, ErrGlobalCapacity)`），**不静默丢弃、不虚增**。
- 三类拒绝严格分开、互不冒充：
  - 没权限 → `ErrPermissionDenied`
  - 这个用户自己额度用满 → `ErrQuotaExceeded`
  - 整体容量不够 → `ErrGlobalCapacity`

## 共享池复用

- `ReleaseToPool(name, user)`：仅当前所有者可把**空闲 ready**的环境放入共享池。
  入池后占用从该用户身上让出（不再计其个人额度），但资源仍在、仍占全局容量；
  `Owner` 置空、原主人记在 `PrevOwner`，重复入池幂等。使用中/创建中/交接中的环境不能入池。
- `Acquire(ctx, user)`：
  1. **优先复用**：池非空时取队首一份，先从池切片摘除并置 `resetting`，
     调用底层 `Cleaner.Clean` 清掉上一任残留，成功后把占用过户给新用户（`Owner=user`，
     `BoundTo` 清空，状态回到 ready）。复用路径总数不增、不虚增。
  2. 池空时才新建：先查个人额度，再查全局容量，任一不满足给出对应错误。
- Provisioner 需实现可选接口 `Cleaner`（`Clean(ctx, env, prevOwner) error`）才允许交接；
  `LocalProvisioner` 已实现。池中和交接中的环境不能被直接 `Bind`（请走 `Acquire`）。
- 原主人在环境入池后仍可 `Destroy` 它；`resetting` 期间销毁/绑定/回收都会被
  `ErrConflict` 挡住，保证“同一环境不会被两人同时拿到、不会被腾挪”。

## 并发与失败一致性

- 全部状态变更在同一把互斥锁下完成；从池里取环境是“锁内弹出 + 置 resetting”，
  因此并发抢占不会超发，同一环境也不可能分给两个人。
- 清洗在锁外执行，期间该环境已不在池中、无人可领取。任何交接失败
  （清洗报错、交接瞬间用户额度已满、Provisioner 不支持清洗）都会把环境**补偿回池**，
  不把占用记到任何人头上；若交接期间环境已被回收/销毁，则销毁洗干净的底层资源，避免泄漏。
- `Reap` 跳过 `in_use`、`provisioning`、`resetting`，回收 ready 及**空闲到期的 pooled**
  （同时从池摘除）。回收、复用、重建交叉时以占用表为准，不复活已回收环境。

## 配额、权限与回收的兼容关系

- 个人额度 `MaxEnvsPerUser`：只统计真正记在该用户名下的 `provisioning/ready/in_use`；
  池里（pooled）与交接中（resetting）的环境不计任何个人额度。
- 全局容量 `MaxActiveEnvs`：统计所有底层仍存在的环境，是比个人额度更外层的一道总盘子。
  判定顺序：权限 → 个人额度 → 全局容量（池空新建时），三种错误互不冒充。
- 到期回收规则不变：`Bind`/`Touch`/复用过户会续期，刚被（重新）激活的环境不会被误回收；
  重复 `Reap` 是幂等 no-op。
- `NewManager` 不传全局容量（不限），旧代码与旧用例无需改动。

## 本地验证

```bash
go build ./...        # 编译
go test ./... -v      # 单测，日志打印关键输入与判定依据
go test -race ./...   # 并发竞态检测
```

测试覆盖（17 个用例）：

- 原有：并发同名创建去重、并发异名创建计数一致、创建失败回滚与重建、
  配额/权限拒绝原因区分、到期回收与使用中保护、重复回收幂等、回收后重建无泄漏。
- 本轮新增：
  - 全局容量触顶返回 `ErrGlobalCapacity` 且账实不增；
  - 权限 / 个人额度 / 全局容量三种拒绝互不冒充；
  - 空闲环境入池复用、占用过户、总数不增、交接后无残留、原主人失去访问权；
  - 多人并发抢有限可复用环境：恰好分完、不超发、同一环境不分给两人；
  - 多人并发新建抢全局盘子：不超发，账实一致；
  - 清洗失败返回 `ErrReuseFailed` 并补偿回池，故障排除后可再次复用；
  - 交接进行中（resetting）不被回收/销毁/绑定，放行后正确过户；
  - 池中环境到期被回收、容量释放后 `Acquire` 走新建而非复活旧环境。

