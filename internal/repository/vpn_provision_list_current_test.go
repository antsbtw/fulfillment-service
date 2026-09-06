package repository

import (
	"context"
	"testing"
	"time"

	"github.com/wenwu/saas-platform/fulfillment-service/internal/models"
)

// ListCurrentByUser（账号级封禁/解封联动）的真 PG 断言。未配 TEST_DATABASE_URL 则 skip。
//
// 锁定：所有面（basic / residential / promo）、所有状态（active / suspended / disabled）的 current 行
// 都要返回；非 current 行与别人的行不返回；顺序 面 → 线路 → created_at DESC 稳定。
func TestVPNProvisionRepo_ListCurrentByUser(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	repo := NewVPNProvisionRepository(pool)

	const userID = "list-current-user"
	cleanup := func() {
		_, _ = pool.Exec(ctx, `DELETE FROM fulfillment.vpn_provisions WHERE user_id IN ($1, $2)`, userID, userID+"-other")
	}
	cleanup()
	t.Cleanup(cleanup)

	exp := time.Now().Add(24 * time.Hour)
	mk := func(id, uid, uuid, plan, tier, face, status string, current bool) *models.VPNProvision {
		u := uuid
		return &models.VPNProvision{ID: id, UserID: uid, SubscriptionID: "sub-" + id, Channel: "stripe", BusinessType: "subscription",
			ServiceTier: tier, OtunUUID: &u, PlanTier: plan, ProductFace: face, Status: status,
			TrafficLimit: 1, ExpireAt: &exp, IsCurrent: current}
	}
	rows := []*models.VPNProvision{
		mk("22222222-2222-2222-2222-000000000001", userID, "o-basic", "standard", models.ServiceTierStandard, models.ProductFaceBasic, models.VPNProvisionStatusActive, true),
		mk("22222222-2222-2222-2222-000000000002", userID, "o-resi", "residential", models.ServiceTierResidential, models.ProductFaceResidential, models.VPNProvisionStatusSuspended, true),
		mk("22222222-2222-2222-2222-000000000003", userID, "o-promo", "promo", models.ServiceTierResidential, models.ProductFaceCampaign, models.VPNProvisionStatusDisabled, true),
		mk("22222222-2222-2222-2222-000000000004", userID, "o-stale", "standard", models.ServiceTierStandard, models.ProductFaceBasic, models.VPNProvisionStatusActive, false),
		mk("22222222-2222-2222-2222-000000000005", userID+"-other", "o-other", "standard", models.ServiceTierStandard, models.ProductFaceBasic, models.VPNProvisionStatusActive, true),
	}
	for _, r := range rows {
		if err := repo.Create(ctx, r); err != nil {
			t.Fatalf("create %s: %v", r.ID, err)
		}
	}

	got, err := repo.ListCurrentByUser(ctx, userID)
	if err != nil {
		t.Fatalf("ListCurrentByUser: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d rows, want 3 (all faces, all statuses, current only)", len(got))
	}
	// 顺序：product_face 升序 → basic, promo, residential。
	wantFaces := []string{models.ProductFaceBasic, models.ProductFaceCampaign, models.ProductFaceResidential}
	wantStatus := []string{models.VPNProvisionStatusActive, models.VPNProvisionStatusDisabled, models.VPNProvisionStatusSuspended}
	for i, r := range got {
		if r.EffectiveProductFace() != wantFaces[i] || r.Status != wantStatus[i] {
			t.Fatalf("row %d = face %s status %s, want %s/%s", i, r.EffectiveProductFace(), r.Status, wantFaces[i], wantStatus[i])
		}
		if r.UserID != userID || !r.IsCurrent {
			t.Fatalf("row %d leaked: user=%s current=%v", i, r.UserID, r.IsCurrent)
		}
	}

	// suspended 行经 Update 落库后能被读回（status 列无枚举约束）。
	got[0].Status = models.VPNProvisionStatusSuspended
	if err := repo.Update(ctx, got[0]); err != nil {
		t.Fatalf("update to suspended: %v", err)
	}
	back, err := repo.GetByID(ctx, got[0].ID)
	if err != nil || back == nil || back.Status != models.VPNProvisionStatusSuspended || !back.IsCurrent {
		t.Fatalf("suspended round-trip: err=%v row=%+v", err, back)
	}
	// 单条 GetCurrentByUser 只认 active：suspended 后该面不再被当作"可下发"的 current。
	cur, err := repo.GetCurrentByUser(ctx, userID)
	if err == nil && cur != nil && cur.Status == models.VPNProvisionStatusSuspended {
		t.Fatalf("GetCurrentByUser returned a suspended row: %+v", cur)
	}
}
