-- 007: 新增 needs_cleanup 字段，用于后台兜底清理失败的 VPS 实例
-- 当 provisionAsync 中 VPS 创建成功但删除失败时，标记此字段为 TRUE
-- CleanupScheduler 定时扫描此字段，重试清理

ALTER TABLE fulfillment.hosting_provisions
    ADD COLUMN IF NOT EXISTS needs_cleanup BOOLEAN NOT NULL DEFAULT FALSE;

-- 部分索引：只索引需要清理的记录，避免全表扫描
CREATE INDEX IF NOT EXISTS idx_hosting_provisions_needs_cleanup
    ON fulfillment.hosting_provisions(needs_cleanup)
    WHERE needs_cleanup = TRUE;
