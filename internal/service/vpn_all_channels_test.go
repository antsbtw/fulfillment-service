package service

import (
	"context"
	"testing"

	"github.com/wenwu/saas-platform/fulfillment-service/internal/models"
)

// residentialRowNoUUID 模拟住宅面 current provision，OtunUUID 留空以避免触达 otunClient
// （buildQuickStatus 仅当 OtunUUID 非空才 SyncUser），从而在无 HTTP/无 mock 下断言分区扇出。
func residentialRowNoUUID(userID string) *models.VPNProvision {
	return &models.VPNProvision{
		ID:          "prov-res",
		UserID:      userID,
		ServiceTier: models.ServiceTierResidential,
		PlanTier:    "residential",
		Status:      models.VPNProvisionStatusActive,
		IsCurrent:   true,
	}
}

func standardRowNoUUID(userID string) *models.VPNProvision {
	return &models.VPNProvision{
		ID:          "prov-std",
		UserID:      userID,
		ServiceTier: models.ServiceTierStandard,
		PlanTier:    "basic",
		Status:      models.VPNProvisionStatusActive,
		IsCurrent:   true,
	}
}

// TestQuickStatusAll_BothFaces：持两面 → 返回两条，标准面与住宅面各一。
func TestQuickStatusAll_BothFaces(t *testing.T) {
	const userID = "u-both"
	store := &fakeVPNStore{rows: []*models.VPNProvision{
		standardRowNoUUID(userID),
		residentialRowNoUUID(userID),
	}}
	s := newSvc(true, store) // MULTI_SERVICE_ENABLED=true，真正按面分区

	out, err := s.GetUserVPNQuickStatusAll(context.Background(), userID)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("want 2 faces, got %d", len(out))
	}
	tiers := map[string]bool{}
	for _, q := range out {
		tiers[q.PlanTier] = true
	}
	if !tiers["basic"] || !tiers["residential"] {
		t.Fatalf("want basic+residential, got %+v", tiers)
	}
}

// TestQuickStatusAll_StandardOnly：只持标准面 → 仅一条，住宅面不进数组。
func TestQuickStatusAll_StandardOnly(t *testing.T) {
	const userID = "u-std"
	store := &fakeVPNStore{rows: []*models.VPNProvision{standardRowNoUUID(userID)}}
	s := newSvc(true, store)

	out, err := s.GetUserVPNQuickStatusAll(context.Background(), userID)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(out) != 1 || out[0].PlanTier != "basic" {
		t.Fatalf("want 1 standard face, got %+v", out)
	}
}

// TestQuickStatusAll_None：无任何 current provision → 空数组（非 nil、非错误）。
func TestQuickStatusAll_None(t *testing.T) {
	store := &fakeVPNStore{}
	s := newSvc(true, store)

	out, err := s.GetUserVPNQuickStatusAll(context.Background(), "nobody")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if out == nil || len(out) != 0 {
		t.Fatalf("want empty non-nil slice, got %#v", out)
	}
}
