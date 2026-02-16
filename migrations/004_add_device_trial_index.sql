-- 防止试用滥用：同一设备或同一邮箱只允许试用一次
--
-- 场景 1: 用户换邮箱注册新号，同一设备试用 -> device_id 唯一索引拦截
-- 场景 2: 用户删号后用同一邮箱重新注册 -> email 唯一索引拦截

-- 同一 device_id 只允许一条 trial 记录
CREATE UNIQUE INDEX idx_entitlements_device_trial
    ON fulfillment.entitlements(device_id)
    WHERE source = 'trial' AND device_id IS NOT NULL;

-- 同一 email 只允许一条 trial 记录
CREATE UNIQUE INDEX idx_entitlements_email_trial
    ON fulfillment.entitlements(email)
    WHERE source = 'trial' AND email IS NOT NULL AND email != '';
