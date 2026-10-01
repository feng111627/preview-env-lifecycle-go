# preview-env-lifecycle-go

按需预览环境的生命周期管理。用本地内存资源（`LocalProvisioner`）模拟命名空间与配额，
保证多人同时开环境、到期回收、失败重试时占用状态一致且失败原因可区分。

## 生命周期状态

```
                Create
                  │
            provisioning ──Provision 失败──▶ failed（占用回滚，从占用表摘除）
                  │ Provision 成功
                  ▼
                ready ──Bind──▶ in_use
                  │               │ （InUse 不被回收；Touch/Bind 会续期）
                  ▼ 到期且无人使用
                reaped（Reap 回收，占用释放）
```

- `Create` 幂等：同名环境已活跃时直接返回已有环境，并发提交不会产生两份。
- 创建先在占用表占位再调底层资源；失败时仅回滚自己写入的占位，同名可立即重建。
- `Destroy` 先释放占用再销毁底层资源，避免重建竞态造成泄漏或重复占用。

## 配额与权限规则

- 配额：`Quota.MaxEnvsPerUser` 限制单用户活跃环境数（provisioning/ready/in_use 均计入）。
  超限拒绝，错误可用 `errors.Is(err, ErrQuotaExceeded)` 判定。
- 权限：仅所有者可 `Destroy`；环境只能被所有者领取（`Bind`），已绑定环境他人不可抢。
  越权拒绝，错误可用 `errors.Is(err, ErrPermissionDenied)` 判定。
- 两类拒绝原因互不混淆：配额错误不会是 `ErrPermissionDenied`，反之亦然。
- 回收：`Reap` 只回收**到期且非 in_use** 的环境；`Bind`/`Touch` 会续期（重新激活），
  刚被激活的环境不会被误回收；重复 `Reap` 是幂等 no-op。

## 本地验证

```bash
go build ./...      # 编译
go test ./... -v    # 单测，日志打印关键输入与判定依据
go test -race ./... # 并发竞态检测
```

测试覆盖：并发同名创建去重、并发异名创建计数一致、创建失败回滚与重建、
配额/权限拒绝原因区分、到期回收与使用中保护、重复回收幂等、回收后重建无泄漏。
