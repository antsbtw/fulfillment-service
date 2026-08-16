-- ============================================================
-- Migration: 010_vpn_provisions_product_face.sql
-- Purpose : 第三产品面 campaign（营销活动 profile，document/marketing-campaign/*，2026-08-16）
--   1. vpn_provisions 加分区键 product_face（basic | residential | campaign）。
--      此前分区谓词是 (service_tier='residential')=$2：campaign 行 service_tier='standard' 会被 basic
--      分区命中并可能成为 basic 的 current → 串面。改按 product_face 分区，basic 分区显式排除 campaign。
--      存量回填：service_tier='residential' → residential；其余 → basic（默认值）。
--   2. campaign_grants：活动账号入账台账（每次领取一条，按 subscription_id 幂等；撤销标 revoked；
--      到期后再领开新周期时旧 grant 标 expired）。/vpn/all campaign 元素的 campaign{} 子对象由它聚合。
-- 上线顺序：本迁移必须先于 fulfillment 新代码（读路径 vpnColumns 含 product_face）。回退：DROP COLUMN /
-- DROP TABLE 即可（旧代码不读这两个对象）。
-- ============================================================

ALTER TABLE fulfillment.vpn_provisions
    ADD COLUMN IF NOT EXISTS product_face VARCHAR(16) NOT NULL DEFAULT 'basic';

UPDATE fulfillment.vpn_provisions
   SET product_face = 'residential'
 WHERE service_tier = 'residential' AND product_face <> 'residential';

CREATE INDEX IF NOT EXISTS idx_vpn_provisions_user_face_current
    ON fulfillment.vpn_provisions(user_id, product_face) WHERE is_current = TRUE;

CREATE TABLE IF NOT EXISTS fulfillment.campaign_grants (
    subscription_id   VARCHAR(256) PRIMARY KEY,          -- subscription-service 行 id（一次领取一条；幂等键）
    user_id           VARCHAR(256) NOT NULL,
    channel_sub_id    VARCHAR(256),                       -- campaign-claim-<claim_id>（排障用）
    days              INT    NOT NULL DEFAULT 0,
    traffic_bytes     BIGINT NOT NULL DEFAULT 0,
    status            VARCHAR(16) NOT NULL DEFAULT 'active',  -- active | revoked | expired
    applied_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    revoked_at        TIMESTAMPTZ,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_campaign_grants_user_status
    ON fulfillment.campaign_grants(user_id, status);
