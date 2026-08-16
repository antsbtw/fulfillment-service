package repository

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/wenwu/saas-platform/fulfillment-service/internal/models"
)

// 真 PG 分区隔离测试（迁移 010 product_face）。未配 TEST_DATABASE_URL 则 skip（沿 otun-manager 仓惯例）。
// 前置：目标库已跑 migrations/001..010（本机：postgres://test:test@127.0.0.1:5456/fulfillment_test）。
func newTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping vpn_provisions product_face DB test")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func TestVPNProvisionRepo_ProductFacePartition(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	repo := NewVPNProvisionRepository(pool)
	grants := NewCampaignGrantRepository(pool)
	const userID = "face-test-user"
	cleanup := func() {
		_, _ = pool.Exec(ctx, `DELETE FROM fulfillment.vpn_provisions WHERE user_id = $1`, userID)
		_, _ = pool.Exec(ctx, `DELETE FROM fulfillment.campaign_grants WHERE user_id = $1`, userID)
	}
	cleanup()
	t.Cleanup(cleanup)

	// 列是 uuid 类型：用固定 uuid 字面量
	pBasic, pCamp, pRes := "11111111-1111-1111-1111-000000000001", "11111111-1111-1111-1111-000000000002", "11111111-1111-1111-1111-000000000003"
	exp := time.Now().Add(24 * time.Hour)
	basicUUID, campUUID, resUUID := "otun-basic", "otun-camp", "otun-res"
	rows := []*models.VPNProvision{
		{ID: pBasic, UserID: userID, SubscriptionID: "sub-basic", Channel: "stripe", BusinessType: "subscription",
			ServiceTier: models.ServiceTierStandard, OtunUUID: &basicUUID, PlanTier: "basic", Status: "active",
			TrafficLimit: 1, ExpireAt: &exp, IsCurrent: true},
		{ID: pCamp, UserID: userID, SubscriptionID: "sub-camp", Channel: "campaign", BusinessType: "gift",
			ServiceTier: models.ServiceTierStandard, OtunUUID: &campUUID, PlanTier: "campaign", Status: "active",
			TrafficLimit: 2, ExpireAt: &exp, IsCurrent: true}, // ★product_face 留空 → Create 推导为 campaign
		{ID: pRes, UserID: userID, SubscriptionID: "sub-res", Channel: "apple", BusinessType: "subscription",
			ServiceTier: models.ServiceTierResidential, OtunUUID: &resUUID, PlanTier: "residential", Status: "active",
			TrafficLimit: 3, ExpireAt: &exp, IsCurrent: true},
	}
	for _, r := range rows {
		if err := repo.Create(ctx, r); err != nil {
			t.Fatalf("create %s: %v", r.ID, err)
		}
	}

	// 1. basic 分区（isResidential=false）只见 basic，绝不命中 campaign（campaign 行 service_tier 也是 standard，
	//    且 created_at 更晚——旧谓词 (service_tier='residential')=false 会取到它）。
	got, err := repo.GetCurrentByUserAndServicePartition(ctx, userID, false)
	if err != nil || got == nil || got.ID != pBasic {
		t.Fatalf("basic partition: want p-basic, got %+v err=%v", got, err)
	}
	if got.ProductFace != models.ProductFaceBasic {
		t.Fatalf("basic row product_face = %q", got.ProductFace)
	}
	if u, _ := repo.GetOtunUUIDByUserAndServicePartition(ctx, userID, false); u == nil || *u != basicUUID {
		t.Fatalf("basic partition uuid: got %v", u)
	}
	// 2. residential 分区只见 residential。
	if got, _ = repo.GetCurrentByUserAndServicePartition(ctx, userID, true); got == nil || got.ID != pRes {
		t.Fatalf("residential partition: got %+v", got)
	}
	// 3. campaign 面只经 *AndFace(campaign) 可见。
	if got, _ = repo.GetCurrentByUserAndFace(ctx, userID, models.ProductFaceCampaign); got == nil || got.ID != pCamp || got.ProductFace != models.ProductFaceCampaign {
		t.Fatalf("campaign face: got %+v", got)
	}
	if u, _ := repo.GetOtunUUIDByUserAndFace(ctx, userID, models.ProductFaceCampaign); u == nil || *u != campUUID {
		t.Fatalf("campaign face uuid: got %v", u)
	}
	// 4. 不分区的 user 级读口全部排除 campaign（契约 C2：/vpn 单条、/my/vpn、换绑回收、邮箱同步都走它们）。
	if got, _ = repo.GetCurrentByUser(ctx, userID); got == nil || got.EffectiveProductFace() == models.ProductFaceCampaign {
		t.Fatalf("GetCurrentByUser must not return campaign row: %+v", got)
	}
	if got, _ = repo.GetCurrentByUserAnyStatus(ctx, userID); got == nil || got.EffectiveProductFace() == models.ProductFaceCampaign {
		t.Fatalf("GetCurrentByUserAnyStatus must not return campaign row: %+v", got)
	}
	if u, _ := repo.GetOtunUUIDByUser(ctx, userID); u == nil || *u == campUUID {
		t.Fatalf("GetOtunUUIDByUser must not return campaign uuid: %v", u)
	}
	// 5. 只留 campaign 行时，不分区读口返回 ErrNotFound（campaign-only 用户对老路径 = 无 provision）。
	_, _ = pool.Exec(ctx, `UPDATE fulfillment.vpn_provisions SET is_current = FALSE WHERE id IN ($1, $2)`, pBasic, pRes)
	if _, err := repo.GetCurrentByUser(ctx, userID); err != ErrNotFound {
		t.Fatalf("campaign-only user: GetCurrentByUser want ErrNotFound, got %v", err)
	}
	if got, _ = repo.GetCurrentByUserAndFace(ctx, userID, models.ProductFaceCampaign); got == nil {
		t.Fatalf("campaign-only user: campaign face must still be visible")
	}
	// 6. Update 重算 product_face（原地续期 standard→residential 会换分区；campaign 恒 campaign）。
	got.TrafficLimit = 99
	if err := repo.Update(ctx, got); err != nil {
		t.Fatalf("update: %v", err)
	}
	if got, _ = repo.GetCurrentByUserAndFace(ctx, userID, models.ProductFaceCampaign); got == nil || got.TrafficLimit != 99 {
		t.Fatalf("after update: %+v", got)
	}
	// 7. cleanup 列表：expire_at 早于阈值的 campaign 行
	past := time.Now().Add(-10 * 24 * time.Hour)
	_, _ = pool.Exec(ctx, `UPDATE fulfillment.vpn_provisions SET expire_at = $1 WHERE id = $2`, past, pCamp)
	list, err := repo.ListExpiredCampaignRows(ctx, time.Now().Add(-7*24*time.Hour), 10)
	if err != nil || len(list) != 1 || list[0].ID != pCamp {
		t.Fatalf("ListExpiredCampaignRows: %v %+v", err, list)
	}

	// 8. campaign_grants 幂等 / 撤销幂等 / 聚合 / 周期作废
	ins, err := grants.InsertIfAbsent(ctx, &models.CampaignGrant{SubscriptionID: "sub-camp", UserID: userID, Days: 7, TrafficBytes: 10})
	if err != nil || !ins {
		t.Fatalf("first insert: %v %v", ins, err)
	}
	if ins, _ = grants.InsertIfAbsent(ctx, &models.CampaignGrant{SubscriptionID: "sub-camp", UserID: userID, Days: 7, TrafficBytes: 10}); ins {
		t.Fatalf("replay must not insert")
	}
	_, _ = grants.InsertIfAbsent(ctx, &models.CampaignGrant{SubscriptionID: "sub-camp-2", UserID: userID, Days: 14, TrafficBytes: 20})
	agg, _ := grants.AggregateActiveByUser(ctx, userID)
	if agg.ClaimsActive != 2 || agg.GrantedDaysTotal != 21 || agg.GrantedTrafficTotal != 30 || agg.LastClaimAt == nil {
		t.Fatalf("aggregate: %+v", agg)
	}
	if ok, _ := grants.MarkRevoked(ctx, "sub-camp"); !ok {
		t.Fatalf("revoke must flip")
	}
	if ok, _ := grants.MarkRevoked(ctx, "sub-camp"); ok {
		t.Fatalf("second revoke must be noop")
	}
	agg, _ = grants.AggregateActiveByUser(ctx, userID)
	if agg.ClaimsActive != 1 || agg.GrantedDaysTotal != 14 {
		t.Fatalf("aggregate after revoke: %+v", agg)
	}
	if err := grants.ExpireActiveByUser(ctx, userID); err != nil {
		t.Fatalf("expire: %v", err)
	}
	agg, _ = grants.AggregateActiveByUser(ctx, userID)
	if agg.ClaimsActive != 0 {
		t.Fatalf("aggregate after expire: %+v", agg)
	}
}
