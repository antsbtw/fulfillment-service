package service

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wenwu/saas-platform/fulfillment-service/internal/client"
	"github.com/wenwu/saas-platform/fulfillment-service/internal/config"
	"github.com/wenwu/saas-platform/fulfillment-service/internal/models"
	"github.com/wenwu/saas-platform/fulfillment-service/internal/repository"
)

// ============================================================================
// 第三产品面 campaign 测试（document/marketing-campaign/IMPL_PROMPT Step 2）：
//   1. golden 无头零差异：持有活动账号的用户，不带能力头 → /vpn/all 与改动前 fixture 逐字节一致（C1）；
//   2. 有头追加元素结构（契约 §3）：plan_tier/profile_class/service_tier/status/campaign{}；
//   3. /vpn/version 无头不因活动账号变化（C6）；
//   4. 分区隔离：basic+campaign 并存互不命中；不分区读口排除 campaign；
//   5. 叠加算术：首领建号 → 二领叠加(expire+days, traffic+=) → 同 subscription 重放幂等 → 到期后再领重置周期 → 撤销扣减下限。
// ============================================================================

// fakeGrantStore 内存 campaign_grants。
type fakeGrantStore struct {
	mu     sync.Mutex
	grants map[string]*models.CampaignGrant
}

func newFakeGrantStore() *fakeGrantStore {
	return &fakeGrantStore{grants: map[string]*models.CampaignGrant{}}
}

func (f *fakeGrantStore) InsertIfAbsent(_ context.Context, g *models.CampaignGrant) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.grants[g.SubscriptionID]; ok {
		return false, nil
	}
	cp := *g
	cp.Status = "active"
	cp.AppliedAt = time.Now()
	f.grants[g.SubscriptionID] = &cp
	return true, nil
}
func (f *fakeGrantStore) GetBySubscriptionID(_ context.Context, id string) (*models.CampaignGrant, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if g, ok := f.grants[id]; ok {
		return g, nil
	}
	return nil, repository.ErrNotFound
}
func (f *fakeGrantStore) MarkRevoked(_ context.Context, id string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if g, ok := f.grants[id]; ok && g.Status == "active" {
		g.Status = "revoked"
		return true, nil
	}
	return false, nil
}
func (f *fakeGrantStore) ExpireActiveByUser(_ context.Context, userID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, g := range f.grants {
		if g.UserID == userID && g.Status == "active" {
			g.Status = "expired"
		}
	}
	return nil
}
func (f *fakeGrantStore) AggregateActiveByUser(_ context.Context, userID string) (*models.CampaignGrantAggregate, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	agg := &models.CampaignGrantAggregate{}
	for _, g := range f.grants {
		if g.UserID == userID && g.Status == "active" {
			agg.ClaimsActive++
			agg.GrantedDaysTotal += g.Days
			agg.GrantedTrafficTotal += g.TrafficBytes
			t := g.AppliedAt
			if agg.LastClaimAt == nil || t.After(*agg.LastClaimAt) {
				agg.LastClaimAt = &t
			}
		}
	}
	return agg, nil
}

// fakeOtun 记录 otun-manager 调用（POST /api/users、PUT /api/users/:uuid、GET /api/users/:uuid/sync、
// DELETE /api/users/:uuid）+ 复用 golden 的 /api/subscribe、realm connect-url 固定响应。
type fakeOtun struct {
	mu      sync.Mutex
	posts   []client.CreateVPNUserRequest
	puts    []client.UpdateVPNUserRequest
	putUUID []string
	putFail int // >0 时 PUT 返回 500（次数递减）
	syncFn  func(uuid string) (int, string)
	srv     *httptest.Server
}

func newFakeOtun(t *testing.T) *fakeOtun {
	t.Helper()
	f := &fakeOtun{}
	golden := newFakeOtunManager(t) // 复用 golden 的两个读口 handler（同响应）
	t.Cleanup(golden.Close)
	mux := http.NewServeMux()
	relay := func(path string) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			resp, err := http.Get(golden.URL + path)
			if err != nil {
				w.WriteHeader(502)
				return
			}
			defer resp.Body.Close()
			b, _ := io.ReadAll(resp.Body)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(b)
		}
	}
	mux.HandleFunc("/api/subscribe", relay("/api/subscribe"))
	mux.HandleFunc("/api/v1/internal/realm/connect-url", relay("/api/v1/internal/realm/connect-url"))
	mux.HandleFunc("/api/users", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(405)
			return
		}
		var req client.CreateVPNUserRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.mu.Lock()
		f.posts = append(f.posts, req)
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(201)
		_, _ = w.Write([]byte(`{"uuid":"` + req.UUID + `","traffic_limit":` + itoa(req.TrafficLimit) + `,"expire_at":"` + req.ExpireAt + `","enabled":true}`))
	})
	mux.HandleFunc("/api/users/", func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.Path, "/api/users/")
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPut:
			f.mu.Lock()
			if f.putFail > 0 {
				f.putFail--
				f.mu.Unlock()
				w.WriteHeader(500)
				_, _ = w.Write([]byte(`{"error":"otun down"}`))
				return
			}
			f.mu.Unlock()
			var req client.UpdateVPNUserRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			f.mu.Lock()
			f.puts = append(f.puts, req)
			f.putUUID = append(f.putUUID, rest)
			f.mu.Unlock()
			_, _ = w.Write([]byte(`{"ok":true}`))
		case r.Method == http.MethodGet && strings.HasSuffix(rest, "/sync"):
			uuid := strings.TrimSuffix(rest, "/sync")
			if f.syncFn != nil {
				code, body := f.syncFn(uuid)
				w.WriteHeader(code)
				_, _ = w.Write([]byte(body))
				return
			}
			_, _ = w.Write([]byte(`{"uuid":"` + uuid + `","traffic_limit":10737418240,"traffic_used":42,"expire_at":"2026-08-23T10:00:00Z","enabled":true,
				"protocols":[{"protocol":"vless","url":"vless://` + uuid + `@camp-primary","node":"primary"},{"protocol":"shadowsocks","url":"ss://` + uuid + `@camp-backup","node":"backup"}],
				"exit_country":"JP","smart_strategy":{"mode":"rules","version":3}}`))
		case r.Method == http.MethodDelete:
			_, _ = w.Write([]byte(`{"ok":true}`))
		default:
			w.WriteHeader(404)
		}
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func itoa(n int64) string { b, _ := json.Marshal(n); return string(b) }

func newCampaignSvc(t *testing.T, store *fakeVPNStore, grants *fakeGrantStore) (*VPNService, *fakeOtun) {
	t.Helper()
	otun := newFakeOtun(t)
	cfg := &config.Config{
		MultiService: config.MultiServiceConfig{Enabled: true},
		Services:     config.ServicesConfig{PublicBaseURL: "https://portal.example.test"},
		Campaign:     config.CampaignConfig{StackHardMaxDays: 56, StackHardMaxTrafficGB: 200, RetentionDays: 7},
	}
	s := &VPNService{cfg: cfg, vpnRepo: store, otunClient: client.NewOTunClient(otun.srv.URL, "test-secret")}
	if grants != nil {
		s.campaignGrants = grants
	}
	return s, otun
}

func campaignRow(userID, uuid string, exp time.Time) *models.VPNProvision {
	return &models.VPNProvision{
		ID: "prov-camp", UserID: userID, SubscriptionID: "sub-camp-1", Channel: "campaign",
		BusinessType: models.BusinessTypeGift, ServiceTier: models.ServiceTierStandard, ProductFace: models.ProductFaceCampaign,
		OtunUUID: ptrStr(uuid), PlanTier: models.PlanTierCampaign, Status: models.VPNProvisionStatusActive,
		TrafficLimit: 10 * 1024 * 1024 * 1024, ExpireAt: &exp, IsCurrent: true,
	}
}

// 1. golden 无头零差异（有活动账号）：与 TestVPNAll_Golden_SwitchOff 同 fixture。
func TestVPNAll_Golden_NoCaps_WithCampaignRow(t *testing.T) {
	const userID = "u-golden"
	store := goldenStore(userID)
	store.rows = append(store.rows, campaignRow(userID, "otun-camp-uuid", mustTime("2026-08-23T10:00:00Z")))
	s, _ := newCampaignSvc(t, store, newFakeGrantStore())

	for _, caps := range []ClientCapabilities{nil, ParseClientCapabilities(""), ParseClientCapabilities("foo,bar")} {
		all, err := s.GetUserVPNSubscribeConfigAllWithCaps(context.Background(), userID, caps)
		if err != nil {
			t.Fatalf("GetUserVPNSubscribeConfigAllWithCaps: %v", err)
		}
		got, _ := json.MarshalIndent(all, "", "  ")
		got = append(got, '\n')
		want, err := os.ReadFile(filepath.Join("testdata", "golden_vpn_all_switch_off.json"))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != string(want) {
			t.Fatalf("no-caps /vpn/all with campaign row drifted from golden (caps=%v).\n--- want ---\n%s\n--- got ---\n%s", caps, want, got)
		}
	}
	// 老签名同样零差异
	all, _ := s.GetUserVPNSubscribeConfigAll(context.Background(), userID)
	if len(all) != 2 {
		t.Fatalf("legacy signature must return 2 faces, got %d", len(all))
	}
}

// 2. 有头追加元素结构。
func TestVPNAll_WithCaps_AppendsCampaignElement(t *testing.T) {
	const userID = "u-golden"
	store := goldenStore(userID)
	store.rows = append(store.rows, campaignRow(userID, "otun-camp-uuid", mustTime("2099-08-23T10:00:00Z")))
	grants := newFakeGrantStore()
	_, _ = grants.InsertIfAbsent(context.Background(), &models.CampaignGrant{SubscriptionID: "sub-camp-1", UserID: userID, Days: 7, TrafficBytes: 10 * 1024 * 1024 * 1024})
	s, _ := newCampaignSvc(t, store, grants)

	all, err := s.GetUserVPNSubscribeConfigAllWithCaps(context.Background(), userID, ParseClientCapabilities("Campaign-Profile, foo"))
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Fatalf("want basic+residential+campaign, got %d", len(all))
	}
	// 前两个与 golden 一致（campaign 元素追加在后，不改既有元素）
	got, _ := json.MarshalIndent(all[:2], "", "  ")
	got = append(got, '\n')
	want, _ := os.ReadFile(filepath.Join("testdata", "golden_vpn_all_switch_off.json"))
	if string(got) != string(want) {
		t.Fatalf("first two elements drifted from golden.\n--- want ---\n%s\n--- got ---\n%s", want, got)
	}
	el := all[2]
	if el.PlanTier != "campaign" || el.Channel != "campaign" || el.ProfileClass != "campaign" || el.ServiceTier != "standard" {
		t.Fatalf("campaign element keys: %+v", el)
	}
	if el.Status != "active" {
		t.Fatalf("status want active, got %s", el.Status)
	}
	if len(el.Protocols) != 2 || !strings.Contains(el.Protocols[0].URL, "otun-camp-uuid") {
		t.Fatalf("protocols must belong to campaign uuid: %+v", el.Protocols)
	}
	if el.TrafficUsed != 42 || el.TrafficLimit != 10*1024*1024*1024 || el.ExpireAt != "2026-08-23T10:00:00Z" {
		t.Fatalf("traffic/expire from otun sync: %+v", el)
	}
	if el.Campaign == nil || el.Campaign.ClaimsActive != 1 || el.Campaign.GrantedDaysTotal != 7 || el.Campaign.StackLimit == nil || el.Campaign.StackLimit.MaxDays != 56 {
		t.Fatalf("campaign sub-object: %+v", el.Campaign)
	}
	if el.ConfigVersion == "" || el.ActiveClass != "" || len(el.Profiles) != 0 {
		t.Fatalf("campaign element must have config_version and no profiles[]/active_class: %+v", el)
	}
	// JSON 键名冻结
	raw, _ := json.Marshal(el)
	for _, k := range []string{`"profile_class":"campaign"`, `"plan_tier":"campaign"`, `"claims_active":1`, `"granted_days_total":7`, `"granted_traffic_total":`, `"last_claim_at":`, `"stack_limit":{"max_days":56,"max_traffic":214748364800}`, `"status":"active"`} {
		if !strings.Contains(string(raw), k) {
			t.Fatalf("missing frozen key %s in %s", k, raw)
		}
	}
	// 只有 campaign 的用户（HasActive=false 情形由 subscriptionClient=nil 模拟为 hasActive；这里再验 golden 元素不受 caps 影响）
}

// 2b. 到期保留期内 → status=expired 仍下发；已清理（is_current=false）→ 不下发（C5）；otun sync 失败 → protocols 空但元素仍在。
func TestVPNAll_WithCaps_ExpiredAndCleaned(t *testing.T) {
	const userID = "u-exp"
	store := &fakeVPNStore{rows: []*models.VPNProvision{campaignRow(userID, "otun-camp-uuid", time.Now().Add(-24*time.Hour))}}
	s, otun := newCampaignSvc(t, store, newFakeGrantStore())
	otun.syncFn = func(string) (int, string) { return 403, `{"error":"user disabled"}` }
	all, err := s.GetUserVPNSubscribeConfigAllWithCaps(context.Background(), userID, ParseClientCapabilities("campaign-profile"))
	if err != nil || len(all) != 1 {
		t.Fatalf("want 1 campaign element, got %d err=%v", len(all), err)
	}
	if all[0].Status != "expired" || len(all[0].Protocols) != 0 || all[0].PlanTier != "campaign" {
		t.Fatalf("expired element: %+v", all[0])
	}
	// 清理后不下发
	store.rows[0].IsCurrent = false
	all, _ = s.GetUserVPNSubscribeConfigAllWithCaps(context.Background(), userID, ParseClientCapabilities("campaign-profile"))
	if len(all) != 0 {
		t.Fatalf("cleaned campaign row must not be emitted, got %d", len(all))
	}
	// 无头 → 空数组（非 nil）
	all, _ = s.GetUserVPNSubscribeConfigAllWithCaps(context.Background(), userID, nil)
	if all == nil || len(all) != 0 {
		t.Fatalf("no caps must be empty non-nil slice, got %v", all)
	}
}

// 3. /vpn/version：无头不因活动账号变化（C6）；有头才纳入。
func TestVPNVersion_NoCaps_UnaffectedByCampaign(t *testing.T) {
	const userID = "u-golden"
	base := goldenStore(userID)
	s0, _ := newCampaignSvc(t, base, newFakeGrantStore())
	v0, err := s0.GetUserConfigVersion(context.Background(), userID)
	if err != nil {
		t.Fatal(err)
	}
	with := goldenStore(userID)
	with.rows = append(with.rows, campaignRow(userID, "otun-camp-uuid", mustTime("2099-08-23T10:00:00Z")))
	s1, _ := newCampaignSvc(t, with, newFakeGrantStore())
	v1, _ := s1.GetUserConfigVersion(context.Background(), userID)
	v1nocaps, _ := s1.GetUserConfigVersionWithCaps(context.Background(), userID, ParseClientCapabilities(""))
	if v0 != v1 || v0 != v1nocaps {
		t.Fatalf("no-caps version must not change with campaign row: %s vs %s / %s", v0, v1, v1nocaps)
	}
	v1caps, _ := s1.GetUserConfigVersionWithCaps(context.Background(), userID, ParseClientCapabilities("campaign-profile"))
	if v1caps == v0 || v1caps == "" {
		t.Fatalf("caps version must include campaign face: %s vs %s", v1caps, v0)
	}
}

// 4. 分区隔离：basic + campaign 并存互不命中；不分区读口排除 campaign；换绑回收（isResidential=nil / false）不碰活动账号。
func TestCampaign_PartitionIsolation(t *testing.T) {
	const userID = "u-both"
	basic := standardRow(userID, "sub-basic", "uuid-basic")
	camp := campaignRow(userID, "uuid-camp", time.Now().Add(24*time.Hour))
	store := &fakeVPNStore{rows: []*models.VPNProvision{basic, camp}} // campaign 更晚（模拟 created_at DESC 先命中）
	s := newSvc(true, store)
	ctx := context.Background()

	if got, _ := s.resolveExistingCurrent(ctx, userID, false); got == nil || got.ID != basic.ID {
		t.Fatalf("basic partition must hit basic, got %+v", got)
	}
	if got, _ := s.resolveExistingOtunUUID(ctx, userID, false); got == nil || *got != "uuid-basic" {
		t.Fatalf("basic partition uuid must be basic, got %v", got)
	}
	if got, _ := s.resolveExistingCurrent(ctx, userID, true); got != nil {
		t.Fatalf("residential partition must be empty, got %+v", got)
	}
	if got, _ := store.GetCurrentByUserAndFace(ctx, userID, models.ProductFaceCampaign); got == nil || got.ID != camp.ID {
		t.Fatalf("campaign face must hit campaign, got %+v", got)
	}
	// 不分区（MultiService 开关 false 的旧路径 + 换绑 isResidential=nil）都排除 campaign
	s0 := newSvc(false, store)
	if got, _ := s0.resolveExistingCurrent(ctx, userID, false); got == nil || got.ID != basic.ID {
		t.Fatalf("legacy unpartitioned must hit basic not campaign, got %+v", got)
	}
	if got, _ := s0.resolveDeprovisionTarget(ctx, userID, nil); got == nil || got.ID != basic.ID {
		t.Fatalf("deprovision target (nil face) must be basic, got %+v", got)
	}
	f := false
	if got, _ := s.resolveDeprovisionTarget(ctx, userID, &f); got == nil || got.ID != basic.ID {
		t.Fatalf("deprovision target (basic) must be basic, got %+v", got)
	}
	// campaign-only：老路径一无所见
	only := &fakeVPNStore{rows: []*models.VPNProvision{camp}}
	s2 := newSvc(true, only)
	if got, _ := s2.resolveExistingCurrent(ctx, userID, false); got != nil {
		t.Fatalf("campaign-only: basic partition must be empty, got %+v", got)
	}
	if got, _ := only.GetCurrentByUser(ctx, userID); got != nil {
		t.Fatalf("campaign-only: GetCurrentByUser must be nil, got %+v", got)
	}
	if _, err := s2.resolveDeprovisionTarget(ctx, userID, &f); err == nil {
		t.Fatalf("campaign-only: deprovision basic must be ErrNotFound")
	}
}

// 5. 叠加算术 + 幂等 + 到期重置 + 撤销。
func TestCampaign_ProvisionStackingAndRevoke(t *testing.T) {
	const userID = "u-stack"
	const GB = int64(1024 * 1024 * 1024)
	store := &fakeVPNStore{rows: []*models.VPNProvision{standardRow(userID, "sub-basic", "uuid-basic")}}
	grants := newFakeGrantStore()
	s, otun := newCampaignSvc(t, store, grants)
	s.logRepo = nil
	ctx := context.Background()
	basicBefore := *store.rows[0]

	req := func(sub string, days int, gb int64) *models.ProvisionRequest {
		return &models.ProvisionRequest{UserID: userID, UserEmail: "u@x", PlanTier: "campaign", Channel: "campaign",
			PurchaseType: "one_time", SubscriptionID: sub, ChannelSubID: "campaign-claim-" + sub, ExpireDays: days, TrafficLimit: gb * GB}
	}
	// 5.1 首领：新 uuid、POST product_face=campaign、行 product_face=campaign
	t0 := time.Now()
	r1, err := s.ProvisionVPNUser(ctx, req("c1", 7, 10))
	if err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if len(otun.posts) != 1 || otun.posts[0].ProductFace != "campaign" || otun.posts[0].ServiceTier != "standard" || otun.posts[0].AuthUserID != userID {
		t.Fatalf("otun POST: %+v", otun.posts)
	}
	camp, _ := store.GetCurrentByUserAndFace(ctx, userID, models.ProductFaceCampaign)
	if camp == nil || camp.ProductFace != "campaign" || camp.PlanTier != "campaign" || camp.ServiceTier != "standard" || *camp.OtunUUID != r1.VPNUserID || *camp.OtunUUID == "uuid-basic" {
		t.Fatalf("campaign row after first claim: %+v", camp)
	}
	if camp.TrafficLimit != 10*GB || camp.ExpireAt.Before(t0.AddDate(0, 0, 7).Add(-time.Minute)) {
		t.Fatalf("first claim quota: limit=%d expire=%v", camp.TrafficLimit, camp.ExpireAt)
	}
	// 5.2 二领（另一 subscription）：expire += 7d，traffic += 10G，PUT 而非 POST
	exp1 := *camp.ExpireAt
	if _, err := s.ProvisionVPNUser(ctx, req("c2", 7, 10)); err != nil {
		t.Fatalf("second claim: %v", err)
	}
	if len(otun.posts) != 1 || len(otun.puts) != 1 || otun.putUUID[0] != *camp.OtunUUID {
		t.Fatalf("stack must PUT once on campaign uuid: posts=%d puts=%d", len(otun.posts), len(otun.puts))
	}
	if camp.TrafficLimit != 20*GB || !camp.ExpireAt.Equal(exp1.AddDate(0, 0, 7)) {
		t.Fatalf("stack arithmetic: limit=%d expire=%v want %v", camp.TrafficLimit, camp.ExpireAt, exp1.AddDate(0, 0, 7))
	}
	if otun.puts[0].TrafficLimit != 20*GB || otun.puts[0].Enabled == nil || !*otun.puts[0].Enabled {
		t.Fatalf("PUT payload: %+v", otun.puts[0])
	}
	// 5.3 同 subscription 重放：幂等，不再叠加
	if _, err := s.ProvisionVPNUser(ctx, req("c2", 7, 10)); err != nil {
		t.Fatal(err)
	}
	if camp.TrafficLimit != 20*GB || len(otun.puts) != 1 {
		t.Fatalf("replay must be idempotent: limit=%d puts=%d", camp.TrafficLimit, len(otun.puts))
	}
	agg, _ := grants.AggregateActiveByUser(ctx, userID)
	if agg.ClaimsActive != 2 || agg.GrantedDaysTotal != 14 || agg.GrantedTrafficTotal != 20*GB {
		t.Fatalf("grants aggregate: %+v", agg)
	}
	// 5.4a（验收 F4）otun PUT 失败 → 撤销整体失败：grant 仍 active、行不变、可重试
	otun.putFail = 1
	if _, err := s.RevokeCampaign(ctx, &CampaignRevokeRequest{UserID: userID, SubscriptionID: "c1", Days: 7, TrafficBytes: 10 * GB}); err == nil {
		t.Fatalf("revoke must fail when otun PUT fails")
	}
	if g, _ := grants.GetBySubscriptionID(ctx, "c1"); g.Status != "active" {
		t.Fatalf("grant must stay active after failed otun sync, got %s", g.Status)
	}
	if camp.TrafficLimit != 20*GB || !camp.ExpireAt.Equal(exp1.AddDate(0, 0, 7)) {
		t.Fatalf("row must be untouched after failed otun sync: %d %v", camp.TrafficLimit, camp.ExpireAt)
	}
	// 5.4 撤销 c1（重试成功）：expire -= 7d，traffic -= 10G；再撤 → noop；撤不存在 grant → noop
	res, err := s.RevokeCampaign(ctx, &CampaignRevokeRequest{UserID: userID, SubscriptionID: "c1", Days: 7, TrafficBytes: 10 * GB})
	if err != nil || res.Noop || res.RevokedDays != 7 || res.TrafficLimit != 10*GB {
		t.Fatalf("revoke: %+v err=%v", res, err)
	}
	if !camp.ExpireAt.Equal(exp1) || camp.TrafficLimit != 10*GB {
		t.Fatalf("after revoke: expire=%v want %v limit=%d", camp.ExpireAt, exp1, camp.TrafficLimit)
	}
	res, _ = s.RevokeCampaign(ctx, &CampaignRevokeRequest{UserID: userID, SubscriptionID: "c1", Days: 7, TrafficBytes: 10 * GB})
	if !res.Noop || camp.TrafficLimit != 10*GB {
		t.Fatalf("second revoke must be noop: %+v", res)
	}
	res, _ = s.RevokeCampaign(ctx, &CampaignRevokeRequest{UserID: userID, SubscriptionID: "nope", Days: 7, TrafficBytes: 10 * GB})
	if !res.Noop {
		t.Fatalf("revoke unknown grant must be noop: %+v", res)
	}
	// 5.5 撤销超量：下限 now / 0（otun 收 1 字节）
	res, _ = s.RevokeCampaign(ctx, &CampaignRevokeRequest{UserID: userID, Days: 365, TrafficBytes: 999 * GB})
	if res.TrafficLimit != 0 || camp.ExpireAt.After(time.Now().Add(time.Second)) {
		t.Fatalf("floor: %+v expire=%v", res, camp.ExpireAt)
	}
	if last := otun.puts[len(otun.puts)-1]; last.TrafficLimit != 1 {
		t.Fatalf("otun floor must be 1 byte (0=unlimited in otun): %+v", last)
	}
	// 5.6 到期后再领：从 now 起算、流量重置、旧 grant 作废、otun 走 POST 复位
	past := time.Now().Add(-48 * time.Hour)
	camp.ExpireAt = &past
	camp.TrafficLimit = 3 * GB
	postsBefore := len(otun.posts)
	if _, err := s.ProvisionVPNUser(ctx, req("c3", 7, 10)); err != nil {
		t.Fatalf("reclaim after expiry: %v", err)
	}
	if len(otun.posts) != postsBefore+1 || otun.posts[postsBefore].ProductFace != "campaign" || otun.posts[postsBefore].UUID != *camp.OtunUUID {
		t.Fatalf("fresh period must POST (reset) on same uuid: %+v", otun.posts)
	}
	if camp.TrafficLimit != 10*GB || camp.ExpireAt.Before(time.Now().AddDate(0, 0, 7).Add(-time.Minute)) {
		t.Fatalf("fresh period quota: limit=%d expire=%v", camp.TrafficLimit, camp.ExpireAt)
	}
	agg, _ = grants.AggregateActiveByUser(ctx, userID)
	if agg.ClaimsActive != 1 || agg.GrantedDaysTotal != 7 {
		t.Fatalf("fresh period aggregate must reset: %+v", agg)
	}
	// 5.7 basic 行全程不变；campaign 行只有一条 current
	if *store.rows[0] != basicBefore {
		t.Fatalf("basic row mutated: before=%+v after=%+v", basicBefore, *store.rows[0])
	}
	n := 0
	for _, r := range store.rows {
		if r.IsCurrent && r.EffectiveProductFace() == models.ProductFaceCampaign {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("want exactly 1 current campaign row, got %d", n)
	}
	// 5.8 内部读口
	view, err := s.GetCampaignProfile(ctx, userID)
	if err != nil || !view.HasProfile || view.Status != "active" || view.RemainingDays < 6 || view.RemainingDays > 7 || view.ClaimsActive != 1 {
		t.Fatalf("profile view: %+v err=%v", view, err)
	}
	if v2, _ := s.GetCampaignProfile(ctx, "nobody"); v2.HasProfile {
		t.Fatalf("no profile must be has_profile=false")
	}
	// 5.9 无 grants 仓接线 → 开通拒绝（明确报错而非静默）
	s.campaignGrants = nil
	if _, err := s.ProvisionVPNUser(ctx, req("c9", 7, 10)); err == nil {
		t.Fatalf("provision without grants store must error")
	}
}

// 6. cleanup：到期 + 保留期后 → is_current=false + otun disable；保留期内不动。
func TestCampaign_CleanupRetention(t *testing.T) {
	const userID = "u-clean"
	// fakeVPNStore 未实现 ListExpiredCampaignRows → 走类型断言失败分支（返回 0，不 panic）；用一个带列表的包装验证行为
	row := campaignRow(userID, "uuid-camp", time.Now().Add(-10*24*time.Hour))
	store := &fakeVPNStoreWithList{fakeVPNStore: &fakeVPNStore{rows: []*models.VPNProvision{row}}}
	s, _ := newCampaignSvc(t, store.fakeVPNStore, newFakeGrantStore())
	s.vpnRepo = store
	if n := s.CleanupExpiredCampaignFaces(context.Background(), 30*24*time.Hour, 10); n != 0 || !row.IsCurrent {
		t.Fatalf("within retention must not clean: n=%d current=%v", n, row.IsCurrent)
	}
	if n := s.CleanupExpiredCampaignFaces(context.Background(), 7*24*time.Hour, 10); n != 1 || row.IsCurrent || row.Status != "expired" {
		t.Fatalf("after retention must clean: n=%d row=%+v", n, row)
	}
}

type fakeVPNStoreWithList struct{ *fakeVPNStore }

func (f *fakeVPNStoreWithList) ListExpiredCampaignRows(_ context.Context, before time.Time, _ int) ([]*models.VPNProvision, error) {
	var out []*models.VPNProvision
	for _, r := range f.rows {
		if r.IsCurrent && r.EffectiveProductFace() == models.ProductFaceCampaign && r.ExpireAt != nil && r.ExpireAt.Before(before) {
			out = append(out, r)
		}
	}
	return out, nil
}

// 7. MapPlanToServiceTier / MapPlanToProductFace / ParseClientCapabilities 纯函数。
func TestCampaign_Mappings(t *testing.T) {
	if models.MapPlanToServiceTier("campaign") != "standard" || models.MapPlanToProductFace("campaign") != "campaign" ||
		models.MapPlanToProductFace("basic") != "basic" || models.MapPlanToProductFace("premium") != "basic" ||
		models.MapPlanToProductFace("unlimited") != "basic" || models.MapPlanToProductFace("residential") != "residential" {
		t.Fatal("mapping")
	}
	if !ParseClientCapabilities("campaign-profile").Has(CapabilityCampaignProfile) || ParseClientCapabilities("").Has(CapabilityCampaignProfile) ||
		!ParseClientCapabilities(" foo , CAMPAIGN-PROFILE ").Has(CapabilityCampaignProfile) || ClientCapabilities(nil).Has(CapabilityCampaignProfile) {
		t.Fatal("caps parse")
	}
	// 分区谓词函数：basic 分区不含 campaign
	if models.PartitionFace(false) != "basic" || models.PartitionFace(true) != "residential" {
		t.Fatal("partition face")
	}
	if models.ProductFaceFor("campaign", "standard") != "campaign" || models.ProductFaceFor("basic", "residential") != "residential" || models.ProductFaceFor("premium", "premium") != "basic" {
		t.Fatal("product face for")
	}
}
