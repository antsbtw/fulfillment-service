-- Fulfillment Service Schema
CREATE SCHEMA IF NOT EXISTS fulfillment;

-- Resources table (VPS/Node instances)
CREATE TABLE IF NOT EXISTS fulfillment.resources (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    subscription_id UUID NOT NULL,
    user_id UUID NOT NULL,

    -- Resource type
    resource_type VARCHAR(32) NOT NULL,         -- hosting_node, otun_node

    -- Cloud provider info
    provider VARCHAR(32) NOT NULL,              -- aws, lightsail, digitalocean
    region VARCHAR(32) NOT NULL,
    instance_id VARCHAR(256),                   -- Cloud provider instance ID

    -- Network info
    public_ip VARCHAR(64),
    private_ip VARCHAR(64),

    -- Node configuration (for hosting)
    api_port INT DEFAULT 8080,
    api_key VARCHAR(256),
    vless_port INT DEFAULT 443,
    ss_port INT DEFAULT 8388,
    public_key VARCHAR(256),
    short_id VARCHAR(64),

    -- SSH credentials (encrypted)
    ssh_private_key TEXT,
    ssh_key_name VARCHAR(128),

    -- Status
    status VARCHAR(32) NOT NULL DEFAULT 'pending',  -- pending, creating, running, installing, active, stopping, stopped, deleted, failed
    error_message TEXT,

    -- Plan info
    plan_tier VARCHAR(32) NOT NULL,             -- basic, standard, premium / 1tb, 2tb, 3tb
    traffic_limit BIGINT DEFAULT 0,             -- Traffic limit in bytes
    traffic_used BIGINT DEFAULT 0,              -- Used traffic in bytes

    -- Timestamps
    created_at TIMESTAMPTZ DEFAULT NOW(),
    updated_at TIMESTAMPTZ DEFAULT NOW(),
    ready_at TIMESTAMPTZ,
    deleted_at TIMESTAMPTZ
);

-- Indexes
CREATE INDEX IF NOT EXISTS idx_resources_subscription_id ON fulfillment.resources(subscription_id);
CREATE INDEX IF NOT EXISTS idx_resources_user_id ON fulfillment.resources(user_id);
CREATE INDEX IF NOT EXISTS idx_resources_status ON fulfillment.resources(status);
CREATE INDEX IF NOT EXISTS idx_resources_provider_region ON fulfillment.resources(provider, region);

-- Resource operation logs
CREATE TABLE IF NOT EXISTS fulfillment.resource_logs (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    resource_id UUID NOT NULL REFERENCES fulfillment.resources(id),

    action VARCHAR(32) NOT NULL,                -- provision_started, instance_created, node_ready, etc.
    status VARCHAR(32) NOT NULL,                -- pending, creating, active, failed, etc.
    message TEXT,
    metadata JSONB,

    created_at TIMESTAMPTZ DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_resource_logs_resource_id ON fulfillment.resource_logs(resource_id);
CREATE INDEX IF NOT EXISTS idx_resource_logs_created_at ON fulfillment.resource_logs(created_at DESC);

-- Available regions configuration
CREATE TABLE IF NOT EXISTS fulfillment.regions (
    code VARCHAR(32) PRIMARY KEY,
    name VARCHAR(128) NOT NULL,
    provider VARCHAR(32) NOT NULL,
    available BOOLEAN DEFAULT TRUE,

    created_at TIMESTAMPTZ DEFAULT NOW(),
    updated_at TIMESTAMPTZ DEFAULT NOW()
);

-- Insert default regions
INSERT INTO fulfillment.regions (code, name, provider, available) VALUES
    ('us-east-1', 'US East (Virginia)', 'lightsail', true),
    ('us-west-2', 'US West (Oregon)', 'lightsail', true),
    ('ap-northeast-1', 'Asia Pacific (Tokyo)', 'lightsail', true),
    ('ap-southeast-1', 'Asia Pacific (Singapore)', 'lightsail', true),
    ('eu-west-1', 'Europe (Ireland)', 'lightsail', true)
ON CONFLICT (code) DO NOTHING;

-- Update timestamp trigger
CREATE OR REPLACE FUNCTION fulfillment.update_updated_at()
RETURNS TRIGGER AS $$
BEGIN
    NEW.updated_at = NOW();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trigger_resources_updated_at
    BEFORE UPDATE ON fulfillment.resources
    FOR EACH ROW EXECUTE FUNCTION fulfillment.update_updated_at();

CREATE TRIGGER trigger_regions_updated_at
    BEFORE UPDATE ON fulfillment.regions
    FOR EACH ROW EXECUTE FUNCTION fulfillment.update_updated_at();
