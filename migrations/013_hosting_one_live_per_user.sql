-- 013: 每个账号最多一条"在用"托管开通记录(2026-11-02)
--
-- 背景:CreateUserNode / Provision 都是"先查 GetActiveByUser 再插入",两台手机同一时刻点开机
-- (或 App 的 POST /my/node 与购买触发的自动开通同时到达)可能都通过检查,开出两台机器、双重计费。
-- 截至 11-02 生产没有出现过重复,本约束是兜底。
--
-- 谓词与 GetActiveByUser 一致:deleted / failed 或已有 deleted_at 的行不占位。
-- 撞约束时 repository 返回 ErrLiveProvisionExists,Provision 改为返回已有那条(幂等)。
-- 服务启动时由 db.EnsureHostingOneLivePerUser 幂等执行,手工再跑也无害。
CREATE UNIQUE INDEX IF NOT EXISTS uq_hosting_prov_user_live
    ON fulfillment.hosting_provisions(user_id)
    WHERE status NOT IN ('deleted', 'failed') AND deleted_at IS NULL;
