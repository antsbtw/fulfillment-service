package service

import (
	"context"
	"testing"
	"time"

	"github.com/wenwu/saas-platform/fulfillment-service/internal/models"
)

// TestProvisionCampaign_TierRoutedToOtun 锁定线路透传（迁移 003 / D1）：
// 活动权益的 service_tier 必须原样发给 otun-manager——otun 侧正是靠它分流到
// realm_users（住宅腿）还是 users（标准腿）。发错 = 用户拿到错误线路的账号。
func TestProvisionCampaign_TierRoutedToOtun(t *testing.T) {
	for _, tc := range []struct {
		name     string
		reqTier  string
		wantTier string
	}{
		{"residential 透传", models.ServiceTierResidential, models.ServiceTierResidential},
		{"standard 透传", models.ServiceTierStandard, models.ServiceTierStandard},
		{"空值兜底 standard（存量 claim / 未升级的 campaign-service）", "", models.ServiceTierStandard},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeVPNStore{}
			s, otun := newCampaignSvc(t, store, newFakeGrantStore())

			_, err := s.provisionCampaign(context.Background(), &models.ProvisionRequest{
				UserID: "u-tier", SubscriptionID: "sub-tier-1", UserEmail: "t@example.com",
				PlanTier: models.PlanTierCampaign, ServiceTier: tc.reqTier,
				ExpireDays: 7, TrafficLimit: 10 * 1024 * 1024 * 1024,
			})
			if err != nil {
				t.Fatalf("provisionCampaign: %v", err)
			}
			if len(otun.posts) != 1 {
				t.Fatalf("want 1 otun CreateUser; got %d", len(otun.posts))
			}
			if got := otun.posts[0].ServiceTier; got != tc.wantTier {
				t.Fatalf("otun service_tier: want %q got %q", tc.wantTier, got)
			}
			// product_face 恒为 promo——线路变了也仍是活动面，不能串进 basic/residential 正式面。
			if got := otun.posts[0].ProductFace; got != models.ProductFaceCampaign {
				t.Fatalf("product_face must stay promo; got %q", got)
			}
		})
	}
}

// TestProvisionCampaign_CrossTierDoesNotReuseUUID 是 D3 的守门测试：
// 用户先领 standard 活动券、再领 residential 活动券时，必须新建独立 otun 账号，
// 【不得】复用 standard 那行的 otun_uuid。
//
// ★为什么会错：promo 面下两条线路各是一行，但按面取 current 只会拿到一行。若不按线路细分，
// residential 这次开通会复用 standard 的 uuid——而该 uuid 只存在于 otun users 表，
// residential 腿在 realm_users 里查无此账号，额度/到期会落到错误的地方，且不报错。
func TestProvisionCampaign_CrossTierDoesNotReuseUUID(t *testing.T) {
	store := &fakeVPNStore{}
	// 已有：standard 线路的活动账号
	exp := time.Now().Add(7 * 24 * time.Hour)
	stdRow := campaignRow("u-cross", "uuid-promo-standard", exp)
	stdRow.ServiceTier = models.ServiceTierStandard
	store.rows = append(store.rows, stdRow)

	s, otun := newCampaignSvc(t, store, newFakeGrantStore())

	// 再领一张 residential 活动券
	_, err := s.provisionCampaign(context.Background(), &models.ProvisionRequest{
		UserID: "u-cross", SubscriptionID: "sub-cross-resi", UserEmail: "t@example.com",
		PlanTier: models.PlanTierCampaign, ServiceTier: models.ServiceTierResidential,
		ExpireDays: 7, TrafficLimit: 10 * 1024 * 1024 * 1024,
	})
	if err != nil {
		t.Fatalf("provisionCampaign residential: %v", err)
	}
	if len(otun.posts) != 1 {
		t.Fatalf("residential claim must create its own otun account; got %d posts", len(otun.posts))
	}
	if got := otun.posts[0].ServiceTier; got != models.ServiceTierResidential {
		t.Fatalf("want residential tier sent to otun; got %q", got)
	}
	if otun.posts[0].UUID == "uuid-promo-standard" {
		t.Fatal("★REGRESSION: residential claim reused the standard promo account's uuid; " +
			"the provision would land in the wrong otun table")
	}
}
