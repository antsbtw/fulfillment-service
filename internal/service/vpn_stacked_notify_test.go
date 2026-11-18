package service

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/wenwu/saas-platform/fulfillment-service/internal/client"
)

// B2(2026-11-18):叠加后回调带 effective_period_end + merged_from,与 subscription-service 的 FulfillmentCallback 字段名一致。
func TestNotifyVPNStacked_Payload(t *testing.T) {
	var got map[string]interface{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/internal/fulfillment/callback" {
			t.Errorf("path = %s", r.URL.Path)
		}
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &got)
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"success":true}`))
	}))
	defer srv.Close()

	s := &VPNService{subscriptionClient: client.NewSubscriptionClient(srv.URL, "k")}
	end := time.Date(2026, 12, 22, 17, 9, 33, 0, time.FixedZone("x", 8*3600))
	s.notifyVPNStacked(context.Background(), "new-sub", "prov-1", end, "base-sub")

	if got["subscription_id"] != "new-sub" || got["status"] != "active" || got["app"] != "otun" {
		t.Fatalf("基本字段不对: %v", got)
	}
	if got["effective_period_end"] != "2026-12-22T09:09:33Z" {
		t.Fatalf("effective_period_end = %v,应为 UTC", got["effective_period_end"])
	}
	if got["merged_from"] != "base-sub" {
		t.Fatalf("merged_from = %v", got["merged_from"])
	}

	got = nil
	s.notifyVPNStacked(context.Background(), "same", "prov-1", end, "same")
	if _, ok := got["merged_from"]; ok {
		t.Fatalf("基准就是本单时不应带 merged_from: %v", got)
	}
}
