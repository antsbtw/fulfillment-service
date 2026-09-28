-- 新式托管机(回执 BACKEND_ANSWERS_APP_PLATFORM_AND_HOSTING_2026-09-28 R-12)。
-- node_kind:hosted_v2 = 带 owner key 开通、走应用平台;hosted_legacy = 老式一体化(存量全部)。
-- needs_rebuild:换套餐时新式机不自动重建,由 App 引导用户删除重建。
ALTER TABLE fulfillment.hosting_provisions
    ADD COLUMN IF NOT EXISTS node_kind VARCHAR(16) NOT NULL DEFAULT 'hosted_legacy',
    ADD COLUMN IF NOT EXISTS needs_rebuild BOOLEAN NOT NULL DEFAULT FALSE;
