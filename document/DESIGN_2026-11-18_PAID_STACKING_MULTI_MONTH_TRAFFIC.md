# 付费叠加购买:流量按"月数 × 每月额度"叠加(设计稿,2026-11-18)

状态:口径已定(用户 11-18 同意);补齐 SQL 11-18 已执行(两库各 2 行,01b77cab/136c0fe8 现 400G);代码已写完、测试通过,未提交未上线(otun-manager 分支 fix/realm-get-traffic-reset-at、fulfillment 分支 fix/paid-stacking-multi-month)。

## 1. 问题

Stripe 一次性购买(以及积分直购、礼品卡)下发的 `traffic_gb` 是**每月额度**(`100 × 份数`,不乘月数),
otun-manager 每 30 天把 `traffic_used` 清零(`QuotaResetService`,标准面 `users`、住宅面 `realm_users` 各一份)。
所以单独买 6 个月 100G = 每月 100G,共 6 个月。

但用户**在有效期内再买**(付费同渠道叠加,`resolvePaidStacking`,2026-09-23 起)时:

- 上限 = 原上限 + **一个月**的额度(没乘新买的月数);
- `traffic_used` 保留,`traffic_reset_at` 推到新到期日 → 整段时间不再清零;
- 原订单如果是多月单、还有没清零的月份,那几个月的额度也一起丢了。

例:先买 1 个月 100G、到期前再买 3 个月 100G/月 → 4 个月总共只有 200G;按单独购买应该是 1 + 3 = 4 个月的额度。

## 2. 口径(用户 11-18 定)

叠加购买时把流量和时长都加上去,合成一个总量,中途不再清零:

```
新上限 = 原上限                                 (已用流量保留,剩余 = 原上限 − 已用)
       + n × 原每月额度                          (原订单还没清零的月份)
       + 新买月数 × 新每月额度
新到期 = 原到期 + 新买月数(自然月,沿用 expireSpan)
traffic_reset_at = 新到期                       (不清零,与现状一致)
```

- `n`:原订单不是叠加态(`traffic_reset_at ≠ expire_at`)时,
  `n = max(0, round((expire_at − traffic_reset_at) / 30 天) − 1)`;原订单已经是叠加态时 `n = 0`(已经是总量)。
  用 round 是为了让 6 个自然月(181–184 天)算成 6 个窗口而不是 7 个。
- 新买月数:`span.Months > 0` 用 Months;老口径只有天数时用 `max(1, round(Days / 30))`。
- 原每月额度:非叠加态下就是 `live.TrafficLimit`。
- 不叠加的情况不变:第一次买、到期后再买、从试用/赠送转付费 → 每月额度,每 30 天清零。
- Apple / Google 自动续费由平台开新一期,不走这里,不变。
- 不加每月使用封顶(用户 11-18 同意先不加;合成后可以一个月用完)。

## 3. 改动

| 仓 | 改动 |
|---|---|
| otun-manager | `realmAccountToUserResponse` 补 `traffic_reset_at`(标准面 `UserResponse` 已经有,住宅面 GET 现在不返回)。 |
| fulfillment-service | `client.VPNUserInfo` 加 `TrafficResetAt`;`resolvePaidStacking` 按第 2 节算上限(入参改为 span + 新每月额度);投影行 `vpn_provisions.traffic_limit` 跟着写总量;补表驱动测试(1+1、1+3、12 个月第 1 个月再买 1、已叠加态再买 6×2)。 |

注意:

- `live` 取不到(otun-manager GetUser 失败)退回投影行时没有 `traffic_reset_at`:取 `n = 0` 并打 WARNING 日志,不阻塞开通。
- 上线顺序:先 otun-manager,再 fulfillment(老 manager 不返回该字段时 fulfillment 按上一条兜底)。
- `ENTITLEMENT_PROFILES_ENABLED` 生产没开(走旧路径)。新路径里订购桶 `TrafficLimit += in.Traffic` 有同样的问题,**打开开关前必须按本口径改**,本次不改。
- 已知小问题(本次不改):清零按 30 天、到期按自然月,多月单最后 1–2 天会多清零一次。

## 4. 现存受影响用户(生产 11-18 核对)

扫描 `users` / `realm_users` 中 `traffic_reset_at ≈ expire_at` 且未到期的 26 个叠加账号,按订单重放:
只叠加过 1 个月单的账号新旧口径一样,不用动;少给的是叠加了多月单的:

| 用户(auth_user_id 前 8 位) | 面 | 订单 | 现上限 | 应为 | 处理 |
|---|---|---|---|---|---|
| 01b77cab | 住宅 | 10-18 1 个月 100G;11-16 再买 3 个月 100G | 200G | 400G | 补 |
| 136c0fe8 | 住宅 | 10-20 1 个月 100G;11-13 再买 3 个月 100G | 200G | 400G | 补 |
| e9dc1502 | 住宅 | 1 + 3 个月 | 200G | 400G | 不补:11-12 盗刷案,已拉黑停用 |
| ebd22182 | 住宅 | 12 + 3 + 6×2 个月 | 400G | 2700G | 不补:11-12 盗刷案,已拉黑停用 |

### 补齐 SQL(11-18 已执行,两库各更新 2 行)

```sql
-- otun_manager_db
BEGIN;
SET statement_timeout = '10s';
UPDATE realm_users
   SET traffic_limit = 400::bigint * 1073741824, updated_at = NOW()
 WHERE product_face = 'residential'
   AND auth_user_id IN ('01b77cab-8a38-4430-be1d-6110dec9dcb2', '136c0fe8-9e27-4df7-a394-ddbcfce2dd6f')
   AND traffic_limit = 200::bigint * 1073741824;
-- 预期 UPDATE 2,否则 ROLLBACK
COMMIT;

-- fulfillment_db(投影行,保持与 otun 一致)
BEGIN;
SET statement_timeout = '10s';
UPDATE fulfillment.vpn_provisions
   SET traffic_limit = 400::bigint * 1073741824, updated_at = NOW()
 WHERE is_current = true AND service_tier = 'residential'
   AND user_id IN ('01b77cab-8a38-4430-be1d-6110dec9dcb2', '136c0fe8-9e27-4df7-a394-ddbcfce2dd6f')
   AND traffic_limit = 200::bigint * 1073741824;
-- 预期 UPDATE 2,否则 ROLLBACK
COMMIT;
```

`traffic_used`、`expire_at`、`traffic_reset_at` 都不动。执行后核对:两人 `realm_users.traffic_limit` = 400G、`enabled = true`,
App 显示的剩余流量相应增加。
