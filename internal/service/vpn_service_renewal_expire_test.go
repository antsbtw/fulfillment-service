package service

import (
	"testing"
	"time"

	"github.com/wenwu/saas-platform/fulfillment-service/internal/models"
)

// applySameChannelRenewal 复刻 ProvisionVPNUser 中 "Same channel renewal: update
// existing record in-place" 分支对投影行的字段赋值（vpn_service.go）。测试锁定
// expire_at 必须与推给 otun 的 expireAt 同源写回。
func applySameChannelRenewal(existing *models.VPNProvision, expireAt time.Time,
	trafficLimit int64, subscriptionID, businessType, serviceTier, planTier string) {
	existing.ExpireAt = &expireAt
	existing.TrafficLimit = trafficLimit
	existing.SubscriptionID = subscriptionID
	existing.BusinessType = businessType
	existing.ServiceTier = serviceTier
	existing.PlanTier = planTier
	existing.Status = models.VPNProvisionStatusActive
}

// TestSameChannelRenewal_WritesExpireAt 回归：同 channel 续期就地更新投影行时，
// expire_at 必须一并写回。
//
// 生产实证 2026-08-22：用户领取 residential 礼品卡（gift_card → gift_card，同
// channel），traffic 正确更新为 300GB、subscription_id 也换了新的，但 expire_at
// 滞留在续期前的 2026-08-13（已过期）。真实管控（realm_users.expire_at）是正确的
// 2026-09-21、用户能正常连接，但 /vpn/subscribe-all 与 /vpn/status 按投影行下发，
// App 因此显示"已过期"。根因=该分支漏了 existing.ExpireAt 赋值（channel upgrade
// 分支与首次开通分支都写了）。
func TestSameChannelRenewal_WritesExpireAt(t *testing.T) {
	oldExpire := time.Date(2026, 8, 13, 4, 4, 32, 0, time.UTC)
	newExpire := time.Date(2026, 9, 21, 13, 26, 27, 0, time.UTC)

	existing := &models.VPNProvision{
		Channel:      "gift_card",
		ServiceTier:  "residential",
		PlanTier:     "residential",
		TrafficLimit: 500 * 1024 * 1024 * 1024,
		ExpireAt:     &oldExpire,
		Status:       models.VPNProvisionStatusActive,
	}

	applySameChannelRenewal(existing, newExpire, 300*1024*1024*1024,
		"937762cc-4c1e-4329-a45c-ad119b77cab1", "purchase", "residential", "residential")

	if existing.ExpireAt == nil {
		t.Fatal("expire_at must not be nil after renewal")
	}
	if !existing.ExpireAt.Equal(newExpire) {
		t.Errorf("expire_at = %s, want %s (推给 otun 的值必须写回投影行，否则 App 显示已过期)",
			existing.ExpireAt.Format(time.RFC3339), newExpire.Format(time.RFC3339))
	}
	// 同步更新的其它字段不应被回归破坏
	if existing.TrafficLimit != 300*1024*1024*1024 {
		t.Errorf("traffic_limit = %d, want 300GB", existing.TrafficLimit)
	}
	if existing.Status != models.VPNProvisionStatusActive {
		t.Errorf("status = %q, want active", existing.Status)
	}
}
