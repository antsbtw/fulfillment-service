package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/wenwu/saas-platform/fulfillment-service/internal/client"
	"github.com/wenwu/saas-platform/fulfillment-service/internal/models"
)

func TestLiveTraffic_ReadsHostingNotLocalRow(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/admin/nodes/obox-0-abc/traffic" || r.Header.Get("X-Admin-Key") != "k" {
			w.WriteHeader(404)
			return
		}
		w.Write([]byte(`{"node_id":"obox-0-abc","traffic_limit":1100000000000,"traffic_used":246300000000,"status":"active"}`))
	}))
	defer srv.Close()

	s := &ProvisionService{hostingClient: client.NewHostingClient(srv.URL, "k")}
	hp := &models.HostingProvision{HostingNodeID: "obox-0-abc", TrafficUsed: 0, TrafficLimit: 1 << 40}

	used, limit := s.liveTraffic(context.Background(), hp)
	if used != 246300000000 || limit != 1100000000000 {
		t.Fatalf("got used=%d limit=%d", used, limit)
	}
}

func TestLiveTraffic_FallsBackWhenHostingDown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) }))
	defer srv.Close()

	s := &ProvisionService{hostingClient: client.NewHostingClient(srv.URL, "k")}
	hp := &models.HostingProvision{HostingNodeID: "n", TrafficUsed: 7, TrafficLimit: 9}
	if used, limit := s.liveTraffic(context.Background(), hp); used != 7 || limit != 9 {
		t.Fatalf("fallback got used=%d limit=%d", used, limit)
	}
}

func TestLiveTraffic_NoNodeIDSkipsCall(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true }))
	defer srv.Close()

	s := &ProvisionService{hostingClient: client.NewHostingClient(srv.URL, "k")}
	s.liveTraffic(context.Background(), &models.HostingProvision{})
	if called {
		t.Fatal("must not call hosting without node id")
	}
}
