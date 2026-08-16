package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wenwu/saas-platform/fulfillment-service/internal/client"
	"github.com/wenwu/saas-platform/fulfillment-service/internal/config"
	"github.com/wenwu/saas-platform/fulfillment-service/internal/models"
)

// fakeVPNStore 追加投影更新（projectionStore 接口）。
func (f *fakeVPNStore) UpdateProjection(_ context.Context, id string, expireAt *time.Time, trafficLimit int64, channel, _ string) error {
	for _, r := range f.rows {
		if r.ID == id {
			r.ExpireAt = expireAt
			r.TrafficLimit = trafficLimit
			r.Channel = channel
		}
	}
	return nil
}

// otunStub 是带账号状态的假 otun-manager（standard 面）：POST/PUT/GET /api/users、POST /api/subscribe。
type otunStub struct {
	mu       sync.Mutex
	accounts map[string]map[string]interface{} // uuid → {traffic_limit, traffic_used, expire_at, enabled}
	puts     []map[string]interface{}
}

func newOtunStub(t *testing.T) (*otunStub, *httptest.Server) {
	t.Helper()
	st := &otunStub{accounts: map[string]map[string]interface{}{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/users", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&body)
		uuid, _ := body["uuid"].(string)
		st.mu.Lock()
		st.accounts[uuid] = map[string]interface{}{
			"uuid": uuid, "traffic_limit": body["traffic_limit"], "traffic_used": float64(0),
			"expire_at": body["expire_at"], "enabled": true,
		}
		st.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"uuid": uuid, "enabled": true})
	})
	mux.HandleFunc("/api/users/", func(w http.ResponseWriter, r *http.Request) {
		uuid := strings.TrimPrefix(r.URL.Path, "/api/users/")
		uuid = strings.TrimSuffix(uuid, "/sync")
		st.mu.Lock()
		defer st.mu.Unlock()
		acc, ok := st.accounts[uuid]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "not found"})
			return
		}
		switch r.Method {
		case http.MethodPut:
			var body map[string]interface{}
			_ = json.NewDecoder(r.Body).Decode(&body)
			for k, v := range body {
				acc[k] = v
			}
			st.puts = append(st.puts, body)
			_ = json.NewEncoder(w).Encode(acc)
		default:
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"uuid": uuid, "traffic_limit": acc["traffic_limit"], "traffic_used": acc["traffic_used"],
				"expire_at": acc["expire_at"], "enabled": acc["enabled"], "protocols": []interface{}{},
			})
		}
	})
	mux.HandleFunc("/api/subscribe", func(w http.ResponseWriter, r *http.Request) {
		st.mu.Lock()
		defer st.mu.Unlock()
		// 单账号测试：取第一个账号
		for _, acc := range st.accounts {
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"uuid": acc["uuid"], "traffic_limit": acc["traffic_limit"], "traffic_used": acc["traffic_used"],
				"expire_at": acc["expire_at"], "enabled": true,
				"protocols": []map[string]string{{"protocol": "vless", "url": "vless://x", "node": "primary"}},
			})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
	srv := httptest.NewServer(mux)
	return st, srv
}

func (st *otunStub) setUsed(uuid string, used int64) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if acc, ok := st.accounts[uuid]; ok {
		acc["traffic_used"] = float64(used)
	}
}

func (st *otunStub) firstUUID() string {
	st.mu.Lock()
	defer st.mu.Unlock()
	for u := range st.accounts {
		return u
	}
	return ""
}

// newSwitchOnStack 拼一套开关 true 的 VPNService：真 OTunClient 打 stub、内存 provision 表、内存记账表。
func newSwitchOnStack(t *testing.T) (*VPNService, *otunStub, *fakeEntitlementStore, *clock, func()) {
	t.Helper()
	stub, srv := newOtunStub(t)
	oc := client.NewOTunClient(srv.URL, "test-secret")
	store := &fakeVPNStore{}
	entStore := &fakeEntitlementStore{}
	clk := &clock{t: time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)}
	ent := &EntitlementProfileService{
		store: entStore, provisions: store, otun: &otunAccountAdapter{c: oc},
		enabled: true, switchLead: 65 * time.Minute, now: clk.now,
	}
	cfg := &config.Config{
		MultiService: config.MultiServiceConfig{Enabled: true},
		Entitlement:  config.EntitlementConfig{Enabled: true, SwitchLead: 65 * time.Minute},
		Services:     config.ServicesConfig{PublicBaseURL: "https://portal.example.test"},
	}
	svc := &VPNService{cfg: cfg, vpnRepo: store, otunClient: oc, entitlement: ent}
	return svc, stub, entStore, clk, srv.Close
}

// TestSwitchOn_ProvisionThenVPNAll_ContractH1H4H9：开关 true 端到端——
// 开通订阅 + 追加 credit → /vpn/all：每面一元素(H1)、status 必有(H4)、既有 16 字段全在(H9)、
// 新增 active_class/profiles[] 出现且枚举合法、subscription active + purchase waiting、
// 既有 expire_at = 生效 profile 值(H2)。
func TestSwitchOn_ProvisionThenVPNAll_ContractH1H4H9(t *testing.T) {
	svc, stub, entStore, clk, done := newSwitchOnStack(t)
	defer done()
	ctx := context.Background()
	const user = "u-on"

	periodEnd := clk.now().Add(30 * 24 * time.Hour)
	periodStart := clk.now()
	resp, err := svc.ProvisionVPNUser(ctx, &models.ProvisionRequest{
		AppSource: "otun", BusinessType: "subscription", Channel: "apple", SubscriptionID: "sub-apple",
		UserID: user, UserEmail: "u@example.test", PlanTier: "basic", PurchaseType: "subscription",
		ChannelSubID: "apple-orig-1", PeriodStart: &periodStart, PeriodEnd: &periodEnd, ExpireDays: 30,
	})
	if err != nil {
		t.Fatalf("provision subscription: %v", err)
	}
	if resp.VPNUserID == "" || stub.firstUUID() != resp.VPNUserID {
		t.Fatalf("otun account must be created and uuid returned, got %+v", resp)
	}
	// 重放同一事件 → 幂等（不新增 entry）
	before := len(entStore.entries)
	if _, err := svc.ProvisionVPNUser(ctx, &models.ProvisionRequest{
		AppSource: "otun", BusinessType: "subscription", Channel: "apple", SubscriptionID: "sub-apple",
		UserID: user, PlanTier: "basic", PurchaseType: "subscription", ChannelSubID: "apple-orig-1",
		PeriodStart: &periodStart, PeriodEnd: &periodEnd, ExpireDays: 30,
	}); err != nil {
		t.Fatalf("replay: %v", err)
	}
	if len(entStore.entries) != before {
		t.Fatalf("replayed provision must not add entries: %d → %d", before, len(entStore.entries))
	}
	// 追加 credit 一次性 30 天 → 桶 waiting，uuid 不变
	resp2, err := svc.ProvisionVPNUser(ctx, &models.ProvisionRequest{
		AppSource: "otun", BusinessType: "purchase", Channel: "credit", SubscriptionID: "sub-credit-1",
		UserID: user, PlanTier: "basic", PurchaseType: "one_time", ChannelSubID: "credit-order-1", ExpireDays: 30,
		TrafficLimit: 100 * GB,
	})
	if err != nil {
		t.Fatalf("provision credit: %v", err)
	}
	if resp2.VPNUserID != resp.VPNUserID {
		t.Fatalf("form B: uuid must not change across profiles: %s vs %s", resp2.VPNUserID, resp.VPNUserID)
	}
	stub.setUsed(resp.VPNUserID, 5*GB)

	all, err := svc.GetUserVPNSubscribeConfigAll(ctx, user)
	if err != nil {
		t.Fatalf("vpn/all: %v", err)
	}
	if len(all) != 1 || all[0].ServiceTier != models.ServiceTierStandard { // H1：一面一元素
		t.Fatalf("H1: want exactly one element for standard face, got %d", len(all))
	}
	raw, _ := json.Marshal(all[0])
	var m map[string]json.RawMessage
	_ = json.Unmarshal(raw, &m)
	// H4 + H9：既有字段（改动前 16 个中本响应必出的）都在，且 status 必有
	for _, k := range []string{"status", "channel", "plan_tier", "service_tier", "subscribe_url", "device_id",
		"protocols", "traffic_limit", "traffic_used", "expire_at", "message", "config_version"} {
		if _, ok := m[k]; !ok {
			t.Fatalf("H9/H4: existing field %q missing in switch-on response: %s", k, raw)
		}
	}
	// 新增字段
	e := all[0]
	if e.ActiveClass != models.EntitlementClassSubscription {
		t.Fatalf("active_class want subscription, got %q", e.ActiveClass)
	}
	if len(e.Profiles) != 2 {
		t.Fatalf("want profiles[subscription, purchase], got %+v", e.Profiles)
	}
	sub, pur := e.Profiles[0], e.Profiles[1]
	if sub.Class != "subscription" || sub.Status != "active" || sub.ExpireAt == nil || *sub.ExpireAt != periodEnd.UTC().Format(time.RFC3339) ||
		fmt.Sprint(sub.Kinds) != "[apple]" || sub.TrafficUsed != 5*GB {
		t.Fatalf("subscription profile view wrong: %+v", sub)
	}
	if pur.Class != "purchase" || pur.Status != "waiting" || pur.ExpireAt != nil || pur.DaysRemaining == nil || *pur.DaysRemaining != 30 ||
		pur.EffectiveFrom == nil || *pur.EffectiveFrom != *sub.ExpireAt || fmt.Sprint(pur.Kinds) != "[credit]" ||
		pur.TrafficRemaining == nil || *pur.TrafficRemaining != 100*GB {
		t.Fatalf("purchase profile view wrong: %+v", pur)
	}
	// H2：既有字段 = 生效 profile（subscription）的值
	if e.ExpireAt != *sub.ExpireAt || e.TrafficLimit != sub.TrafficLimit || e.TrafficUsed != 5*GB || e.Status != "active" || e.Channel != "apple" {
		t.Fatalf("H2: top-level fields must equal active profile: expire=%s limit=%d used=%d status=%s channel=%s",
			e.ExpireAt, e.TrafficLimit, e.TrafficUsed, e.Status, e.Channel)
	}
	// 枚举合法性（契约 §4 冻结）
	for _, p := range e.Profiles {
		if !map[string]bool{"subscription": true, "purchase": true, "trial": true}[p.Class] {
			t.Fatalf("illegal class %q", p.Class)
		}
		if !map[string]bool{"active": true, "waiting": true, "expired": true, "none": true}[p.Status] {
			t.Fatalf("illegal profile status %q", p.Status)
		}
	}

	// 时间推进到订阅过期后 → 读时兜底 Resolve：桶接续，otun 收到 PUT（expire 推后 + enabled=true）
	clk.t = periodEnd.Add(2 * time.Minute)
	all, err = svc.GetUserVPNSubscribeConfigAll(ctx, user)
	if err != nil || len(all) != 1 {
		t.Fatalf("vpn/all after expiry: %v / %d", err, len(all))
	}
	e = all[0]
	if e.ActiveClass != models.EntitlementClassPurchase || e.Status != "active" {
		t.Fatalf("after subscription expiry purchase must be active: class=%s status=%s", e.ActiveClass, e.Status)
	}
	wantExp := clk.now().Add(30 * 24 * time.Hour).UTC().Format(time.RFC3339)
	if e.ExpireAt != wantExp {
		t.Fatalf("H2/H3: top-level expire_at must be bucket expire %s, got %s", wantExp, e.ExpireAt)
	}
	stub.mu.Lock()
	lastPut := stub.puts[len(stub.puts)-1]
	stub.mu.Unlock()
	if lastPut["expire_at"] != wantExp || lastPut["enabled"] != true {
		t.Fatalf("otun PUT must carry new expire + enabled=true, got %+v", lastPut)
	}
	// traffic_limit = baseline(5GB) + 桶剩余(100GB)
	if int64(lastPut["traffic_limit"].(float64)) != 105*GB {
		t.Fatalf("otun traffic_limit want baseline+bucket=105GB, got %v", lastPut["traffic_limit"])
	}

	// /status 单条（H3）= 生效 profile
	qs, err := svc.GetUserVPNQuickStatus(ctx, user)
	if err != nil {
		t.Fatalf("quick status: %v", err)
	}
	if qs.ExpireAt != wantExp || qs.ActiveClass != models.EntitlementClassPurchase || qs.TrafficLimit != 100*GB || qs.ServiceTier != models.ServiceTierStandard {
		t.Fatalf("H3: /status must reflect active purchase profile, got %+v", qs)
	}
}

// TestSwitchOn_SubscriptionResponse_HasStatusAlways：none 分支仍返回元素、status="expired"、expire_at 非空。
func TestSwitchOn_NoneBranch_StatusExpiredExpireNotNull(t *testing.T) {
	svc, _, _, clk, done := newSwitchOnStack(t)
	defer done()
	ctx := context.Background()
	const user = "u-none"
	periodEnd := clk.now().Add(24 * time.Hour)
	if _, err := svc.ProvisionVPNUser(ctx, &models.ProvisionRequest{
		AppSource: "otun", Channel: "stripe", SubscriptionID: "sub-1", UserID: user, PlanTier: "basic",
		PurchaseType: "subscription", ChannelSubID: "st-1", PeriodEnd: &periodEnd, ExpireDays: 1,
	}); err != nil {
		t.Fatalf("provision: %v", err)
	}
	clk.t = periodEnd.Add(time.Hour)
	all, err := svc.GetUserVPNSubscribeConfigAll(ctx, user)
	if err != nil || len(all) != 1 {
		t.Fatalf("vpn/all: %v / %d", err, len(all))
	}
	e := all[0]
	if e.ActiveClass != models.ActiveClassNone || e.Status != "expired" || e.ExpireAt != periodEnd.UTC().Format(time.RFC3339) {
		t.Fatalf("none branch: want status=expired expire_at=%s active_class=none, got status=%s expire=%s class=%s",
			periodEnd.UTC().Format(time.RFC3339), e.Status, e.ExpireAt, e.ActiveClass)
	}
	if len(e.Profiles) != 1 || e.Profiles[0].Status != "expired" {
		t.Fatalf("subscription profile want expired, got %+v", e.Profiles)
	}
}
