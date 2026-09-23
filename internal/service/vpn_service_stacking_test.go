package service

import (
	"testing"
	"time"

	"github.com/wenwu/saas-platform/fulfillment-service/internal/client"
	"github.com/wenwu/saas-platform/fulfillment-service/internal/models"
)

// resolvePaidStacking 的裁决矩阵（2026-09-23，euanjam8 案）。
//
// 生产实证：住宅面 stripe→stripe 再购时 otun-manager GetUser 对 realm uuid 恒 404，
// 旧实现取不到基准就退化 now+30d——第二笔 $2.99 只把到期从 10-23 11:54 挪到 10-23 13:22，
// 上限还被覆盖成 100G。本矩阵锁定：基准优先 live、其次投影行；未过期叠加（到期+days、上限累加、
// 已用保留），已过期 fresh period。
func TestResolvePaidStacking(t *testing.T) {
	const gb = int64(1073741824)
	now := time.Date(2026, 9, 23, 13, 22, 35, 0, time.UTC)
	future := now.Add(30*24*time.Hour - 88*time.Minute) // 10-23 11:54:35（首笔到期）
	past := now.Add(-24 * time.Hour)

	prov := func(exp *time.Time, limit int64) *models.VPNProvision {
		return &models.VPNProvision{Channel: "stripe", ServiceTier: "residential", ExpireAt: exp, TrafficLimit: limit}
	}
	live := func(exp string, limit int64) *client.VPNUserInfo {
		return &client.VPNUserInfo{ExpireAt: exp, TrafficLimit: limit}
	}

	cases := []struct {
		name       string
		existing   *models.VPNProvision
		live       *client.VPNUserInfo
		wantExpire time.Time
		wantLimit  int64
		wantStack  bool
	}{
		{"live 未过期 → 在 live 到期上叠加、上限累加",
			prov(&past, 50*gb), live(future.Format(time.RFC3339), 100*gb),
			future.AddDate(0, 0, 30), 200 * gb, true},
		{"live 取不到(404) → 退回投影行叠加",
			prov(&future, 100*gb), nil,
			future.AddDate(0, 0, 30), 200 * gb, true},
		{"live expire_at 不可解析 → 退回投影行",
			prov(&future, 100*gb), live("not-a-time", 999*gb),
			future.AddDate(0, 0, 30), 200 * gb, true},
		{"live 空 expire_at → 退回投影行",
			prov(&future, 100*gb), live("", 999*gb),
			future.AddDate(0, 0, 30), 200 * gb, true},
		{"live 已过期 → fresh period（不看投影行）",
			prov(&future, 100*gb), live(past.Format(time.RFC3339), 100*gb),
			now.AddDate(0, 0, 30), 100 * gb, false},
		{"live 取不到且投影行已过期 → fresh period",
			prov(&past, 100*gb), nil,
			now.AddDate(0, 0, 30), 100 * gb, false},
		{"投影行无到期且 live 取不到 → fresh period",
			prov(nil, 100*gb), nil,
			now.AddDate(0, 0, 30), 100 * gb, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotExpire, gotLimit, gotStack := resolvePaidStacking(tc.existing, tc.live, now, 30, 100*gb)
			if !gotExpire.Equal(tc.wantExpire) {
				t.Errorf("expire = %s, want %s", gotExpire.Format(time.RFC3339), tc.wantExpire.Format(time.RFC3339))
			}
			if gotLimit != tc.wantLimit {
				t.Errorf("limit = %d GB, want %d GB", gotLimit/gb, tc.wantLimit/gb)
			}
			if gotStack != tc.wantStack {
				t.Errorf("stacked = %v, want %v", gotStack, tc.wantStack)
			}
		})
	}

	// days<=0 兜底 30 天（与 calculateExpireAt 一致）
	if e, _, _ := resolvePaidStacking(prov(nil, 0), nil, now, 0, gb); !e.Equal(now.AddDate(0, 0, 30)) {
		t.Errorf("days<=0 应兜底 30 天, got %s", e)
	}
}

// TestResolvePaidStacking_EuanjamCase 用生产真实数字复盘：首笔 11:54:55 到期 10-23 11:54:55 / 100G，
// 第二笔 13:22:35 应得到期 11-22 11:54:55 / 200G（人工修正采用的就是这组值）。
func TestResolvePaidStacking_EuanjamCase(t *testing.T) {
	const gb = int64(1073741824)
	now := time.Date(2026, 9, 23, 13, 22, 35, 0, time.UTC)
	first := time.Date(2026, 10, 23, 11, 54, 55, 0, time.UTC)
	existing := &models.VPNProvision{Channel: "stripe", ServiceTier: "residential", ExpireAt: &first, TrafficLimit: 100 * gb}
	e, l, s := resolvePaidStacking(existing, nil, now, 30, 100*gb)
	want := time.Date(2026, 11, 22, 11, 54, 55, 0, time.UTC)
	if !s || !e.Equal(want) || l != 200*gb {
		t.Fatalf("got stacked=%v expire=%s limit=%dGB, want true / %s / 200GB", s, e.Format(time.RFC3339), l/gb, want.Format(time.RFC3339))
	}
}
