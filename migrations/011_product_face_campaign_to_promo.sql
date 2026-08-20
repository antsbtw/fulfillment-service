-- ============================================================
-- Migration: 011_product_face_campaign_to_promo.sql
-- Purpose : 第三产品面枚举值改名 campaign → promo（2026-08-20，客户端确认清单 Q3）
--   起因：credit-service 早有一套 GiftCampaign（活动赠送【正式订阅】，落地页 /v3/campaigns/claim），
--   与本模块的第三产品面（独立 profile 面，落地页 /c/<token>）同名不同物。两套并存会让用户侧
--   "我的活动"页出现两种性质完全不同的"活动"，也让三端枚举撞名。故本产品面改名 promo。
--
--   边界（已拍板）：只改【产品面枚举值】。服务仍叫 campaign-service，表仍叫 campaigns/campaign_claims，
--   路由仍是 /api/v1/campaigns/*，落地页仍是 /c/<token>——已发出的 token/二维码不受影响。
--
--   改的三列字面值：
--     1. fulfillment.vpn_provisions.product_face : 'campaign' → 'promo'（分区键，必须改）
--     2. fulfillment.vpn_provisions.plan_tier    : 'campaign' → 'promo'
--     3. fulfillment.vpn_provisions.channel      : 'campaign' → 'promo'
--   ★service_tier 不动：DB 里仍是 'standard'（节点面确为标准节点）。下发值另在响应装配处置为 promo
--     （契约 C3 修订 / Q1 方案 B），两者本就不同源。
--
-- 上线顺序：本迁移与新代码【必须同时上】。新代码的分区谓词是 product_face='promo'，
--   旧数据若未改名，campaign 行会既不属于 promo 分区、也不属于 basic 分区（basic 分区显式
--   排除 <> 'promo'，而旧值是 'campaign' → 会被 basic 分区【命中】）→ 串面风险。
--   故：先跑迁移，再滚代码；同一维护窗口内完成。
-- 回退：把 'promo' 改回 'campaign' 即可（下方 DOWN 段），配合回滚代码。
-- 幂等：WHERE 子句限定旧值，重复执行零影响。
-- ============================================================

UPDATE fulfillment.vpn_provisions SET product_face = 'promo' WHERE product_face = 'campaign';
UPDATE fulfillment.vpn_provisions SET plan_tier    = 'promo' WHERE plan_tier    = 'campaign';
UPDATE fulfillment.vpn_provisions SET channel      = 'promo' WHERE channel      = 'campaign';

-- 对账：三列均应为 0 行残留。
-- SELECT count(*) FROM fulfillment.vpn_provisions
--  WHERE product_face = 'campaign' OR plan_tier = 'campaign' OR channel = 'campaign';

-- ---------------------- DOWN（回退用，勿默认执行） ----------------------
-- UPDATE fulfillment.vpn_provisions SET product_face = 'campaign' WHERE product_face = 'promo';
-- UPDATE fulfillment.vpn_provisions SET plan_tier    = 'campaign' WHERE plan_tier    = 'promo';
-- UPDATE fulfillment.vpn_provisions SET channel      = 'campaign' WHERE channel      = 'promo';
