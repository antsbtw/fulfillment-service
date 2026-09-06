package client

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestGetRealmConnectURL_403IsTypedInactive 锁定住宅面到期链路（2026-09-06）：otun-manager
// connect-url 对到期/禁用 realm 账号返 403 后，fulfillment 必须以 typed error + 结构化 Code 上抛，
// 而不是 fmt.Errorf 泛化文本（否则 /subscribe 只能靠字符串匹配，/subscribe-all 无法区分到期与故障）。
func TestGetRealmConnectURL_403IsTypedInactive(t *testing.T) {
	cases := []struct {
		name     string
		status   int
		body     string
		wantNil  bool // resp==nil && err==nil
		wantCode string
		wantExp  bool
	}{
		{"403 subscription_expired → typed, IsSubscriptionExpired", 403, `{"error":"subscription_expired"}`, false, "subscription_expired", true},
		{"403 traffic_exhausted → typed, not expired", 403, `{"error":"traffic_exhausted"}`, false, "traffic_exhausted", false},
		{"403 non-json body → typed, empty code", 403, `forbidden`, false, "", false},
		{"404 no_assignment → nil,nil 降级", 404, `{"error":"no_assignment"}`, true, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/v1/internal/realm/connect-url" {
					t.Errorf("unexpected path %s", r.URL.Path)
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			resp, err := NewOTunClient(srv.URL, "s").GetRealmConnectURL(context.Background(), "u1")
			if tc.wantNil {
				if resp != nil || err != nil {
					t.Fatalf("want nil,nil got %v,%v", resp, err)
				}
				return
			}
			var inactive *OTunAccountInactiveError
			if !errors.As(err, &inactive) {
				t.Fatalf("want OTunAccountInactiveError, got %T: %v", err, err)
			}
			if inactive.Code != tc.wantCode || inactive.IsSubscriptionExpired() != tc.wantExp {
				t.Fatalf("code=%q expired=%v, want %q/%v", inactive.Code, inactive.IsSubscriptionExpired(), tc.wantCode, tc.wantExp)
			}
		})
	}
}
