package service

import (
	"context"
	"encoding/json"
	"flag"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wenwu/saas-platform/fulfillment-service/internal/client"
	"github.com/wenwu/saas-platform/fulfillment-service/internal/config"
	"github.com/wenwu/saas-platform/fulfillment-service/internal/models"
)

// updateGolden 重新抓取 golden fixture：go test ./internal/service -run TestVPNAll_Golden -update
// ★fixture 是在 entitlement profiles 改动【之前】的代码上抓的（契约 H9 / 实施 prompt Step 8.8）：
// 开关 ENTITLEMENT_PROFILES_ENABLED=false 时 /vpn/all 的响应必须与它逐字节一致。
var updateGolden = flag.Bool("update", false, "rewrite golden fixtures")

// newFakeOtunManager 起一个假 otun-manager，只回本测试需要的两个读口（标准面 /api/subscribe、
// 住宅面 realm connect-url），响应内容固定，保证序列化结果可复现。
func newFakeOtunManager(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/subscribe", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"uuid":"otun-std-uuid","traffic_limit":53687091200,"traffic_used":1234,
			"expire_at":"2026-12-01T00:00:00Z","enabled":true,
			"protocols":[{"protocol":"vless","url":"vless://std-primary","node":"primary"},
			             {"protocol":"shadowsocks","url":"ss://std-backup","node":"backup"}],
			"exit_country":"US","smart_strategy":{"mode":"rules","version":1}
		}`))
	})
	mux.HandleFunc("/api/v1/internal/realm/connect-url", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"ok":true,"egress_id":"eg-1","connect_url":"hysteria2-realm://res-hy2",
			"connect_urls":["hysteria2-realm://res-hy2","reality-realm://res-reality"],
			"exit_country":"DE","smart_strategy":{"mode":"rules","version":2},
			"nodes":[{"egress_id":"eg-1","role":"primary","region":"de","connect_url":"hysteria2-realm://res-hy2","connect_urls":["hysteria2-realm://res-hy2","reality-realm://res-reality"]},
			         {"egress_id":"eg-2","role":"backup","region":"de","connect_url":"hysteria2-realm://res-hy2-b","connect_urls":["hysteria2-realm://res-hy2-b"]}],
			"traffic_used":4321,"traffic_limit":107374182400,
			"regions":[{"country":"DE","state":"active","is_current":true,
			            "nodes":[{"egress_id":"eg-1","role":"primary","region":"de","connect_url":"hysteria2-realm://res-hy2","connect_urls":["hysteria2-realm://res-hy2"]}],
			            "smart_strategy":{"mode":"rules","version":2}}]
		}`))
	})
	return httptest.NewServer(mux)
}

// goldenStore 两面各一条 current provision（标准 basic + 住宅 residential），expire 固定。
func goldenStore(userID string) *fakeVPNStore {
	exp := mustTime("2026-12-01T00:00:00Z")
	return &fakeVPNStore{rows: []*models.VPNProvision{
		{
			ID: "prov-std", UserID: userID, SubscriptionID: "sub-std", Channel: "stripe",
			BusinessType: models.BusinessTypeSubscription, ServiceTier: models.ServiceTierStandard,
			OtunUUID: ptrStr("otun-std-uuid"), PlanTier: "basic", Status: models.VPNProvisionStatusActive,
			TrafficLimit: 53687091200, TrafficUsed: 0, ExpireAt: &exp, IsCurrent: true,
		},
		{
			ID: "prov-res", UserID: userID, SubscriptionID: "sub-res", Channel: "apple",
			BusinessType: models.BusinessTypeSubscription, ServiceTier: models.ServiceTierResidential,
			OtunUUID: ptrStr("otun-res-uuid"), PlanTier: "residential", Status: models.VPNProvisionStatusActive,
			TrafficLimit: 107374182400, TrafficUsed: 0, ExpireAt: &exp, IsCurrent: true,
		},
	}}
}

func newGoldenSvc(t *testing.T, store vpnProvisionStore) (*VPNService, func()) {
	t.Helper()
	srv := newFakeOtunManager(t)
	cfg := &config.Config{
		MultiService: config.MultiServiceConfig{Enabled: true},
		Services:     config.ServicesConfig{PublicBaseURL: "https://portal.example.test"},
	}
	s := &VPNService{
		cfg:        cfg,
		vpnRepo:    store,
		otunClient: client.NewOTunClient(srv.URL, "test-secret"),
	}
	return s, srv.Close
}

// TestVPNAll_Golden_SwitchOff：开关 false 时 /vpn/all 逐字节 == 改动前抓的 fixture。
func TestVPNAll_Golden_SwitchOff(t *testing.T) {
	const userID = "u-golden"
	s, done := newGoldenSvc(t, goldenStore(userID))
	defer done()

	all, err := s.GetUserVPNSubscribeConfigAll(context.Background(), userID)
	if err != nil {
		t.Fatalf("GetUserVPNSubscribeConfigAll: %v", err)
	}
	got, err := json.MarshalIndent(all, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got = append(got, '\n')

	path := filepath.Join("testdata", "golden_vpn_all_switch_off.json")
	if *updateGolden {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		t.Logf("golden updated: %s", path)
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden (run with -update to create): %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("switch-off /vpn/all response drifted from pre-change golden.\n--- want ---\n%s\n--- got ---\n%s", want, got)
	}
}

func mustTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}
