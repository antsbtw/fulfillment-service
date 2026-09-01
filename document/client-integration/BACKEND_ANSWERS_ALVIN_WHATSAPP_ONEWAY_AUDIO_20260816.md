# 后端核查回复：Alvin WhatsApp 通话对方听不见我（2026-08-16 诊断报告）

> 对应前端核查要求 Q1–Q4 + 第三节预查。所有数据取自真源（v3-db-01 各库 / fulfillment 内部接口实测 / 标准节点 sing-box 日志），只读，未做任何变更。
> 报告时间窗：2026-08-16T16:17–16:41Z（= 北京 08-17 00:17–00:41，深夜）。

## 0. 一句话结论

- **链路已定：basic 面 / hysteria2 / 出口 SG 主节点（ap-so-1）**，客户端加载 OTun-Basic 是**对的**——他的 residential 面 08-13 已到期被停用。
- **他这次不在新加坡，在上海联通固网 WiFi**（节点日志源 IP `223.166.126.201`，与 6 月已核实的上海联通固网 IP 一致；cf-ray 的 NRT/SIN 是 CF anycast 落点，不能当用户位置）。
- 服务端两节点在窗口内**健康且持续为该 UUID 转发 TCP+UDP**（含到 WhatsApp 网段）；未见报错。
- **节点日志里有一个方向性证据**：通话期间该 UUID 到 WhatsApp 中继 `57.144.15.54:3478` 的 UDP 会话被**反复新建**（16:37:17 / 16:38:41 / 16:39:01 / 16:39:20×2 / 16:39:30 / 16:39:32，每 10–20s 一条新会话）。正常通话应是一条长活会话；这种"不断重开"是 ICE 连通性检查拿不到稳定路径的典型表现，与"上行语音丢"吻合，但**不能坐实丢在哪一跳**。
- 综合（上海联通固网 × 深夜 × hy2=UDP 载体 × 上行连续小包）最像**大陆固网对跨境持续 UDP 流的限速/丢包**（老问题的又一表现，7 月他自己也说过 "VLESS+Reality+Global 才稳"）。**要坐实：让他通话中发一次诊断，并对照换 TCP 系协议（VLESS/Trojan）通话一次。**
- 顺手发现 3 个后端缺陷 + 1 个客户端死路由，见 §5。

## 1. Q1 —— 当前生效订阅是 basic 还是 residential？

**两面都持有，但只有 basic 有效。**

| 面 | 订阅记录 (subscription.otun_subscriptions) | 执行面真源 (otun_manager_db) |
|---|---|---|
| basic | id `72c85dfd`, `com.otun.vpn.basic.500g.monthly`, channel apple, status active, period 06-15 → **08-23**, auto_renew t | `users` uuid **b0ef235c**（=诊断里的 hy2 password）, product_face=basic, **enabled=t, expire 08-28**, 500GB 用 46GB, primary=vpn-ap-so-1(SG) backup=vpn-ap-no-1(JP) |
| residential | id `6dd2bc09`, gift_card 500GB, period 07-14 → **08-13**, DB status 仍写 active（读时判定语义，见 §5） | `realm_users` uuid 243bf44c, **enabled=f, expire 08-13**, 500GB 用 158GB；region GB/JP/SG 三行仍在（GB 为 current, egress-eu-01） |

账号 = auth `1189fd07` alvin@captalyst.com（老邮箱账号）。所以：**客户端拉到 OTun-Basic(id=5)、hy2 到 ap-so-1/ap-no-1、无 realm 痕迹、smart 分流** —— 全部正确，是这个账号此刻唯一能用的面。

⚠️ 08-15 回信承诺的"赠 300GB Residential"**尚未开通**（realm_users 仍是到期停用状态）。若他去点 Residential 会连不上，别和本问题混淆。

## 2. Q2 —— 后端本该下发什么？（fulfillment 内部接口实测，08-17）

`GET /api/internal/vpn/user/1189fd07/subscribe-all`（= BFF `/api/v1/resources/vpn/all`）返回 2 个元素：

1. **basic**：`plan_tier=basic service_tier=standard status=active expire 08-28`，12 条 protocols（vless/ss/vmess/trojan/**hysteria2**/tuic × primary SG + backup JP），`exit_country=SG`，smart_strategy：`stun→direct(仅 74.125.250.129/32,162.159.207.0/32)` / `ip_is_private→direct` / `geosite-cn→direct` / `geoip-cn→direct` / `final=proxy`，`up_mbps=down_mbps=20`。**与诊断 CONFIG 段一致**（客户端未渲染那条 STUN 直连规则，此处不影响：WhatsApp 中继 57.144.15.54 不在该 cidr，本来就走 proxy）。
2. **residential**：`status=active` 但 **`expire_at=2026-08-13`（已过）**，仍下发完整 realm 六协议 + GB 区域。→ 后端把过期面照常标 active 下发（缺陷，§5.1）。

**责任判定：后端下发的 basic 内容正确；客户端选 basic 正确。** 不存在"后端下发 residential 客户端没切"的情况——恰恰相反，是后端多下发了一个已过期的 residential 面还标 active。

## 3. Q3 —— trial/status 为什么没有 tier？

- 路由：`portal /api/trial/` → **payment-service** `TrialService.GetTrialStatus`（services/payment-service/internal/service/trial_service.go:130）。
- 语义：这是**标准面 7 天试用**的状态口（他的试用 06-10→06-17，故 `trial_expires_at=2026-06-17` / `trial_status=expired`）。`has_active_subscription` = subscription-service `CheckActiveSubscription(user, otun, vpn)`，**任一 tier 有效即 true**，模型里根本没有 tier/product 字段（models.go:650 `TrialStatusResponse`）。
- **它不是判 tier 的接口，也不打算加。** 判通道用：
  - `GET /api/v1/resources/vpn/status/all` —— 每面一条 `{status, plan_tier, channel, traffic_*, expire_at}`；
  - `GET /api/v1/subscriptions` —— 订阅记录列表。
  - ⚠️ 单条 `GET /api/v1/resources/vpn/status` **现在会返回 residential**（见 §5.1），暂时别用它判"当前生效 tier"。

## 4. Q4 —— announcements 的 tier 参数怎么用？

**纯内容筛选，不影响任何下发。** communication-service `AnnouncementHandler.List` 只把 `tier` 传给 repo 作 `audience_type='tier' AND audience_key=?` 的 OR 条件（announcement_repo.go:115），空则不筛。传错只会多/少看到定向公告。

前端说要去查 `tier=residential` 从哪取——请查，但**大概率就是从单条 `/vpn/status` 的 `plan_tier` 取的**，那是后端 §5.1 的锅：单条 status 现在返回的是（已过期的）residential。

## 5. 顺手发现的缺陷（后端 3 + 客户端 1）

### 5.1 单条 `/vpn/status`（及 subscribe-all/status-all 的 residential 元素）对已过期面仍 status=active
- 代码：fulfillment `GetUserVPNQuickStatus` → `vpnRepo.GetCurrentByUser`（`is_current AND status='active' ORDER BY created_at DESC LIMIT 1`）→ 取到 07-14 建的 residential provision（比 06-15 的 basic 新），`vpn_provisions.status` 从未被到期事件翻成 expired（otun-manager 侧 cron 停用了 realm_users，但 fulfillment 行不动）。
- 表现：单条 status 返回 `{"status":"active","plan_tier":"residential","traffic_used":0}` **且无 expire_at**（residential 的 uuid 在 realm_users，`otunClient.SyncUser` 打 `/api/users/:uuid` 404 被吞，回填失败）。`status-all` 的 residential 元素同样 `traffic_used:0` 无 `expire_at`；`subscribe-all` 的 residential 元素倒是读了 realm 真源（158GB, expire 08-13）但仍 `status=active`。
- 这就是 CLIENT_REQ_2026-08-16 R1"订阅页显示反了"的后端根因，也是本次 `tier=residential` 残留的最可能来源。
- 修法（待排期）：读时按 `expire_at`/`realm_users.enabled` 判定有效面（形态 B 的 entitlement 开关打开后 `buildQuickStatus` 已有 `ActiveClassNone→expired` 逻辑，生产开关仍 false）；quick status 的 residential 分支改读 realm 真源而不是 `/api/users/:uuid`。

### 5.2 `POST /api/bind` 是死路由
- 诊断 API LOG 首条 `POST https://portal.situstechnologies.com/api/bind` 200，body 却是 `{"service":"saas-platform-v3","status":"ok",...}` —— 这是 portal nginx **`location /` 兜底**的健康 JSON，没有任何服务收到这个请求。客户端拿 200 当成功。请前端确认这个调用是干什么的（疑 v2 遗留设备绑定），要么删，要么给后端提需求。

### 5.3 has_active_subscription 不分面（Q3 已述）——按设计，不改。

## 6. 第三节预查（链路已定 basic/hy2，先给已拿到的）

- **两节点健康**：vpn-sg-01(ap-so-1, 172.26.1.85) / vpn-jp-01(ap-no-1) sing-box 运行中，hy2 8445/udp、tuic 8446/udp 监听；节点 route 为空、outbound 仅 direct，**无 UDP 限制/无 relay 配置**；出口为 Lightsail 静态 IP 1:1 NAT。
- **该 UUID 在窗口内的服务端记录**（SG 节点 16:00–17:00Z）：hy2 693 条 UDP 会话 + 654 条 TCP 会话，来源 `223.166.126.201`（上海联通固网）为主；16:27–16:28 短暂 VLESS（同 IP，疑另一设备或他切协议试了一下）；16:47 起出现 `117.136.119.62`（中国移动蜂窝）与 `223.166.83.64`（联通另一 IP）。JP 备节点同时也有该 UUID 少量流量（urltest / 第二设备）。
- **WhatsApp 相关（全在 SG 主节点）**：
  - TCP 57.144.161.32:443 / 57.144.161.33:5222 / 157.240.15.61:5222 多次成功建连（信令/聊天正常）；
  - UDP 57.144.161.32:443（QUIC）多条；
  - **UDP 57.144.15.54:3478（通话中继）在 16:37:17–16:39:32 被新建 7 次，157.240.15.62:3478 于 16:40:16 又一次** —— 见 §0。
- 节点日志无 error/timeout 行。sing-box 对 UDP 会话不打收尾日志，服务端也看不到单会话字节数——**"上行是否到达中继"服务端日志无法证明**。

## 7. 建议下一步（按性价比）

1. **让 Alvin 通话中（对方听不见的当下）点发送诊断**，同时告诉我们当时是 WiFi 还是蜂窝、在上海还是新加坡。
2. **对照实验**：同一网络下把协议切到 **VLESS(Reality) 或 Trojan（TCP 系）**再打一通；若 TCP 系正常、hy2 单向无声，即坐实"大陆固网对跨境持续 UDP 流不友好"，处理口径 = 大陆固网环境下语音通话建议用 TCP 系协议（可考虑 App 侧在 smart/自动模式下对 CN 固网默认不选 hy2）。
3. 若 TCP 系也单向无声 → 再查 hy2 服务端 UDP 会话超时（默认 5min，不该影响）与 WhatsApp 中继侧对 SG 出口的对待。
4. 后端排期修 §5.1；前端确认 §5.2 `/api/bind`。
5. 补开 300GB Residential 赠礼（走 admin/一个 curl，见运维手册），避免他复测 residential 时撞到期。

## 附：本次用到的锚点/查询法
- otun_manager_db：`users`(basic 面, uuid=hy2 password) / `realm_users`+`realm_user_region`+`realm_user_egress`(residential 面) 按 auth_user_id 查。
- subscription_db：`subscription.otun_subscriptions`；fulfillment_db：`fulfillment.vpn_provisions`。
- 后端实际下发：portal-01 `curl -H "X-Internal-Secret: $SEC" http://127.0.0.1:8014/api/internal/vpn/user/<auth_user_id>/{status,status-all,subscribe-all}`。
- 节点日志：`ssh vpn-sg-01; sudo journalctl -u otun-agent --since ... | grep <uuid>`；`inbound packet connection to` = UDP 会话，`inbound connection to` = TCP；来源 IP 在紧邻上一行 `connection from`。
