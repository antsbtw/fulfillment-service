package service

import (
	"testing"
	"time"

	"github.com/wenwu/saas-platform/fulfillment-service/internal/client"
)

// 付费叠加合成总量口径(2026-11-18,设计稿 DESIGN_2026-11-18_PAID_STACKING_MULTI_MONTH_TRAFFIC):
// 新上限 = 基准上限 + n × 基准每月额度 + 新买月数 × 新每月额度。
func TestResolvePaidStacking_MultiMonthTraffic(t *testing.T) {
	const gb = int64(1073741824)
	day := 24 * time.Hour
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	ts := func(t time.Time) string { return t.Format(time.RFC3339) }
	live := func(resetAt, expire time.Time, limit int64) *client.VPNUserInfo {
		return &client.VPNUserInfo{ExpireAt: ts(expire), TrafficResetAt: ts(resetAt), TrafficLimit: limit}
	}

	cases := []struct {
		name      string
		live      *client.VPNUserInfo
		span      expireSpan
		monthly   int64
		wantLimit int64
	}{
		{"1 个月单到期前再买 1 个月 → 100+100(与旧口径一致)",
			live(now.Add(-28*day), now.Add(2*day), 100*gb), expireSpan{Days: 30, Months: 1}, 100 * gb, 200 * gb},
		{"01b77cab:1 个月单到期前再买 3 个月 → 100+3×100",
			live(now.Add(-29*day), now.Add(1*day), 100*gb), expireSpan{Days: 90, Months: 3}, 100 * gb, 400 * gb},
		{"12 个月单第 1 个月再买 1 个月 → 100+11×100+100",
			live(now.Add(-16*day), addCalendarMonths(now.Add(-16*day), 12), 100*gb), expireSpan{Months: 1}, 100 * gb, 1300 * gb},
		{"12 个月单已清零 3 次(reset_at 已推进)再买 1 个月 → 100+8×100+100",
			live(now.Add(-(90+5)*day).Add(90*day), addCalendarMonths(now.Add(-(90+5)*day), 12), 100*gb), expireSpan{Months: 1}, 100 * gb, 1000 * gb},
		{"清零点刚过、QuotaResetService 还没 tick:reset_at 落后一个窗口也按当前窗口算",
			live(now.Add(-31*day), now.Add(-31*day).Add(91*day), 100*gb), expireSpan{Months: 1}, 100 * gb, 300 * gb},
		{"已是叠加态(reset_at=expire)再买 6 个月×200G → 400+6×200",
			live(now.Add(170*day), now.Add(170*day), 400*gb), expireSpan{Months: 6}, 200 * gb, 1600 * gb},
		{"拿不到 traffic_reset_at(老 manager)→ n=0,只加新买部分",
			&client.VPNUserInfo{ExpireAt: ts(now.Add(200 * day)), TrafficLimit: 100 * gb}, expireSpan{Months: 3}, 100 * gb, 400 * gb},
		{"老口径只有天数 days_valid=90 → 按 3 个月",
			live(now.Add(-29*day), now.Add(1*day), 100*gb), expireSpan{Days: 90}, 100 * gb, 400 * gb},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, got, stacked := resolvePaidStacking(nil, tc.live, now, tc.span, tc.monthly)
			if !stacked || got != tc.wantLimit {
				t.Fatalf("limit = %d GB stacked=%v, want %d GB", got/gb, stacked, tc.wantLimit/gb)
			}
		})
	}

	// 6 个自然月(181–184 天)= 6 个窗口:刚开始时还剩 5 个没清零。
	start := time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC)
	if n := remainingUnresetPeriods(&start, addCalendarMonths(start, 6), start.Add(time.Hour)); n != 5 {
		t.Errorf("6 个月单起点 n = %d, want 5", n)
	}
	// 1 个自然月单 n=0。
	if n := remainingUnresetPeriods(&start, addCalendarMonths(start, 1), start.Add(time.Hour)); n != 0 {
		t.Errorf("1 个月单 n = %d, want 0", n)
	}
	// 过期后再买仍是 fresh period:上限 = 每月额度,不乘月数(每 30 天清零)。
	expired := live(now.Add(-40*day), now.Add(-10*day), 100*gb)
	if _, l, s := resolvePaidStacking(nil, expired, now, expireSpan{Months: 6}, 100*gb); s || l != 100*gb {
		t.Errorf("expired: limit=%dGB stacked=%v, want 100GB false", l/gb, s)
	}
}
