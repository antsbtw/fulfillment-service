-- 新式托管机(回执 BACKEND_ANSWERS_APP_PLATFORM_AND_HOSTING_2026-09-28 R-12)。
-- node_kind:hosted_v2 = 带 owner key 开通、走应用平台;hosted_legacy = 老式一体化(存量全部)。
-- needs_rebuild:换套餐时新式机不自动重建,由 App 引导用户删除重建。
ALTER TABLE fulfillment.hosting_provisions
    ADD COLUMN IF NOT EXISTS node_kind VARCHAR(16) NOT NULL DEFAULT 'hosted_legacy',
    ADD COLUMN IF NOT EXISTS needs_rebuild BOOLEAN NOT NULL DEFAULT FALSE;

-- 新版 App 购买时选择了延迟开通的用户。续费/升级/账号迁移等自动开通事件不带 defer_provision,
-- 靠这张表跟随用户的选择,避免给新版 App 用户自动开出老式机。用户自己调 POST /my/node 不受影响。
CREATE TABLE IF NOT EXISTS fulfillment.hosting_setup_deferred (
    user_id         VARCHAR(64) PRIMARY KEY,
    subscription_id VARCHAR(64),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
