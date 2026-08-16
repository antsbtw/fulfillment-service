-- ============================================================
-- Migration: 009_entitlement_profiles.sql
-- Purpose : 订阅 / 订购 profile 记账层（形态 B，规则 v2 + 契约 v0.2，2026-08-16）
--   同一用户同一产品面（standard / residential）的多笔权益不再合成一个 expire_at/traffic_limit
--   互相覆盖：按 (user, face, class) 各记一条 profile，条目全部进 entries；下游 otun-manager 账号
--   仍每面一个、uuid 不变，vpn_provisions 退化为该面账号的【投影】（生效 profile 的 expire/traffic）。
-- 上线顺序：本迁移 + 回填可先于代码开关（ENTITLEMENT_PROFILES_ENABLED=false 影子写），回退只关开关，不删表。
-- ============================================================

-- 1. profiles：每 (user_id, service_face, class) 一条
--    class   : subscription | purchase | trial
--    status  : active | waiting | expired | none        （契约 §4.2，权益语义）
--    purchase 桶：days_remaining/traffic_limit 累计入账；active_since/traffic_baseline 生效时记；
--                 days_consumed/traffic_used 跨多次生效累计（回切结算后不清零）
CREATE TABLE IF NOT EXISTS fulfillment.vpn_entitlement_profiles (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id           VARCHAR(256) NOT NULL,
    service_face      VARCHAR(16)  NOT NULL,                 -- standard | residential
    class             VARCHAR(16)  NOT NULL,                 -- subscription | purchase | trial
    status            VARCHAR(16)  NOT NULL DEFAULT 'none',  -- active | waiting | expired | none
    expire_at         TIMESTAMPTZ,
    active_since      TIMESTAMPTZ,
    traffic_limit     BIGINT NOT NULL DEFAULT 0,
    traffic_used      BIGINT NOT NULL DEFAULT 0,
    traffic_baseline  BIGINT NOT NULL DEFAULT 0,             -- 进入生效时该面 otun 账号 traffic_used 基线
    days_remaining    INT    NOT NULL DEFAULT 0,
    days_consumed     INT    NOT NULL DEFAULT 0,
    effective_from    TIMESTAMPTZ,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT uq_vpn_ent_profile UNIQUE (user_id, service_face, class)
);

CREATE INDEX IF NOT EXISTS idx_vpn_ent_profile_user_face
    ON fulfillment.vpn_entitlement_profiles(user_id, service_face);
-- 调度器扫描：生效 profile 即将到期
CREATE INDEX IF NOT EXISTS idx_vpn_ent_profile_active_expire
    ON fulfillment.vpn_entitlement_profiles(expire_at) WHERE status = 'active';

DROP TRIGGER IF EXISTS trigger_vpn_ent_profiles_updated_at ON fulfillment.vpn_entitlement_profiles;
CREATE TRIGGER trigger_vpn_ent_profiles_updated_at
    BEFORE UPDATE ON fulfillment.vpn_entitlement_profiles
    FOR EACH ROW EXECUTE FUNCTION fulfillment.update_updated_at();

-- 2. entries：每笔权益条目（订阅周期 / 一次性订购 / 赠送 / trial），事件幂等键 (channel, channel_sub_id, source_event_id)
--    kind：契约 §4.3 展示分类（apple|google|stripe|stripe_onetime|credit|campaign|gift|gift_card|manual|trial）
CREATE TABLE IF NOT EXISTS fulfillment.vpn_entitlement_entries (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    profile_id        UUID NOT NULL REFERENCES fulfillment.vpn_entitlement_profiles(id) ON DELETE CASCADE,
    subscription_id   VARCHAR(256) NOT NULL DEFAULT '',
    channel           VARCHAR(32)  NOT NULL,
    channel_sub_id    VARCHAR(256) NOT NULL DEFAULT '',
    kind              VARCHAR(32)  NOT NULL,
    purchase_type     VARCHAR(32)  NOT NULL,                 -- subscription | one_time | gift | trial
    days              INT    NOT NULL DEFAULT 0,
    traffic           BIGINT NOT NULL DEFAULT 0,
    period_start      TIMESTAMPTZ,
    period_end        TIMESTAMPTZ,
    granted_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    revoked_at        TIMESTAMPTZ,
    source_event_id   VARCHAR(256) NOT NULL DEFAULT '',
    created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT uq_vpn_ent_entry_event UNIQUE (channel, channel_sub_id, source_event_id)
);

CREATE INDEX IF NOT EXISTS idx_vpn_ent_entry_profile ON fulfillment.vpn_entitlement_entries(profile_id);
CREATE INDEX IF NOT EXISTS idx_vpn_ent_entry_sub     ON fulfillment.vpn_entitlement_entries(subscription_id);

-- 3. vpn_provisions 投影行加 active_class（subscription | purchase | trial | none）
ALTER TABLE fulfillment.vpn_provisions ADD COLUMN IF NOT EXISTS active_class VARCHAR(16);

-- 4. 回填：每条 is_current=TRUE 的 provision → 一条 profile + 一条 entry
--    channel=trial → class=trial；其余 → class=subscription（历史已叠加进账号的一次性天数视为
--    订阅 profile 的一部分，不迁移不回滚，规则 §2）。kind 按契约 §4.3 映射。
--    ★campaign 无法回填：契约要求 channel_sub_id LIKE 'campaign-claim-%'，但 vpn_provisions 没有
--    channel_sub_id 列（subscription_id 是 subscription-service 生成的随机 uuid，不带前缀），
--    存量 credit 一律回填 kind=credit；新事件起 fulfillment 按 ProvisionRequest.channel_sub_id 判 campaign。
--    幂等：profile 按唯一键 ON CONFLICT DO NOTHING；entry 幂等键 = (channel, 'backfill:'||provision.id, 'backfill-009')。
INSERT INTO fulfillment.vpn_entitlement_profiles
    (user_id, service_face, class, status, expire_at, active_since, traffic_limit, traffic_used,
     traffic_baseline, days_remaining, days_consumed, effective_from, created_at, updated_at)
SELECT
    p.user_id,
    CASE WHEN p.service_tier = 'residential' THEN 'residential' ELSE 'standard' END,
    CASE WHEN p.channel = 'trial' THEN 'trial' ELSE 'subscription' END,
    CASE WHEN p.expire_at IS NULL OR p.expire_at > NOW() THEN 'active' ELSE 'expired' END,
    p.expire_at,
    p.created_at,
    COALESCE(p.traffic_limit, 0),
    COALESCE(p.traffic_used, 0),
    0, 0, 0,
    p.created_at,
    p.created_at, NOW()
FROM fulfillment.vpn_provisions p
WHERE p.is_current = TRUE
ON CONFLICT (user_id, service_face, class) DO NOTHING;

INSERT INTO fulfillment.vpn_entitlement_entries
    (profile_id, subscription_id, channel, channel_sub_id, kind, purchase_type, days, traffic,
     period_start, period_end, granted_at, source_event_id, created_at)
SELECT
    pr.id,
    COALESCE(p.subscription_id, ''),
    COALESCE(p.channel, 'manual'),
    'backfill:' || p.id::text,
    CASE
        WHEN p.channel = 'trial'                                          THEN 'trial'
        WHEN p.channel = 'apple'                                          THEN 'apple'
        WHEN p.channel = 'google'                                         THEN 'google'
        WHEN p.channel = 'stripe' AND p.business_type = 'purchase'        THEN 'stripe_onetime'
        WHEN p.channel = 'stripe'                                         THEN 'stripe'
        WHEN p.channel = 'credit'                                         THEN 'credit'
        WHEN p.channel = 'gift'                                           THEN 'gift'
        WHEN p.channel = 'gift_card'                                      THEN 'gift_card'
        ELSE 'manual'
    END,
    CASE WHEN p.channel = 'trial' THEN 'trial' ELSE 'subscription' END,
    GREATEST(0, COALESCE(EXTRACT(EPOCH FROM (p.expire_at - p.created_at)) / 86400, 0))::INT,
    COALESCE(p.traffic_limit, 0),
    p.created_at,
    p.expire_at,
    p.created_at,
    'backfill-009',
    p.created_at
FROM fulfillment.vpn_provisions p
JOIN fulfillment.vpn_entitlement_profiles pr
  ON pr.user_id = p.user_id
 AND pr.service_face = CASE WHEN p.service_tier = 'residential' THEN 'residential' ELSE 'standard' END
 AND pr.class = CASE WHEN p.channel = 'trial' THEN 'trial' ELSE 'subscription' END
WHERE p.is_current = TRUE
ON CONFLICT (channel, channel_sub_id, source_event_id) DO NOTHING;

-- 投影行 active_class 回填（与 profile 同口径）
UPDATE fulfillment.vpn_provisions p
SET active_class = CASE WHEN p.channel = 'trial' THEN 'trial' ELSE 'subscription' END
WHERE p.is_current = TRUE AND p.active_class IS NULL;
