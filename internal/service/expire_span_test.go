package service

import (
	"testing"
	"time"

	"github.com/wenwu/saas-platform/fulfillment-service/internal/client"
	"github.com/wenwu/saas-platform/fulfillment-service/internal/models"
)

func TestAddCalendarMonths(t *testing.T) {
	u := func(y int, m time.Month, d int) time.Time { return time.Date(y, m, d, 9, 30, 0, 0, time.UTC) }
	cases := []struct {
		from time.Time
		n    int
		want time.Time
	}{
		{u(2026, 11, 11), 1, u(2026, 12, 11)},
		{u(2026, 11, 11), 12, u(2027, 11, 11)},
		{u(2027, 1, 31), 1, u(2027, 2, 28)},
		{u(2028, 1, 31), 1, u(2028, 2, 29)},
		{u(2027, 1, 31), 3, u(2027, 4, 30)},
		{u(2026, 10, 31), 1, u(2026, 11, 30)},
		{u(2026, 12, 31), 2, u(2027, 2, 28)},
	}
	for _, c := range cases {
		if got := addCalendarMonths(c.from, c.n); !got.Equal(c.want) {
			t.Errorf("%v + %d months: got %v, want %v", c.from, c.n, got, c.want)
		}
	}
}

func TestExpireSpanOf(t *testing.T) {
	s := &VPNService{}
	// 按月一次性购买:自然月 + 赠送天数。
	sp := s.expireSpanOf(&models.ProvisionRequest{Channel: "stripe", ExpireDays: 367, ExpireMonths: 12, ExpireBonusDays: 7})
	if sp.Months != 12 || sp.BonusDays != 7 || sp.Days != 367 {
		t.Fatalf("stripe: %+v", sp)
	}
	base := time.Date(2027, 1, 31, 0, 0, 0, 0, time.UTC)
	if got, want := sp.endFrom(base), time.Date(2028, 2, 7, 0, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Errorf("endFrom: got %v, want %v", got, want) // 2028-01-31 + 7 天
	}
	// 老上游(没带月数)按天,行为不变。
	if sp = s.expireSpanOf(&models.ProvisionRequest{Channel: "credit", ExpireDays: 90}); sp.Months != 0 || !sp.endFrom(base).Equal(base.AddDate(0, 0, 90)) {
		t.Errorf("legacy days: %+v", sp)
	}
	// Apple/Google 不认月数(平台定周期)。
	if sp = s.expireSpanOf(&models.ProvisionRequest{Channel: "apple", ExpireMonths: 12}); sp.Months != 0 || sp.Days != 30 {
		t.Errorf("apple: %+v", sp)
	}
	// approxDays:1-31 起 1 个自然月 = 28 天。
	if d := (expireSpan{Months: 1}).approxDays(base); d != 28 {
		t.Errorf("approxDays: got %d, want 28", d)
	}
}

// 未过期续买在原到期上加自然月;过期则从 now 起。
func TestResolvePaidStacking_CalendarMonths(t *testing.T) {
	const gb = int64(1073741824)
	now := time.Date(2027, 1, 20, 0, 0, 0, 0, time.UTC)
	cur := time.Date(2027, 1, 31, 12, 0, 0, 0, time.UTC)
	live := &client.VPNUserInfo{ExpireAt: cur.Format(time.RFC3339), TrafficLimit: 100 * gb}
	e, l, stacked := resolvePaidStacking(nil, live, now, expireSpan{Days: 30, Months: 1}, 100*gb)
	if want := time.Date(2027, 2, 28, 12, 0, 0, 0, time.UTC); !e.Equal(want) || l != 200*gb || !stacked {
		t.Errorf("stack: got %v %d %v, want %v", e, l, stacked, want)
	}
	past := &models.VPNProvision{ExpireAt: &now}
	e, _, stacked = resolvePaidStacking(past, nil, now.Add(time.Hour), expireSpan{Months: 12}, gb)
	if want := addCalendarMonths(now.Add(time.Hour), 12); !e.Equal(want) || stacked {
		t.Errorf("expired: got %v %v, want %v", e, stacked, want)
	}
}

// Apple / Google 正式环境:节点到期 = 平台 period_end(由平台定);其余情况退回 30 天老口径。
func TestExpireSpanOf_PlatformPeriodEnd(t *testing.T) {
	s := &VPNService{}
	now := time.Now()
	appleEnd := now.Add(31*24*time.Hour + time.Hour) // 31 天周期(如 10-28 → 11-28)
	past := now.Add(-time.Hour)

	cases := []struct {
		name    string
		req     models.ProvisionRequest
		usePlat bool
	}{
		{"apple production", models.ProvisionRequest{Channel: "apple", Environment: "Production", PeriodEnd: &appleEnd}, true},
		{"google production", models.ProvisionRequest{Channel: "google", Environment: "Production", PeriodEnd: &appleEnd}, true},
		{"apple sandbox keeps 30d", models.ProvisionRequest{Channel: "apple", Environment: "Sandbox", PeriodEnd: &appleEnd}, false},
		{"no environment (old upstream) keeps 30d", models.ProvisionRequest{Channel: "apple", PeriodEnd: &appleEnd}, false},
		{"no period_end keeps 30d", models.ProvisionRequest{Channel: "apple", Environment: "Production"}, false},
		{"period_end passed keeps 30d", models.ProvisionRequest{Channel: "apple", Environment: "Production", PeriodEnd: &past}, false},
		{"stripe ignores period_end", models.ProvisionRequest{Channel: "stripe", Environment: "production", PeriodEnd: &appleEnd, ExpireDays: 30}, false},
	}
	for _, c := range cases {
		req := c.req
		got := s.expireSpanOf(&req).endFrom(now)
		if c.usePlat {
			if !got.Equal(appleEnd.UTC()) {
				t.Errorf("%s: got %v, want platform end %v", c.name, got, appleEnd)
			}
		} else if want := now.AddDate(0, 0, 30); !got.Equal(want) {
			t.Errorf("%s: got %v, want now+30d %v", c.name, got, want)
		}
	}
}

// 同一期重放:按平台到期开通后,同一 period_end 再来一次不算"延长",走幂等短路。
func TestPlatformPeriodEnd_ReplayDoesNotExtend(t *testing.T) {
	s := &VPNService{}
	end := time.Now().Add(20 * 24 * time.Hour).UTC()
	req := &models.ProvisionRequest{Channel: "apple", Environment: "Production", PeriodEnd: &end}
	existing := end // 第一次开通写下的节点到期
	prospective := s.expireSpanOf(req).endFrom(time.Now())
	if prospective.After(existing.Add(time.Minute)) {
		t.Fatalf("replay should not extend: prospective=%v existing=%v", prospective, existing)
	}
	// 真续费:下一期 period_end 比现有晚约一个周期 → 延长、且不被当作重放。
	next := end.AddDate(0, 1, 0)
	req.PeriodEnd = &next
	if p := s.expireSpanOf(req).endFrom(time.Now()); !p.After(existing.Add(time.Minute)) {
		t.Fatalf("renewal should extend: %v", p)
	}
	if isPlatformPeriodReplay(req, &models.VPNProvision{ExpireAt: &existing}) {
		t.Fatalf("renewal misjudged as replay")
	}
}
