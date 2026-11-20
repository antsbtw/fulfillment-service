package service

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/wenwu/saas-platform/fulfillment-service/internal/models"
)

// 定稿更正(2026-11-20):活动住宅用户的付费住宅行已过期,活动住宅仍有效。
// 声明 campaign-profile 的客户端必须能在 status-all 里找到 (promo, residential) 那条、且是 active;
// 否则按「看住宅条目」会把正在用的活动住宅误停。
func campaignUserRows(userID string) []*models.VPNProvision {
	past, future := time.Now().Add(-48*time.Hour), time.Now().Add(72*time.Hour)
	face := models.ProductFaceCampaign
	return []*models.VPNProvision{
		{ID: "p-res", UserID: userID, ServiceTier: models.ServiceTierResidential, PlanTier: "residential",
			Status: models.VPNProvisionStatusActive, IsCurrent: true, ExpireAt: &past},
		{ID: "p-promo-res", UserID: userID, ServiceTier: models.ServiceTierResidential, PlanTier: models.PlanTierCampaign,
			Status: models.VPNProvisionStatusActive, IsCurrent: true, ExpireAt: &future, ProductFace: face},
		{ID: "p-promo-off", UserID: userID, ServiceTier: models.ServiceTierStandard, PlanTier: models.PlanTierCampaign,
			Status: "revoked", IsCurrent: true, ProductFace: face},
	}
}

func TestQuickStatusAll_CampaignCapsAddsPromoLines(t *testing.T) {
	const userID = "u-promo"
	s := newSvc(true, &fakeVPNStore{rows: campaignUserRows(userID)})
	out, err := s.GetUserVPNQuickStatusAllWithCaps(context.Background(), userID, ParseClientCapabilities("campaign-profile"))
	if err != nil {
		t.Fatal(err)
	}
	got := map[[2]string]string{}
	for _, q := range out {
		if q.ServiceTier == "" {
			t.Fatalf("with caps every entry needs service_tier: %+v", q)
		}
		got[[2]string{q.PlanTier, q.ServiceTier}] = q.Status
	}
	if got[[2]string{"residential", "residential"}] != models.VPNProvisionStatusExpired {
		t.Fatalf("paid residential should read expired: %v", got)
	}
	if got[[2]string{models.PlanTierCampaign, "residential"}] != "active" {
		t.Fatalf("promo residential must be present and active: %v", got)
	}
	if _, ok := got[[2]string{models.PlanTierCampaign, "standard"}]; ok {
		t.Fatalf("revoked promo line must not be listed: %v", got)
	}
}

func TestQuickStatusAll_NoCapsUnchanged(t *testing.T) {
	const userID = "u-promo"
	s := newSvc(true, &fakeVPNStore{rows: campaignUserRows(userID)})
	a, err := s.GetUserVPNQuickStatusAll(context.Background(), userID)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := s.GetUserVPNQuickStatusAllWithCaps(context.Background(), userID, ParseClientCapabilities(""))
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	if string(ja) != string(jb) {
		t.Fatalf("no-caps must equal legacy:\n%s\n%s", ja, jb)
	}
	if len(a) != 1 || a[0].PlanTier != "residential" || a[0].ServiceTier != "" {
		t.Fatalf("legacy shape changed: %s", ja)
	}
}
