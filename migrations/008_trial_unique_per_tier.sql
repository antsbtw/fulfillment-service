-- 008: trial 防滥用唯一约束加 service_tier 维度（basic/residential 解耦配套）
--
-- 背景：006 建的三个 trial 唯一约束（user/device/email）假设"每用户/设备/email 只能一条 trial"，
-- 是 basic/residential 解耦【之前】的模型。解耦后 standard 与 residential 是两类独立服务，
-- 应允许"每类各一条 trial"（同一设备可同时有 standard trial + residential trial）。
-- 旧约束（仅 device_id 等单列）会让第二条 trial provision 撞 23505 唯一冲突 → residential 履约失败。
--
-- 修复：把唯一键从 (单列) 改为 (单列, service_tier)，即"每 <维度> 每 tier 一条 trial"。
-- service_tier 取值 standard/residential（已验证现有 trial 数据全为 standard，无 NULL，无冲突）。
--
-- 与上游已修配套：subscription 侧 trial 去重已按 tier 分区（GetTrialByUserAndTier 等），
-- fulfillment 侧 MULTI_SERVICE_ENABLED=true 让 residential 走独立 provision，本约束放行第二条。

-- user 维度
DROP INDEX IF EXISTS fulfillment.idx_vpn_prov_user_trial;
CREATE UNIQUE INDEX idx_vpn_prov_user_trial
    ON fulfillment.vpn_provisions(user_id, service_tier)
    WHERE business_type = 'trial';

-- device 维度（撞这个导致 residential 500 的元凶）
DROP INDEX IF EXISTS fulfillment.idx_vpn_prov_device_trial;
CREATE UNIQUE INDEX idx_vpn_prov_device_trial
    ON fulfillment.vpn_provisions(device_id, service_tier)
    WHERE business_type = 'trial' AND device_id IS NOT NULL AND device_id != '';

-- email 维度
DROP INDEX IF EXISTS fulfillment.idx_vpn_prov_email_trial;
CREATE UNIQUE INDEX idx_vpn_prov_email_trial
    ON fulfillment.vpn_provisions(email, service_tier)
    WHERE business_type = 'trial' AND email IS NOT NULL AND email != '';
