package service

import (
	"context"
	"testing"
	"time"

	"github.com/wenwu/saas-platform/fulfillment-service/internal/models"
)

// TestLegacy_PlatformPeriodReplay_NoExtensionNoReset 锁定 2026-10-15 link19900511 案：
// 开关 false（生产现状）下，App 恢复购买让 payment 对已验证交易重发 created 事件（同 period_end），
// 旧路径不得再按 fresh period 把 expire 推到 now+30d、也不得 PUT otun 清零已用流量；
// 真续期（新 period_end）照旧放行。
func TestLegacy_PlatformPeriodReplay_NoExtensionNoReset(t *testing.T) {
	svc, stub, _, _, done := newSwitchOnStack(t)
	defer done()
	svc.cfg.Entitlement.Enabled = false // 走旧路径 + 影子写
	svc.entitlement.enabled = false
	ctx := context.Background()

	now := time.Now().UTC().Truncate(time.Second)
	p1Start, p1End := now.Add(-7*24*time.Hour), now.Add(23*24*time.Hour)
	req := func(start, end time.Time) *models.ProvisionRequest {
		return &models.ProvisionRequest{
			AppSource: "otun", BusinessType: "subscription", Channel: "apple", SubscriptionID: "sub-apple",
			UserID: "u-replay", UserEmail: "u@example.test", PlanTier: "basic", PurchaseType: "subscription",
			ChannelSubID: "apple-orig-1", PeriodStart: &start, PeriodEnd: &end, ExpireDays: 30,
		}
	}

	resp, err := svc.ProvisionVPNUser(ctx, req(p1Start, p1End))
	if err != nil {
		t.Fatalf("initial provision: %v", err)
	}
	row := svc.vpnRepo.(*fakeVPNStore).rows[0]
	firstExpire := *row.ExpireAt
	stub.setUsed(resp.VPNUserID, 90*GB)
	putsBefore := len(stub.puts)

	// 让 now+30d 明显晚于首次开通的到期（模拟几天后再点恢复购买）
	row.ExpireAt = ptrTime(firstExpire.Add(-3 * 24 * time.Hour))
	replayExpire := *row.ExpireAt

	r2, err := svc.ProvisionVPNUser(ctx, req(p1Start, p1End))
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if r2.Message != "Already provisioned (idempotent)" {
		t.Fatalf("same-period replay must short-circuit, got message %q", r2.Message)
	}
	if len(stub.puts) != putsBefore {
		t.Fatalf("replay must not PUT otun (would reset traffic/extend expiry): puts %d → %d", putsBefore, len(stub.puts))
	}
	if !row.ExpireAt.Equal(replayExpire) {
		t.Fatalf("replay must not move expire_at: %s → %s", replayExpire, *row.ExpireAt)
	}

	// 真续期：新 period_end → 必须放行并下发
	p2Start, p2End := p1End, p1End.Add(30*24*time.Hour)
	if _, err := svc.ProvisionVPNUser(ctx, req(p2Start, p2End)); err != nil {
		t.Fatalf("renewal: %v", err)
	}
	if len(stub.puts) == putsBefore {
		t.Fatalf("renewal with new period_end must PUT otun")
	}
	if !row.ExpireAt.After(replayExpire) {
		t.Fatalf("renewal must extend expire_at, got %s", *row.ExpireAt)
	}
}

// TestIsPlatformPeriodReplay 用生产实测形态锁判据：开通行 expire 与 period_end 差 0–2 天 = 重放；
// 真续期 period_end 晚约一个周期 = 放行；开通行到期早已过时（漏续期修复）= 放行；非平台渠道不判。
func TestIsPlatformPeriodReplay(t *testing.T) {
	at := func(s string) *time.Time {
		v, err := time.Parse("2006-01-02 15:04", s)
		if err != nil {
			t.Fatal(err)
		}
		return &v
	}
	tests := []struct {
		name      string
		channel   string
		periodEnd *time.Time
		expire    *time.Time
		want      bool
	}{
		{"link19900511 案：pe 比开通到期晚 1 天", "apple", at("2026-11-07 15:51"), at("2026-11-06 14:51"), true},
		{"pe 与到期同刻", "apple", at("2026-10-28 12:34"), at("2026-10-28 12:34"), true},
		{"已被刷过：到期远超 pe", "apple", at("2026-11-07 15:51"), at("2026-11-14 02:04"), true},
		{"真续期：pe 晚一个月", "apple", at("2026-12-07 15:51"), at("2026-11-06 14:51"), false},
		{"google 真续期", "google", at("2026-11-15 04:16"), at("2026-10-15 04:17"), false},
		{"google 重放", "google", at("2026-10-15 04:16"), at("2026-10-15 04:17"), true},
		{"漏续期修复：开通到期停在三个月前", "apple", at("2026-10-16 09:12"), at("2026-07-16 09:12"), false},
		{"无 period_end：保持旧行为", "apple", nil, at("2026-11-06 14:51"), false},
		{"无现有到期：保持旧行为", "apple", at("2026-11-07 15:51"), nil, false},
		{"stripe 不判", "stripe", at("2026-11-07 15:51"), at("2026-11-06 14:51"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isPlatformPeriodReplay(
				&models.ProvisionRequest{Channel: tt.channel, PeriodEnd: tt.periodEnd},
				&models.VPNProvision{ExpireAt: tt.expire})
			if got != tt.want {
				t.Fatalf("isPlatformPeriodReplay = %v, want %v", got, tt.want)
			}
		})
	}
}
