#!/usr/bin/env bash
# 一次性补齐(2026-11-11):Apple / Google 正式环境有效订阅,节点到期早于平台 period_end 的,
# 把节点到期改成平台 period_end。
#
# 背景:fulfillment 7b1411f 之前对 apple/google 写死"收到通知起 30 天",31 天周期的节点比 Apple
# 早 24–33 小时到期,而续费通知在到期前约 8 小时才到 → 中间断网。代码修好后要等下次续费才对齐,
# 本脚本把当前这一期补齐。设计:subscription-service docs/DESIGN_2026-11-11_CALENDAR_MONTH_PERIOD.md
#
# 用法(在能 ssh v3-db-01 / v3-portal-01 的机器上跑):
#   ./backfill_platform_period_end_20261111.sh            # 演练:只列名单和要做的改动,不写任何东西
#   ./backfill_platform_period_end_20261111.sh --apply    # 执行
#
# 名单(运行时只读计算,不落盘、不进仓库):
#   subscription.otun_subscriptions:channel in (apple, google)、environment=Production、status=active、
#   current_period_end > now()
#   ⨝ fulfillment.vpn_provisions(is_current、status=active、同 subscription_id)
#   ⨝ otun-manager 账号(users 或 realm_users,enabled=true)
#   且 节点到期 < 平台 period_end - 1 分钟,且 差值 ≤ 7 天。
#   差值 > 7 天或节点已停用的(疑似别的问题)、节点比平台晚的,只列出不处理。
#
# 每一行做两件事:
#   1. otun-manager PUT /api/users/:uuid {expire_at, preserve_traffic_used:true}
#      —— 与付费叠加续购同一条口:到期改晚但【不清零已用流量】(不带 preserve 会被当成续期清零)。
#      住宅账号由 otun-manager 自动分流到 realm_users,只改到期。
#   2. fulfillment.vpn_provisions.expire_at 改成同一时间(带旧值条件,防止期间被续费改过)。
# 执行前会再读一次 otun-manager 账号的当前到期,和名单不一致的行跳过。
set -euo pipefail

APPLY=0
[[ "${1:-}" == "--apply" ]] && APPLY=1

DB_HOST=v3-db-01
API_HOST=v3-portal-01
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

psql_db() { # $1=db  stdin=sql
  ssh "$DB_HOST" "docker exec -i v3-postgres psql -U saas_user -d $1 -At -F '|' -v ON_ERROR_STOP=1"
}
TS="YYYY-MM-DD\"T\"HH24:MI:SS\"Z\""

echo "== 1. 平台侧:Apple/Google 正式环境有效订阅"
psql_db subscription_db >"$TMP/subs" <<SQL
select id, channel, to_char(current_period_end at time zone 'UTC', '$TS')
from subscription.otun_subscriptions
where channel in ('apple','google') and environment ilike 'production'
  and status = 'active' and current_period_end > now();
SQL
echo "   $(wc -l <"$TMP/subs") 个"
[[ -s "$TMP/subs" ]] || { echo "没有订阅,结束"; exit 0; }

IDS=$(cut -d'|' -f1 "$TMP/subs" | sed "s/.*/'&'/" | paste -sd,)
psql_db fulfillment_db >"$TMP/prov" <<SQL
select subscription_id, id, otun_uuid, to_char(expire_at at time zone 'UTC', '$TS')
from fulfillment.vpn_provisions
where is_current and status = 'active' and otun_uuid is not null and expire_at is not null
  and subscription_id in ($IDS);
SQL

UUIDS=$(cut -d'|' -f3 "$TMP/prov" | sed "s/.*/'&'/" | paste -sd,)
psql_db otun_manager_db >"$TMP/otun" <<SQL
select uuid, to_char(expire_at at time zone 'UTC', '$TS'), enabled from users where uuid::text in ($UUIDS)
union all
select uuid, to_char(expire_at at time zone 'UTC', '$TS'), enabled from realm_users where uuid::text in ($UUIDS);
SQL

python3 - "$TMP" <<'PY' >"$TMP/plan"
import sys, datetime as dt
T = sys.argv[1]
P = lambda x: dt.datetime.strptime(x, '%Y-%m-%dT%H:%M:%SZ')
subs = {l.split('|')[0]: l.strip().split('|') for l in open(T + '/subs') if l.strip()}
otun = {l.split('|')[0]: l.strip().split('|') for l in open(T + '/otun') if l.strip()}
for l in open(T + '/prov'):
    if not l.strip():
        continue
    sid, pid, uuid, proj = l.strip().split('|')
    _, channel, plat = subs[sid]
    o = otun.get(uuid)
    if not o:
        print(f"SKIP|no-otun-account|{uuid}|{pid}|{channel}|{proj}|{plat}"); continue
    node, enabled = o[1], o[2]
    h = (P(plat) - P(node)).total_seconds() / 3600
    if node != proj:
        # 上次执行改好了 otun、投影行没跟上:只补投影行(otun 到期已等于平台)。
        if abs((P(node) - P(plat)).total_seconds()) < 60 and P(proj) < P(node):
            print(f"PROJ|projection-lagging|{uuid}|{pid}|{channel}|{proj}|{plat}"); continue
        print(f"SKIP|projection!=otun({proj})|{uuid}|{pid}|{channel}|{node}|{plat}"); continue
    if h <= 1 / 60:
        print(f"OK|{'equal' if h > -1/60 else f'node-later-{-h:.1f}h'}|{uuid}|{pid}|{channel}|{node}|{plat}"); continue
    if enabled != 't' or h > 24 * 7:
        print(f"SKIP|special(enabled={enabled},{h:.1f}h)|{uuid}|{pid}|{channel}|{node}|{plat}"); continue
    print(f"FIX|{h:.1f}h|{uuid}|{pid}|{channel}|{node}|{plat}")
PY

echo "== 2. 名单(uuid 只显示前 8 位)"
awk -F'|' '{printf "   %-4s %-28s %s… %-6s node=%s → platform=%s\n",$1,$2,substr($3,1,8),$5,$6,$7}' "$TMP/plan" | sort
echo "   FIX $(grep -c '^FIX' "$TMP/plan" || true) / PROJ $(grep -c '^PROJ' "$TMP/plan" || true) / OK $(grep -c '^OK' "$TMP/plan" || true) / SKIP $(grep -c '^SKIP' "$TMP/plan" || true)"

if [[ $APPLY -eq 0 ]]; then
  echo "== 演练结束(没有写任何东西)。确认后加 --apply 执行。"
  exit 0
fi

echo "== 3. 执行"
ok=0; fail=0
while IFS='|' read -r kind _ uuid pid channel node plat; do
  [[ "$kind" == "FIX" || "$kind" == "PROJ" ]] || continue
  if [[ "$kind" == "PROJ" ]]; then
    n=$(psql_db fulfillment_db <<SQL
with u as (
  update fulfillment.vpn_provisions set expire_at = '$plat'::timestamptz, updated_at = now()
  where id = '$pid' and date_trunc('second', expire_at) = '$node'::timestamptz returning 1)
select count(*) from u;
SQL
)
    echo "   PROJ ${uuid:0:8}… projection $node → $plat (rows=$n)"; ok=$((ok+1)); continue
  fi
  # 3a. 再读一次 otun-manager 库里的当前到期(users / realm_users),和名单不一致就跳过(期间可能已续费)。
  #     不用 GET /api/users/:uuid:它对住宅 uuid 返回 404。
  cur=$(psql_db otun_manager_db <<SQL
select to_char(expire_at at time zone 'UTC', '$TS') from users where uuid::text = '$uuid'
union all
select to_char(expire_at at time zone 'UTC', '$TS') from realm_users where uuid::text = '$uuid';
SQL
)
  if [[ "$cur" != "$node" ]]; then
    echo "   SKIP ${uuid:0:8}… 当前到期 '$cur' ≠ 名单 '$node'"; fail=$((fail+1)); continue
  fi
  # 3b. otun-manager:改到期,保留已用流量
  code=$(ssh -n "$API_HOST" "SECRET=\$(docker exec fulfillment-service printenv INTERNAL_SECRET); \
        URL=\$(docker exec fulfillment-service printenv OTUN_MANAGER_URL); \
        curl -s -o /dev/null -w '%{http_code}' -m 10 -X PUT -H 'Content-Type: application/json' \
          -H \"X-Internal-Secret: \$SECRET\" \"\$URL/api/users/$uuid\" \
          -d '{\"expire_at\":\"$plat\",\"preserve_traffic_used\":true}'")
  if [[ "$code" != "200" ]]; then
    echo "   FAIL ${uuid:0:8}… otun-manager HTTP $code"; fail=$((fail+1)); continue
  fi
  # 3c. fulfillment 投影行(带旧值条件)
  n=$(psql_db fulfillment_db <<SQL
with u as (
  update fulfillment.vpn_provisions set expire_at = '$plat'::timestamptz, updated_at = now()
  where id = '$pid' and date_trunc('second', expire_at) = '$node'::timestamptz returning 1)
select count(*) from u;
SQL
)
  echo "   DONE ${uuid:0:8}… $channel $node → $plat (projection rows=$n)"
  ok=$((ok+1))
done <"$TMP/plan"
echo "== 完成:成功 $ok,跳过/失败 $fail。再跑一次演练核对应全部为 OK。"
