package service

import (
	"math"
	"time"

	"github.com/wenwu/saas-platform/fulfillment-service/internal/models"
)

// expireSpan 是一次开通给的时长(设计 subscription-service/docs/DESIGN_2026-11-11_CALENDAR_MONTH_PERIOD.md)。
//
//   - Months > 0:按自然月(对齐 Apple / Google / Stripe),到期 = 基准 + Months 个自然月,
//     目标月没有这一天时落到该月最后一天;再加 BonusDays(优惠码赠送天数)。
//     只有 subscription-service 转来的按月一次性购买(Stripe 一次性 / 积分直购 / 礼品卡)会带。
//   - Months == 0:沿用老口径,基准 + Days 天。
type expireSpan struct {
	Days      int
	Months    int
	BonusDays int
}

// expireSpanOf 把开通请求转成时长。Days 与原 calculateExpireDays 同源;
// Apple / Google 由平台定周期,不认 Months(上游也不会带)。
func (s *VPNService) expireSpanOf(req *models.ProvisionRequest) expireSpan {
	span := expireSpan{Days: s.calculateExpireDays(req.Channel, req.ExpireDays)}
	switch req.Channel {
	case "apple", "google":
		return span
	}
	if req.ExpireMonths > 0 {
		span.Months = req.ExpireMonths
		if req.ExpireBonusDays > 0 {
			span.BonusDays = req.ExpireBonusDays
		}
	}
	return span
}

// endFrom 返回从 base 起算的到期时间。
func (p expireSpan) endFrom(base time.Time) time.Time {
	if p.Months > 0 {
		return addCalendarMonths(base, p.Months).AddDate(0, 0, p.BonusDays)
	}
	days := p.Days
	if days <= 0 {
		days = 30
	}
	return base.AddDate(0, 0, days)
}

// approxDays 给只认天数的地方(记账层条目 Days)用:从 now 起这段时长折合的天数,向上取整。
func (p expireSpan) approxDays(now time.Time) int {
	if p.Months <= 0 {
		if p.Days <= 0 {
			return 30
		}
		return p.Days
	}
	return int(math.Ceil(p.endFrom(now).Sub(now).Hours() / 24))
}

// addCalendarMonths 在 t(按 UTC)上加 n 个自然月,时刻不变;目标月没有 t 的那一天时
// 落到该月最后一天(1-31 + 1 个月 = 2-28/29,而不是 time.AddDate 的 3-03)。
// 一次算到位,不逐月累加:1-31 + 3 个月 = 4-30。与 subscription-service 同名函数一致。
func addCalendarMonths(t time.Time, n int) time.Time {
	t = t.UTC()
	y, m, d := t.Date()
	hh, mm, ss := t.Clock()
	ns := t.Nanosecond()
	first := time.Date(y, m+time.Month(n), 1, 0, 0, 0, 0, time.UTC)
	if last := first.AddDate(0, 1, -1).Day(); d > last {
		d = last
	}
	return time.Date(first.Year(), first.Month(), d, hh, mm, ss, ns, time.UTC)
}
