package http

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/wenwu/saas-platform/fulfillment-service/internal/client"
	"github.com/wenwu/saas-platform/fulfillment-service/internal/service"
)

// TestRespondVPNConfigError 锁定 /subscribe 的错误分型：到期（含住宅面 connect-url 403 typed error）
// → 403 SUBSCRIPTION_EXPIRED；超限/禁用/其他 → 维持 404。
func TestRespondVPNConfigError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cases := []struct {
		name     string
		err      error
		wantCode int
		wantBiz  string
	}{
		{"ErrNoActiveSubscription", service.ErrNoActiveSubscription, 403, "SUBSCRIPTION_EXPIRED"},
		{"realm connect-url 403 subscription_expired（包一层 %w）",
			fmt.Errorf("failed to get realm connect-url from otun-manager: %w",
				&client.OTunAccountInactiveError{StatusCode: 403, Code: "subscription_expired", Body: `{"error":"subscription_expired"}`}),
			403, "SUBSCRIPTION_EXPIRED"},
		{"trial_expired 也算到期",
			&client.OTunAccountInactiveError{StatusCode: 403, Code: "trial_expired", Body: `{"error":"trial_expired"}`},
			403, "SUBSCRIPTION_EXPIRED"},
		{"traffic_exhausted 不是到期 → 404 原语义",
			&client.OTunAccountInactiveError{StatusCode: 403, Code: "traffic_exhausted", Body: `{"error":"traffic_exhausted"}`},
			404, ""},
		{"普通故障 → 404", fmt.Errorf("no active VPN provision"), 404, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			respondVPNConfigError(c, tc.err)
			if w.Code != tc.wantCode {
				t.Fatalf("status=%d want %d body=%s", w.Code, tc.wantCode, w.Body.String())
			}
			var body map[string]interface{}
			_ = json.Unmarshal(w.Body.Bytes(), &body)
			if got, _ := body["code"].(string); got != tc.wantBiz {
				t.Fatalf("code=%q want %q", got, tc.wantBiz)
			}
			if w.Code == http.StatusForbidden && body["success"] != false {
				t.Fatalf("403 must carry success:false")
			}
		})
	}
}
