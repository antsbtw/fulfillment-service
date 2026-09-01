package service

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/wenwu/saas-platform/fulfillment-service/internal/client"
	"github.com/wenwu/saas-platform/fulfillment-service/internal/models"
)

// 背景（2026-09-01）：投影行 vpn_provisions 到期后【没有任何写路径】会去改 status，
// 生产实测 507 条 is_current 行 status=active 但 expire_at 已过（Alvin 的 basic 面
// Apple 订阅 08-23 到期，投影行仍 active、expire_at 滞留 07-15）→ /vpn/status/all
// 下发 "active"，端上显示"仍有效"而实际连不上。
//
// 修法是读时判定（与 subscription-service effectiveStatus 同口径），下面钉死其语义。

func TestEffectiveProvisionStatus(t *testing.T) {
	past := time.Now().Add(-24 * time.Hour)
	future := time.Now().Add(24 * time.Hour)

	cases := []struct {
		name   string
		status string
		expire *time.Time
		want   string
	}{
		{
			name:   "active 且已过期 → 读时判为 expired（本次修的核心场景）",
			status: models.VPNProvisionStatusActive, expire: &past,
			want: models.VPNProvisionStatusExpired,
		},
		{
			name:   "active 且未过期 → 保持 active",
			status: models.VPNProvisionStatusActive, expire: &future,
			want: models.VPNProvisionStatusActive,
		},
		{
			name:   "active 但 expire_at 为 NULL → 不臆断到期，保持 active",
			status: models.VPNProvisionStatusActive, expire: nil,
			want: models.VPNProvisionStatusActive,
		},
		{
			// 非 active 的状态（如 converted/suspended）不因时间被改写，
			// 否则会把"已转化的 trial"等状态误报成 expired。
			name:   "非 active 状态即使已过期也原样返回",
			status: "converted", expire: &past,
			want: "converted",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := effectiveProvisionStatus(tc.status, tc.expire); got != tc.want {
				t.Fatalf("effectiveProvisionStatus(%q, %v) = %q, want %q",
					tc.status, tc.expire, got, tc.want)
			}
		})
	}
}

// isOTunAccountInactive 必须只对 otun 明确拒绝（403）为真。
// ★关键边界：网络错误/5xx 必须为 false——否则 otun-manager 一抖动，
// 全体在线用户会被误判成"已过期"。
func TestIsOTunAccountInactive(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "403 subscription_expired → true",
			err:  &client.OTunAccountInactiveError{StatusCode: 403, Body: `{"error":"subscription_expired"}`},
			want: true,
		},
		{
			name: "403 user disabled（配额耗尽/禁用）→ true",
			err:  &client.OTunAccountInactiveError{StatusCode: 403, Body: `{"error":"user disabled"}`},
			want: true,
		},
		{
			name: "被 %w 包装后仍能识别（errors.As 而非字符串匹配）",
			err:  fmt.Errorf("sync failed: %w", &client.OTunAccountInactiveError{StatusCode: 403}),
			want: true,
		},
		{
			name: "普通 5xx 错误 → false（读失败不等于到期）",
			err:  errors.New("otun-manager sync returned status 500: internal error"),
			want: false,
		},
		{
			name: "网络错误 → false",
			err:  errors.New("send request: connection refused"),
			want: false,
		},
		{
			name: "nil → false",
			err:  nil,
			want: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isOTunAccountInactive(tc.err); got != tc.want {
				t.Fatalf("isOTunAccountInactive(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
