package service

import (
	"context"
	"testing"
	"time"

	"github.com/wenwu/saas-platform/fulfillment-service/internal/models"
)

// TestCampaignElements_BothTiersEmitted 是矩阵下发的守门测试（契约 v0.6）。
//
// ★背景（2026-08-20 生产实测）：用户同时持有标准活动券与住宅活动券时，
// /vpn/all 只下发了一个活动元素——buildCampaignElement 用 GetCurrentByUserAndFace
// 按面取且 LIMIT 1，两行里只返回最新那条。用户在 App/Web 里看不到另一张券。
//
// 产品面本就是二维矩阵 (plan_tier × service_tier)：
//
//	basic+standard / residential+residential / promo+standard / promo+residential
//
// promo 面有几条线路就该下发几个元素。
func TestCampaignElements_BothTiersEmitted(t *testing.T) {
	exp := time.Now().Add(7 * 24 * time.Hour)

	std := campaignRow("u-matrix", "uuid-promo-std", exp)
	std.ID, std.ServiceTier = "prov-promo-std", models.ServiceTierStandard

	resi := campaignRow("u-matrix", "uuid-promo-resi", exp)
	resi.ID, resi.ServiceTier = "prov-promo-resi", models.ServiceTierResidential

	store := &fakeVPNStore{rows: []*models.VPNProvision{std, resi}}
	s, _ := newCampaignSvc(t, store, newFakeGrantStore())

	els := s.buildCampaignElements(context.Background(), "u-matrix")
	if len(els) != 2 {
		t.Fatalf("★REGRESSION: 同时持有两条线路的活动券时必须下发 2 个元素；got %d", len(els))
	}

	// 顺序固定：residential 在前（service_tier 升序），避免端上误判"配置变了"
	if els[0].ServiceTier != models.ServiceTierResidential ||
		els[1].ServiceTier != models.ServiceTierStandard {
		t.Fatalf("元素顺序必须稳定 [residential, standard]；got [%q, %q]",
			els[0].ServiceTier, els[1].ServiceTier)
	}

	// ★分键二元组必须能区分两个元素——这正是 v0.5 压平 service_tier 后做不到的
	k0 := els[0].PlanTier + "|" + els[0].ServiceTier
	k1 := els[1].PlanTier + "|" + els[1].ServiceTier
	if k0 == k1 {
		t.Fatalf("★两个活动元素算出同一个分键 %q——端上会互相覆盖", k0)
	}

	// 两者都仍是活动产品面
	for i, el := range els {
		if el.PlanTier != models.PlanTierCampaign || el.ProfileClass != models.ProductFaceCampaign {
			t.Fatalf("els[%d] 产品面标识必须是 promo；got plan_tier=%q profile_class=%q",
				i, el.PlanTier, el.ProfileClass)
		}
	}

	// 住宅那个带 realm 专属字段，标准那个不带
	if len(els[0].Nodes) == 0 || len(els[0].Regions) == 0 {
		t.Error("住宅活动元素应带 nodes[]/regions[]")
	}
	if len(els[1].Nodes) != 0 || len(els[1].Regions) != 0 {
		t.Error("标准活动元素不得带 nodes[]/regions[]")
	}
}

// TestCampaignElements_NoAccountEmitsNothing 锁定 C5：无活动账号不追加任何元素。
func TestCampaignElements_NoAccountEmitsNothing(t *testing.T) {
	s, _ := newCampaignSvc(t, &fakeVPNStore{}, newFakeGrantStore())
	if els := s.buildCampaignElements(context.Background(), "nobody"); len(els) != 0 {
		t.Fatalf("无活动账号时不得下发元素；got %d", len(els))
	}
}
