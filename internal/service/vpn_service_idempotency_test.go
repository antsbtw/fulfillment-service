package service

import (
	"testing"
	"time"
)

// TestProvisionIdempotency_ExtendsExpiryDecision 锁定 ProvisionVPNUser 幂等短路的核心判据：
// 同 tier 但要延长 expire_at（同档续期 / gift 改一年）必须放行（不 skip），
// 否则新 expire_at 永远下发不到 otun-manager（订阅表已变、节点仍按旧周期到期）。
//
// 测试直接复用 ProvisionVPNUser 短路块里同源的 calculateExpireDays + calculateExpireAt，
// 以及 `prospectiveExpireAt.After(existing.ExpireAt + 1min)` 的延长判断，
// 保证“是否延长”的判定与真正执行的 expireAt 同源。
func TestProvisionIdempotency_ExtendsExpiryDecision(t *testing.T) {
	s := &VPNService{} // calculateExpireDays / calculateExpireAt 是纯方法，不依赖任何依赖

	const tolerance = time.Minute

	// extendsExpiry 复刻短路块的判据
	extendsExpiry := func(channel string, reqDays int, existingExpire *time.Time) bool {
		days := s.calculateExpireDays(channel, reqDays)
		prospective := s.calculateExpireAt(days)
		if existingExpire == nil {
			return true
		}
		return prospective.After(existingExpire.Add(tolerance))
	}

	now := time.Now()

	tests := []struct {
		name           string
		channel        string
		reqDays        int
		existingExpire *time.Time
		wantExtends    bool // true => 应放行更新；false => 应短路 skip
	}{
		{
			name:           "gift 改一年, 现有明天到期 => 延长, 放行",
			channel:        "gift",
			reqDays:        365,
			existingExpire: ptrTime(now.AddDate(0, 0, 1)),
			wantExtends:    true,
		},
		{
			name:           "gift 续期, 现有还有 30 天, 再发一年 => 延长, 放行",
			channel:        "gift",
			reqDays:        365,
			existingExpire: ptrTime(now.AddDate(0, 0, 30)),
			wantExtends:    true,
		},
		{
			name:           "gift 重复回放, 现有已是一年后, 再发一年(同周期) => 不延长, 短路",
			channel:        "gift",
			reqDays:        365,
			existingExpire: ptrTime(now.AddDate(0, 0, 365)),
			wantExtends:    false,
		},
		{
			name:           "现有 provision 无 expire_at => 视为需要下发, 放行",
			channel:        "gift",
			reqDays:        365,
			existingExpire: nil,
			wantExtends:    true,
		},
		{
			name:           "apple 续期固定30天, 现有还剩很久 => 不延长, 短路",
			channel:        "apple",
			reqDays:        0,
			existingExpire: ptrTime(now.AddDate(0, 0, 200)),
			wantExtends:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extendsExpiry(tt.channel, tt.reqDays, tt.existingExpire)
			if got != tt.wantExtends {
				t.Errorf("extendsExpiry(%s, %d days) = %v, want %v",
					tt.channel, tt.reqDays, got, tt.wantExtends)
			}
		})
	}
}

func ptrTime(t time.Time) *time.Time { return &t }
