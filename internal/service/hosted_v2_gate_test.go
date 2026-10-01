package service

// CLIENT_REQUEST_HOSTED_V2_GATE_2026-09-30:托管闸门钉住前,带 owner_key 的建机只对测试白名单开新式机。
// 判据在 hosting-service;这里只验证 fulfillment 按答复决定、且任何异常都退回老式。

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/wenwu/saas-platform/fulfillment-service/internal/client"
	"github.com/wenwu/saas-platform/fulfillment-service/internal/config"
)

func gateService(hostingURL string) *ProvisionService {
	s := &ProvisionService{cfg: &config.Config{}}
	if hostingURL != "" {
		s.hostingClient = client.NewHostingClient(hostingURL, "adm")
	}
	return s
}

func TestHostedV2Gate(t *testing.T) {
	var gotKey, gotUser string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/admin/obox/hosted-v2-allowed" {
			http.NotFound(w, r)
			return
		}
		gotKey, gotUser = r.Header.Get("X-Admin-Key"), r.URL.Query().Get("user_id")
		switch gotUser {
		case "tester":
			w.Write([]byte(`{"allowed":true,"reason":"test_account","agent_release":"v1.14.0"}`))
		case "broken":
			w.WriteHeader(http.StatusInternalServerError)
		case "garbage":
			w.Write([]byte(`not json`))
		default:
			w.Write([]byte(`{"allowed":false,"reason":"gate_not_pinned","agent_release":"v1.14.0"}`))
		}
	}))
	defer srv.Close()
	ctx := context.Background()
	s := gateService(srv.URL)

	if !s.hostedV2Allowed(ctx, "tester") {
		t.Error("whitelisted account should get hosted_v2")
	}
	if gotKey != "adm" || gotUser != "tester" {
		t.Errorf("request: key=%q user=%q", gotKey, gotUser)
	}
	for _, u := range []string{"someone", "broken", "garbage"} {
		if s.hostedV2Allowed(ctx, u) {
			t.Errorf("%s: must fall back to hosted_legacy", u)
		}
	}
	if gateService("http://127.0.0.1:1").hostedV2Allowed(ctx, "tester") {
		t.Error("unreachable hosting must fall back to hosted_legacy")
	}
	if gateService("").hostedV2Allowed(ctx, "tester") {
		t.Error("no hosting client must fall back to hosted_legacy")
	}
}
