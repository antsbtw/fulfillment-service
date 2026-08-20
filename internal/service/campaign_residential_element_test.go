package service

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/wenwu/saas-platform/fulfillment-service/internal/models"
)

// TestCampaignElement_ResidentialUsesRealmConnectURL 是住宅活动券"领到了却连不上"的守门测试。
//
// ★背景：活动面取配置原本只调 SyncUser（/api/users/:uuid/sync），而该口只查 otun users 表。
// 住宅线路的活动账号 uuid 落在 realm_users 表，那里查无此行 → 404 → protocols[] 空。
// 表现是用户领到券、账号也确实建好了，但拉配置拿到空协议列表，连不上且不报错。
// 修复 = 住宅线路改走 realm connect-url（与正式住宅套餐同一条路、同一套装配函数）。
func TestCampaignElement_ResidentialUsesRealmConnectURL(t *testing.T) {
	exp := time.Now().Add(7 * 24 * time.Hour)
	row := campaignRow("u-resi-el", "uuid-promo-resi", exp)
	row.ServiceTier = models.ServiceTierResidential

	store := &fakeVPNStore{rows: []*models.VPNProvision{row}}
	s, _ := newCampaignSvc(t, store, newFakeGrantStore())

	el := s.buildCampaignElement(context.Background(), "u-resi-el")
	if el == nil {
		t.Fatal("campaign element must be present for an active residential promo account")
	}

	// ★核心：必须拿到 realm 协议，不能是空列表
	if len(el.Protocols) == 0 {
		t.Fatal("★REGRESSION: residential promo account got EMPTY protocols — " +
			"user claimed a voucher but cannot connect")
	}
	for _, p := range el.Protocols {
		if !strings.Contains(p.URL, "-realm://") {
			t.Fatalf("residential promo must carry realm URLs; got %q", p.URL)
		}
	}
	// 住宅专属字段应随之下发（与正式住宅面同源）
	if len(el.Nodes) == 0 {
		t.Error("residential promo should carry nodes[] (N=2 出口摘要)")
	}
	if len(el.Regions) == 0 {
		t.Error("residential promo should carry regions[] (授权集)")
	}
	if el.ExitCountry == "" {
		t.Error("residential promo should carry exit_country")
	}
	// 住宅用量真源来自 realm（realm_users），非 vpn_provisions 那列恒 0 的值
	if el.TrafficUsed != 4321 {
		t.Errorf("residential traffic_used should come from realm truth; got %d", el.TrafficUsed)
	}

	// ★契约 C3 不受线路影响：三个分键仍恒为 promo，否则端上会与 basic 面撞 accountKey 互相覆盖
	if el.ServiceTier != models.ProductFaceCampaign ||
		el.PlanTier != models.PlanTierCampaign ||
		el.ProfileClass != models.ProductFaceCampaign {
		t.Fatalf("★契约 C3 破坏：活动面三分键必须恒为 promo；got service_tier=%q plan_tier=%q profile_class=%q",
			el.ServiceTier, el.PlanTier, el.ProfileClass)
	}
	// 活动面不下发 subscribe_url（契约 Q7），线路变化不得引入它
	if el.SubscribeURL != "" {
		t.Errorf("契约 Q7：活动元素不得下发 subscribe_url；got %q", el.SubscribeURL)
	}
}

// TestCampaignElement_StandardUnchanged 锁定标准线路活动元素零回归：
// 仍走 SyncUser，且不得出现住宅专属字段（nodes/regions），保持 golden 形态。
func TestCampaignElement_StandardUnchanged(t *testing.T) {
	exp := time.Now().Add(7 * 24 * time.Hour)
	row := campaignRow("u-std-el", "uuid-promo-std", exp) // campaignRow 默认 ServiceTierStandard
	store := &fakeVPNStore{rows: []*models.VPNProvision{row}}
	s, _ := newCampaignSvc(t, store, newFakeGrantStore())

	el := s.buildCampaignElement(context.Background(), "u-std-el")
	if el == nil {
		t.Fatal("campaign element must be present")
	}
	if len(el.Nodes) != 0 || len(el.Regions) != 0 {
		t.Fatalf("标准线路活动元素不得出现住宅专属字段；nodes=%d regions=%d", len(el.Nodes), len(el.Regions))
	}
	for _, p := range el.Protocols {
		if strings.Contains(p.URL, "-realm://") {
			t.Fatalf("标准线路不得下发 realm URL；got %q", p.URL)
		}
	}
}
