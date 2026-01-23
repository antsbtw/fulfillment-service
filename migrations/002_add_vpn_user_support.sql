-- Add VPN user support
-- This migration adds an index for VPN user resources

-- Comment on resource_type values
COMMENT ON COLUMN fulfillment.resources.resource_type IS
    'Resource type: hosting_node (VPS), otun_node (OTun managed node), vpn_user (VPN subscription user)';

-- Index for VPN user queries
CREATE INDEX IF NOT EXISTS idx_resources_vpn_user
ON fulfillment.resources(user_id, resource_type)
WHERE resource_type = 'vpn_user' AND deleted_at IS NULL;

-- Index for active resources by user and type
CREATE INDEX IF NOT EXISTS idx_resources_user_type_active
ON fulfillment.resources(user_id, resource_type, status)
WHERE status = 'active' AND deleted_at IS NULL;
