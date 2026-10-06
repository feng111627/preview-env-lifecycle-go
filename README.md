# preview-env-lifecycle-go

按需预览环境的生命周期管理。用本地内存资源（`LocalProvisioner`）模拟命名空间与配额，
保证多人同时开环境、到期回收、失败重试时占用状态一致且失败原因可区分。
在共享复用与全局容量管控下，空闲环境可被后来的申请直接复用，
全局活跃环境总数受统一上限约束，账面上的占用与真实底层资源始终一致。

## 生命周期状态

```
                Create / Acquire（池空时新建）
                  │
            provisioning ──Provision 失败──▶ failed（占用回滚，从占用表摘除）
                  │ Provision 成功
                  ▼
                ready ──Bind──▶ in_use
                  │  ▲            │ （InUse 不被回收、不可被复用；Touch/Bind 会续期）
                  │  │Release     │Release（仅所有者，且未被他人绑定）
                  ▼  │            ▼
              共享池（pooled, ready）
                  │  ├─原主直接 Bind 拿回──▶ in_use（清掉池内标记，占用重新记到原主名下，过个人额度）
                  │  ├─他人 Acquire────────▶ in_use（Sanitize 清理后交接，占用从原主转到新使用者）
                  │  └─到期且无人使用
                  ▼
                reaped（Reap 回收，占用释放）
```

- `Create` 幂等：同名环境已活跃时直接返回已有环境，并发提交不会产生两份。
- 创建先在占用表占位再调底层资源；失败时仅回滚自己写入的占位，同名可立即重建。
- `Destroy` 先释放占用再销毁底层资源，避免重建竞态造成泄漏或重复占用。

## 共享复用与全局容量

- **全局容量**：`Quota.MaxGlobalEnvs` 限制全局活跃环境总数（provisioning/ready/in_use，
  含共享池内的）。到顶后新建申请返回 `ErrCapacityExhausted`，
  可用 `errors.Is` 判定，与 `ErrQuotaExceeded`（个人额度）、`ErrPermissionDenied`（权限）
  三类拒绝互不冒充。
- **共享池**：所有者用 `Release(name, requester)` 把空闲环境释放进池。
  池内环境不计入任何个人配额，但仍计入全局容量；到期照样被 `Reap` 回收。
  仅所有者可释放（越权返回 `ErrPermissionDenied`）；重复释放是幂等 no-op。
- **复用**：`Acquire(ctx, user)` 优先从池中取一个空闲环境，
  经 `Provisioner.Sanitize` 就地清理（不增删底层资源）后交付，占用从原主让出、记到新使用者头上，
  环境总数不虚增；池空时才新建（受全局容量约束）。复用本身不增加全局计数，
  因此全局到顶时复用仍然可行。
- **原主直接拿回（不重新走申请）**：环境进池后，原主仍可用 `Bind(name, owner)` 直接拿回自用，
  不需要重新申请。这会被当作一次正常的“重新投入使用”处理：**清掉池内空闲标记（`Pooled=false`）、
  状态变为 `in_use`、占用重新记到原主名下、过个人额度校验并续期**，之后表现与正常申请到的环境完全一致
  （可 `Touch`/`Release`/`Destroy`，到期前受回收保护，再次交回后照样可被复用或回收）。
  他人不能用 `Bind` 直接拿池内环境（返回 `ErrPermissionDenied`），只能走 `Acquire` 的复用交接；
  原主个人额度已满时返回 `ErrQuotaExceeded`，环境原样留在池中、不丢不重，仍可被他人 `Acquire` 复用。
  系统不允许出现“池内空闲标记未清、状态却是使用中”的幽灵占用——这种环境既不会被回收也无法被复用，
  会长期白占全局名额。
- **占用与全局总量怎么记**：任一时刻每个活跃环境要么算在某个具体使用者头上（非池内），
  要么算在共享池里（空闲可复用），二者必居其一，不会出现没人认领的名额。
  全局活跃数（`ActiveCount()`）= 各用户占用之和 + 池内空闲数，始终等于底层真实资源数
  （如 `NamespaceCount()`）。交回池子：个人占用 -1、全局不变；原主拿回或他人复用：个人占用在双方间转移
  （复用给他人时原主 -1、新人 +1；原主拿回时原主 +1）、全局不变；回收/销毁：全局 -1、底层资源同步删除。
- **保护**：正在使用（InUse）或刚被重新激活（续期）的环境不会被复用、腾挪或回收。
- **并发与补偿**：认领、释放、回收、拿回都在同一把锁下判定，并发抢池不超发、
  同一环境不会分给两人（原主直接拿回与他人 `Acquire` 竞争时也只有一个赢家）；
  清理（Sanitize）失败时认领被补偿回池中，
  新使用者不计占用、环境不丢不重，错误返回 `ErrReuseFailed`。
  各类失败原因互不冒充：`ErrCapacityExhausted`（全局容量）、`ErrQuotaExceeded`（个人额度）、
  `ErrPermissionDenied`（无权限）、`ErrReuseFailed`（复用交接失败）。
  `ActiveCount()` 可与底层实际资源数（如 `NamespaceCount()`）对账。

## 配额与权限规则

- 配额：`Quota.MaxEnvsPerUser` 限制单用户活跃环境数（provisioning/ready/in_use 均计入，
  池内环境除外）。超限拒绝，错误可用 `errors.Is(err, ErrQuotaExceeded)` 判定。
- 权限：仅所有者可 `Destroy`/`Release`；环境只能被所有者领取（`Bind`），已绑定环境他人不可抢。
  共享池内环境原主可直接 `Bind` 拿回，他人只能走 `Acquire` 复用，越权直接 `Bind` 返回 `ErrPermissionDenied`。
  越权拒绝，错误可用 `errors.Is(err, ErrPermissionDenied)` 判定。
- 三类拒绝原因（全局容量/个人配额/权限）互不混淆。
- 回收：`Reap` 只回收**到期且非 in_use** 的环境（含池内空闲环境）；`Bind`/`Touch`/`Acquire`
  会续期（重新激活），刚被激活的环境不会被误回收；重复 `Reap` 是幂等 no-op。

## 本地验证

```bash
go build ./...      # 编译
go test ./... -v    # 单测，日志打印关键输入与判定依据
go test -race ./... # 并发竞态检测
```

只验证本次修复的“进池 → 直接拿回自用”路径及其与回收/复用的交叉：

```bash
go test ./... -v -run 'TestReleaseThenReclaim|TestReclaim'
go test -race ./... -run 'TestReclaim'
```

日志中可看到每条用例的输入与判定，例如 `ledger[...]: active=1 namespaces=1 pooled=0 occupied=1`
表示账面活跃数 == 底层命名空间数，且环境已从池中移出、记到具体使用者名下；
`pooled + occupied == active` 恒成立保证不存在无人认领的幽灵占用。

测试覆盖：并发同名创建去重、并发异名创建计数一致、创建失败回滚与重建、
配额/权限/全局容量三类拒绝原因区分、到期回收与使用中保护、重复回收幂等、回收后重建无泄漏、
空闲环境释放与复用（占用转移、清理验证、计数不虚增）、全局到顶时复用仍可行、
并发抢池不超发不重复分发、复用清理失败的补偿与重试、
混合并发操作（申请/释放/销毁/回收交叉）后的账实一致性、
**进池后原主直接拿回自用**（池内标记清除、占用记回原主、续期、表现等同正常申请）、
拿回时个人额度拒绝（环境留池可被他人复用，错误不冒充容量/权限）、
非原主直接 Bind 池内环境的权限拒绝、拿回后再交回再被回收/复用的账实一致、
原主拿回与他人并发 Acquire 不重复分发、拿回/复用/回收混合并发下的账实一致。

