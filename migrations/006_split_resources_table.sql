-- ============================================================
-- Migration: 006_split_resources_table.sql
-- Purpose: Split resources table into hosting_provisions + vpn_provisions
--          Merge entitlements into vpn_provisions
-- Strategy: Drop old tables, create new tables (no data migration)
-- ============================================================

-- 1. Drop old tables
DROP TABLE IF EXISTS fulfillment.resource_logs;
DROP TABLE IF EXISTS fulfillment.entitlements;
DROP TABLE IF EXISTS fulfillment.resources;

-- 2. Create hosting_provisions
CREATE TABLE fulfillment.hosting_provisions (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    subscription_id   VARCHAR(256) NOT NULL,
    user_id           VARCHAR(256) NOT NULL,
    channel           VARCHAR(32),

    -- hosting-service reference
    hosting_node_id   VARCHAR(256),
    provider          VARCHAR(32) NOT NULL,
    region            VARCHAR(32) NOT NULL,

    -- Node connection info (cached from hosting-service callback)
    public_ip         VARCHAR(64),
    api_port          INT DEFAULT 8080,
    api_key           VARCHAR(256),
    vless_port        INT DEFAULT 443,
    ss_port           INT DEFAULT 8388,
    public_key        VARCHAR(256),
    short_id          VARCHAR(64),

    -- Status and plan
    status            VARCHAR(32) NOT NULL DEFAULT 'pending',
    error_message     TEXT,
    plan_tier         VARCHAR(32) NOT NULL,
    traffic_limit     BIGINT DEFAULT 0,
    traffic_used      BIGINT DEFAULT 0,

    created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    ready_at          TIMESTAMPTZ,
    deleted_at        TIMESTAMPTZ
);

CREATE INDEX idx_hosting_prov_user ON fulfillment.hosting_provisions(user_id);
CREATE INDEX idx_hosting_prov_sub ON fulfillment.hosting_provisions(subscription_id);
CREATE INDEX idx_hosting_prov_active ON fulfillment.hosting_provisions(user_id, status)
    WHERE status = 'active' AND deleted_at IS NULL;

-- 3. Create vpn_provisions (merged from resources + entitlements)
CREATE TABLE fulfillment.vpn_provisions (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id           VARCHAR(256) NOT NULL,
    subscription_id   VARCHAR(256),
    channel           VARCHAR(32),

    -- Business classification
    business_type     VARCHAR(32) NOT NULL DEFAULT 'purchase',
    service_tier      VARCHAR(32) NOT NULL DEFAULT 'standard',

    -- otun-manager reference
    otun_uuid         VARCHAR(36),

    -- Plan and status
    plan_tier         VARCHAR(32),
    status            VARCHAR(32) NOT NULL DEFAULT 'active',

    -- Traffic and expiry
    traffic_limit     BIGINT DEFAULT 0,
    traffic_used      BIGINT DEFAULT 0,
    expire_at         TIMESTAMPTZ,

    -- Trial/gift fields
    email             VARCHAR(255) DEFAULT '',
    device_id         VARCHAR(255) DEFAULT '',
    granted_by        VARCHAR(100) DEFAULT 'system',
    note              TEXT DEFAULT '',

    -- Current record marker (for trial→purchase conversion history)
    is_current        BOOLEAN NOT NULL DEFAULT TRUE,

    created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_vpn_prov_user ON fulfillment.vpn_provisions(user_id);
CREATE INDEX idx_vpn_prov_sub ON fulfillment.vpn_provisions(subscription_id);
CREATE INDEX idx_vpn_prov_otun ON fulfillment.vpn_provisions(otun_uuid);
CREATE INDEX idx_vpn_prov_biz ON fulfillment.vpn_provisions(business_type, service_tier);
CREATE INDEX idx_vpn_prov_expire ON fulfillment.vpn_provisions(expire_at) WHERE status = 'active';
CREATE INDEX idx_vpn_prov_current ON fulfillment.vpn_provisions(user_id, is_current) WHERE is_current = TRUE;

-- Trial abuse prevention unique indexes
CREATE UNIQUE INDEX idx_vpn_prov_user_trial
    ON fulfillment.vpn_provisions(user_id) WHERE business_type = 'trial';
CREATE UNIQUE INDEX idx_vpn_prov_device_trial
    ON fulfillment.vpn_provisions(device_id)
    WHERE business_type = 'trial' AND device_id IS NOT NULL AND device_id != '';
CREATE UNIQUE INDEX idx_vpn_prov_email_trial
    ON fulfillment.vpn_provisions(email)
    WHERE business_type = 'trial' AND email IS NOT NULL AND email != '';

-- 4. Create provision_logs (renamed from resource_logs)
CREATE TABLE fulfillment.provision_logs (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    provision_id    UUID NOT NULL,
    provision_type  VARCHAR(20) NOT NULL,
    action          VARCHAR(32) NOT NULL,
    status          VARCHAR(32) NOT NULL,
    message         TEXT,
    metadata        JSONB,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_prov_logs_provision ON fulfillment.provision_logs(provision_id);
CREATE INDEX idx_prov_logs_created ON fulfillment.provision_logs(created_at DESC);
