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
