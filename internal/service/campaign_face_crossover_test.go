package service

import (
	"context"
	"testing"

	"github.com/wenwu/saas-platform/fulfillment-service/internal/models"
)

// TestCampaignFace_NoCrossoverWithPaidResidential 守门：已持有【付费住宅套餐】的用户
// 再领住宅活动券时，活动行必须落 promo 面，付费住宅行必须原封不动。
//
// ★2026-08-20 生产上出现过一行 plan_tier=promo 但 product_face=residential 的脏数据
// （住宅活动券落进了付费住宅面），导致该面 current 被顶掉、付费住宅套餐在 /vpn/all 里消失。
// 当时的日志已被重启轮转，未能确证成因；用当时那版代码与当前代码均无法复现。
// 保留本测试钉死这个不变量——真串面时它会失败。
func TestCampaignFace_NoCrossoverWithPaidResidential(t *testing.T) {
	paid := &models.VPNProvision{
		ID: "prov-paid-resi", UserID: "u-repro", ServiceTier: models.ServiceTierResidential,
		ProductFace: models.ProductFaceResidential, PlanTier: "residential",
		Status: models.VPNProvisionStatusActive, IsCurrent: true,
		TrafficLimit: 100 << 30, OtunUUID: ptrStr("uuid-paid-resi"),
	}
	store := &fakeVPNStore{rows: []*models.VPNProvision{paid}}
	s, _ := newCampaignSvc(t, store, newFakeGrantStore())

	if _, err := s.provisionCampaign(context.Background(), &models.ProvisionRequest{
		UserID: "u-repro", SubscriptionID: "sub-repro", UserEmail: "r@e.test",
		PlanTier: models.PlanTierCampaign, Channel: models.PlanTierCampaign,
		ServiceTier: models.ServiceTierResidential,
		ExpireDays:  7, TrafficLimit: 10 << 30,
	}); err != nil {
		t.Fatalf("provision: %v", err)
	}
	for _, r := range store.rows {
		t.Logf("face=%-12q tier=%-12q plan=%-8q limit=%d", r.ProductFace, r.ServiceTier, r.PlanTier, r.TrafficLimit)
	}
	// 付费住宅行必须原封不动
	if paid.TrafficLimit != 100<<30 || paid.ProductFace != models.ProductFaceResidential {
		t.Fatalf("★付费住宅行被改动: %+v", paid)
	}
	// 活动行必须落 promo 面
	var promoRows int
	for _, r := range store.rows {
		if r.PlanTier == models.PlanTierCampaign {
			promoRows++
			if r.ProductFace != models.ProductFaceCampaign {
				t.Fatalf("★活动行 face 串到 %q", r.ProductFace)
			}
		}
	}
	if promoRows != 1 {
		t.Fatalf("want 1 promo row; got %d", promoRows)
	}
}
