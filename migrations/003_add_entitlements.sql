-- Add entitlements table for trial, gift, promo entitlement management
-- This table tracks what resources users are entitled to and why

CREATE TABLE IF NOT EXISTS fulfillment.entitlements (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id         UUID NOT NULL,                              -- auth-service user UUID
    email           VARCHAR(255) NOT NULL DEFAULT '',
    otun_uuid       VARCHAR(36),                                -- otun-manager VPN user UUID (set after provisioning)
    source          VARCHAR(50) NOT NULL,                       -- trial, gift, purchase, promo
    status          VARCHAR(20) NOT NULL DEFAULT 'active',      -- active, expired, revoked
    traffic_limit   BIGINT NOT NULL DEFAULT 0,                  -- bytes
    traffic_used    BIGINT NOT NULL DEFAULT 0,                  -- bytes (synced from otun-manager)
    expire_at       TIMESTAMPTZ,
    service_tier    VARCHAR(20) NOT NULL DEFAULT 'standard',
    granted_by      VARCHAR(100) DEFAULT 'system',              -- system, admin email, campaign_id
    note            TEXT DEFAULT '',
    device_id       VARCHAR(255) DEFAULT '',                    -- device that activated
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Indexes
CREATE INDEX idx_entitlements_user_id ON fulfillment.entitlements(user_id);
CREATE INDEX idx_entitlements_source ON fulfillment.entitlements(source);
CREATE INDEX idx_entitlements_status ON fulfillment.entitlements(status);

-- Unique constraint: one trial per user (partial unique index)
CREATE UNIQUE INDEX idx_entitlements_user_trial ON fulfillment.entitlements(user_id) WHERE source = 'trial';

-- Auto-update updated_at trigger
CREATE TRIGGER trigger_entitlements_updated_at
    BEFORE UPDATE ON fulfillment.entitlements
    FOR EACH ROW EXECUTE FUNCTION fulfillment.update_updated_at();
