package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/wenwu/saas-platform/fulfillment-service/internal/client"
	"github.com/wenwu/saas-platform/fulfillment-service/internal/models"
)

// 2026-11-18 Windows 停用后无限重连案:/vpn/status/all 每面带 inactive_reason,
// 客户端据此区分「流量用完」和「到期」。只在 otun-manager 明确 403 时输出;5xx 不输出也不判到期。
func TestBuildQuickStatus_InactiveReason(t *testing.T) {
	cases := []struct {
		name       string
		code       int
		body       string
		wantStatus string
		wantReason string
	}{
		{"新版 manager 带 reason:额度用完", 403, `{"error":"user disabled","reason":"traffic_exhausted"}`, models.VPNProvisionStatusExpired, "traffic_exhausted"},
		{"新版 manager 带 reason:人工禁用", 403, `{"error":"user disabled","reason":"user_disabled"}`, models.VPNProvisionStatusExpired, "user_disabled"},
		{"老版 manager 无 reason:error 本身是到期码", 403, `{"error":"subscription_expired"}`, models.VPNProvisionStatusExpired, "subscription_expired"},
		{"老版 manager 无 reason:user disabled 推不出原因", 403, `{"error":"user disabled"}`, models.VPNProvisionStatusExpired, ""},
		{"5xx 不判到期、不带原因", 500, `{"error":"boom"}`, models.VPNProvisionStatusActive, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.code)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			s := newSvc(true, &fakeVPNStore{})
			s.otunClient = client.NewOTunClient(srv.URL, "")
			uuid := "u-1"
			vp := residentialRowNoUUID("user-1")
			vp.OtunUUID = &uuid

			got := s.buildQuickStatus(context.Background(), vp)
			if got.Status != tc.wantStatus || got.InactiveReason != tc.wantReason {
				t.Fatalf("status=%q reason=%q, want %q / %q", got.Status, got.InactiveReason, tc.wantStatus, tc.wantReason)
			}
		})
	}
}
