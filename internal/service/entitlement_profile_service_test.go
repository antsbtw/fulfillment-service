package service

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/wenwu/saas-platform/fulfillment-service/internal/models"
)

// ==================== 内存 fakes ====================

// fakeEntitlementStore 模拟 vpn_entitlement_profiles / entries 两张表（含唯一键语义）。
type fakeEntitlementStore struct {
	profiles []*models.EntitlementProfile
	entries  []*models.EntitlementEntry
	seq      int
}

func (f *fakeEntitlementStore) nextID(prefix string) string {
	f.seq++
	return fmt.Sprintf("%s-%d", prefix, f.seq)
}

func (f *fakeEntitlementStore) GetProfilesByUserFace(_ context.Context, userID, face string) ([]*models.EntitlementProfile, error) {
	var out []*models.EntitlementProfile
	for _, p := range f.profiles {
		if p.UserID == userID && p.ServiceFace == face {
			cp := *p
			out = append(out, &cp) // 拷贝：模拟 DB 读出的独立对象
		}
	}
	return out, nil
}

func (f *fakeEntitlementStore) UpsertProfile(_ context.Context, p *models.EntitlementProfile) error {
	for i, ex := range f.profiles {
		if ex.UserID == p.UserID && ex.ServiceFace == p.ServiceFace && ex.Class == p.Class {
			cp := *p
			cp.ID = ex.ID
			cp.CreatedAt = ex.CreatedAt
			f.profiles[i] = &cp
			p.ID = ex.ID
			return nil
		}
	}
	p.ID = f.nextID("prof")
	cp := *p
	f.profiles = append(f.profiles, &cp)
	return nil
}

func (f *fakeEntitlementStore) CreateEntry(_ context.Context, e *models.EntitlementEntry) (bool, error) {
	for _, ex := range f.entries {
		if ex.Channel == e.Channel && ex.ChannelSubID == e.ChannelSubID && ex.SourceEventID == e.SourceEventID {
			return false, nil
		}
	}
	e.ID = f.nextID("entry")
	cp := *e
	f.entries = append(f.entries, &cp)
	return true, nil
}

func (f *fakeEntitlementStore) ListEntriesByProfile(_ context.Context, profileID string) ([]*models.EntitlementEntry, error) {
	var out []*models.EntitlementEntry
	for _, e := range f.entries {
		if e.ProfileID == profileID {
			cp := *e
			out = append(out, &cp)
		}
	}
	return out, nil
}

func (f *fakeEntitlementStore) MarkEntryRevoked(_ context.Context, id string, at time.Time) error {
	for _, e := range f.entries {
		if e.ID == id && e.RevokedAt == nil {
			t := at
			e.RevokedAt = &t
		}
	}
	return nil
}

func (f *fakeEntitlementStore) ListFacesDueForResolve(_ context.Context, horizon time.Time, limit int) ([][2]string, error) {
	var out [][2]string
	seen := map[string]bool{}
	for _, a := range f.profiles {
		if a.Status != models.ProfileStatusActive || a.ExpireAt == nil || a.ExpireAt.After(horizon) {
			continue
		}
		for _, w := range f.profiles {
			if w.UserID == a.UserID && w.ServiceFace == a.ServiceFace && w.Class == models.EntitlementClassPurchase &&
				w.DaysRemaining > 0 && w.ID != a.ID {
				k := a.UserID + "|" + a.ServiceFace
				if !seen[k] {
					seen[k] = true
					out = append(out, [2]string{a.UserID, a.ServiceFace})
				}
			}
		}
	}
	return out, nil
}

func (f *fakeEntitlementStore) profile(userID, face, class string) *models.EntitlementProfile {
	for _, p := range f.profiles {
		if p.UserID == userID && p.ServiceFace == face && p.Class == class {
			return p
		}
	}
	return nil
}

// fakeProjectionStore 模拟 vpn_provisions 投影行。
type fakeProjectionStore struct {
	rows []*models.VPNProvision
}

func (f *fakeProjectionStore) GetCurrentByUserAndServicePartition(_ context.Context, userID string, isResidential bool) (*models.VPNProvision, error) {
	for i := len(f.rows) - 1; i >= 0; i-- {
		r := f.rows[i]
		if r.UserID == userID && r.IsCurrent && (r.ServiceTier == models.ServiceTierResidential) == isResidential {
			return r, nil
		}
	}
	return nil, nil
}

func (f *fakeProjectionStore) UpdateProjection(_ context.Context, id string, expireAt *time.Time, trafficLimit int64, channel, activeClass string) error {
	for _, r := range f.rows {
		if r.ID == id {
			r.ExpireAt = expireAt
			r.TrafficLimit = trafficLimit
			r.Channel = channel
		}
	}
	return nil
}

// fakeOtunGateway 模拟该面 otun 账号：记录每次 Push、可设当前 used。
type pushRecord struct {
	UUID         string
	Face         string
	TrafficLimit int64
	ExpireAt     time.Time
}

type fakeOtunGateway struct {
	used   map[string]int64
	pushes []pushRecord
}

func (f *fakeOtunGateway) ReadUsage(_ context.Context, otunUUID, _ string) (int64, error) {
	return f.used[otunUUID], nil
}

func (f *fakeOtunGateway) Push(_ context.Context, otunUUID, face, _, _, _ string, trafficLimit int64, expireAt time.Time) error {
	f.pushes = append(f.pushes, pushRecord{UUID: otunUUID, Face: face, TrafficLimit: trafficLimit, ExpireAt: expireAt})
	return nil
}

func (f *fakeOtunGateway) last() *pushRecord {
	if len(f.pushes) == 0 {
		return nil
	}
	return &f.pushes[len(f.pushes)-1]
}

// clock 可拨快的时钟。
type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }

const GB = int64(1024 * 1024 * 1024)

type harness struct {
	svc   *EntitlementProfileService
	store *fakeEntitlementStore
	prov  *fakeProjectionStore
	otun  *fakeOtunGateway
	clk   *clock
}

func newHarness(t *testing.T, enabled bool) *harness {
	t.Helper()
	clk := &clock{t: time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)}
	store := &fakeEntitlementStore{}
	otun := &fakeOtunGateway{used: map[string]int64{}}
	prov := &fakeProjectionStore{}
	svc := &EntitlementProfileService{
		store: store, provisions: prov, otun: otun,
		enabled: enabled, switchLead: 65 * time.Minute, now: clk.now,
	}
	return &harness{svc: svc, store: store, prov: prov, otun: otun, clk: clk}
}

// addAccount 给该 user 该面挂一条投影行（= 已有 otun 账号）。
func (h *harness) addAccount(userID, face, otunUUID string) *models.VPNProvision {
	tier := models.ServiceTierStandard
	plan := "basic"
	if face == models.ServiceFaceResidential {
		tier, plan = models.ServiceTierResidential, "residential"
	}
	vp := &models.VPNProvision{
		ID: "prov-" + face + "-" + userID, UserID: userID, ServiceTier: tier, PlanTier: plan,
		Channel: "stripe", Status: models.VPNProvisionStatusActive, OtunUUID: ptrStr(otunUUID), IsCurrent: true,
	}
	h.prov.rows = append(h.prov.rows, vp)
	return vp
}

func (h *harness) apply(t *testing.T, in *EntryInput) bool {
	t.Helper()
	_, applied, err := h.svc.ApplyEntry(context.Background(), in)
	if err != nil {
		t.Fatalf("ApplyEntry(%+v): %v", in, err)
	}
	return applied
}

func (h *harness) sync(t *testing.T, userID, face string) *resolveResult {
	t.Helper()
	res, err := h.svc.Sync(context.Background(), userID, face)
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	return res
}

func days(n int) time.Duration { return time.Duration(n) * 24 * time.Hour }

func subEntry(user, face, channel, subID string, periodEnd time.Time, traffic int64) *EntryInput {
	ps := periodEnd.Add(-days(30))
	return &EntryInput{UserID: user, ServiceFace: face, SubscriptionID: subID, Channel: channel,
		ChannelSubID: "chsub-" + subID, PurchaseType: models.PurchaseTypeSubscription,
		PeriodStart: &ps, PeriodEnd: &periodEnd, Traffic: traffic, Days: 30}
}

func oneTimeEntry(user, face, channel, subID string, d int, traffic int64) *EntryInput {
	return &EntryInput{UserID: user, ServiceFace: face, SubscriptionID: subID, Channel: channel,
		ChannelSubID: "chsub-" + subID, PurchaseType: models.PurchaseTypeOneTime, Days: d, Traffic: traffic}
}

// ==================== 1. stripe 年付 + apple 月订 → 订阅 profile max，剩余天数不被抹 ====================

func TestEntitlement_SubscriptionMax_YearlyNotErasedByMonthly(t *testing.T) {
	h := newHarness(t, true)
	const user, face = "u1", models.ServiceFaceStandard
	h.addAccount(user, face, "uuid-std")
	now := h.clk.now()

	yearEnd := now.Add(days(365))
	monthEnd := now.Add(days(30))
	h.apply(t, subEntry(user, face, "stripe", "sub-stripe-year", yearEnd, 50*GB))
	h.sync(t, user, face)
	h.apply(t, subEntry(user, face, "apple", "sub-apple-month", monthEnd, 50*GB))
	res := h.sync(t, user, face)

	sub := res.Profiles[models.EntitlementClassSubscription]
	if sub == nil || sub.ExpireAt == nil || !sub.ExpireAt.Equal(yearEnd) {
		t.Fatalf("subscription profile expire must be max(period_end)=%s, got %+v", yearEnd, sub)
	}
	if res.ActiveClass != models.EntitlementClassSubscription {
		t.Fatalf("active_class want subscription, got %s", res.ActiveClass)
	}
	// otun 账号被推到 365 天（不是 30 天）
	if p := h.otun.last(); p == nil || !p.ExpireAt.Equal(yearEnd) {
		t.Fatalf("otun expire must be pushed to yearEnd, got %+v", p)
	}
	// 两个 kind 都在
	proj, _ := h.svc.Project(context.Background(), user, face, nil)
	if len(proj.Profiles) != 1 || len(proj.Profiles[0].Kinds) != 2 {
		t.Fatalf("want one subscription profile with kinds [apple stripe], got %+v", proj.Profiles)
	}
}

// ==================== 2. 订阅期内 credit 30 天 → 桶 waiting；到期接续；再来订阅回切结算 ====================

func TestEntitlement_PurchaseBucket_WaitThenActivateThenSwitchBack(t *testing.T) {
	h := newHarness(t, true)
	const user, face = "u2", models.ServiceFaceStandard
	vp := h.addAccount(user, face, "uuid-std")
	ctx := context.Background()
	t0 := h.clk.now()

	// 订阅到 t0+10d
	subEnd := t0.Add(days(10))
	h.apply(t, subEntry(user, face, "stripe", "sub-1", subEnd, 50*GB))
	h.sync(t, user, face)

	// 订阅期内买 credit 30 天 100GB → 桶 waiting，effective_from = 订阅到期，不消耗
	h.apply(t, oneTimeEntry(user, face, "credit", "order-1", 30, 100*GB))
	res := h.sync(t, user, face)
	if res.ActiveClass != models.EntitlementClassSubscription {
		t.Fatalf("subscription still valid → active must stay subscription, got %s", res.ActiveClass)
	}
	pur := res.Profiles[models.EntitlementClassPurchase]
	if pur.Status != models.ProfileStatusWaiting || pur.DaysRemaining != 30 || pur.ExpireAt != nil {
		t.Fatalf("bucket must be waiting 30d no expire, got %+v", pur)
	}
	proj, _ := h.svc.Project(ctx, user, face, nil)
	var pv *models.VPNProfileView
	for i := range proj.Profiles {
		if proj.Profiles[i].Class == models.EntitlementClassPurchase {
			pv = &proj.Profiles[i]
		}
	}
	if pv == nil || pv.EffectiveFrom == nil || *pv.EffectiveFrom != subEnd.UTC().Format(time.RFC3339) || pv.ExpireAt != nil {
		t.Fatalf("waiting bucket effective_from must be subscription expire, expire_at null; got %+v", pv)
	}
	if pv.DaysRemaining == nil || *pv.DaysRemaining != 30 || pv.DaysConsumed == nil || *pv.DaysConsumed != 0 {
		t.Fatalf("waiting bucket days_remaining=30 consumed=0, got %+v", pv)
	}
	// 投影行/otun 仍是订阅到期
	if p := h.otun.last(); p == nil || !p.ExpireAt.Equal(subEnd) {
		t.Fatalf("otun expire must remain subscription end while waiting, got %+v", p)
	}

	// 桥接：到期前 30 分钟（< lead 65min）→ 调度器把 otun expire 提前推到 subEnd+30d，但 active_class 仍 subscription
	h.clk.t = subEnd.Add(-30 * time.Minute)
	pushesBefore := len(h.otun.pushes)
	sched := NewEntitlementScheduler(h.svc, time.Minute, 65*time.Minute)
	sched.runOnce(ctx)
	if len(h.otun.pushes) == pushesBefore {
		t.Fatalf("scheduler must bridge-push before subscription expiry")
	}
	if p := h.otun.last(); !p.ExpireAt.Equal(subEnd.Add(days(30))) {
		t.Fatalf("bridge push expire want subEnd+30d, got %s", p.ExpireAt)
	}
	if !vp.ExpireAt.Equal(subEnd) {
		t.Fatalf("projection row must still show subscription expire during bridge, got %s", vp.ExpireAt)
	}
	res = h.sync(t, user, face)
	if res.ActiveClass != models.EntitlementClassSubscription {
		t.Fatalf("bridge must not flip active_class early, got %s", res.ActiveClass)
	}

	// 订阅到期后 1 分钟：桶接续生效，active_since=now，expire=now+30d，基线=otun 当前 used
	h.otun.used["uuid-std"] = 7 * GB
	h.clk.t = subEnd.Add(time.Minute)
	res = h.sync(t, user, face)
	if res.ActiveClass != models.EntitlementClassPurchase {
		t.Fatalf("after subscription expiry bucket must activate, got %s", res.ActiveClass)
	}
	pur = res.Profiles[models.EntitlementClassPurchase]
	wantExp := h.clk.now().Add(days(30))
	if pur.Status != models.ProfileStatusActive || pur.ExpireAt == nil || !pur.ExpireAt.Equal(wantExp) || pur.TrafficBaseline != 7*GB {
		t.Fatalf("activated bucket want active expire=%s baseline=7GB, got %+v", wantExp, pur)
	}
	// otun：expire 推后 + traffic_limit = baseline + 桶剩余（基线法，不 reset used）
	if p := h.otun.last(); !p.ExpireAt.Equal(wantExp) || p.TrafficLimit != 7*GB+100*GB {
		t.Fatalf("otun push want expire=%s limit=107GB, got %+v", wantExp, p)
	}
	if !vp.ExpireAt.Equal(wantExp) || vp.TrafficLimit != 100*GB {
		t.Fatalf("projection row want expire=%s limit=100GB(bucket), got exp=%s limit=%d", wantExp, vp.ExpireAt, vp.TrafficLimit)
	}

	// 用了 10 天 + 3GB → days_remaining 递减到 20、traffic_used=3GB
	h.clk.advance(days(10))
	h.otun.used["uuid-std"] = 10 * GB
	used := int64(10 * GB)
	proj, _ = h.svc.Project(ctx, user, face, &used)
	for i := range proj.Profiles {
		if proj.Profiles[i].Class == models.EntitlementClassPurchase {
			pv = &proj.Profiles[i]
		}
	}
	if *pv.DaysRemaining != 20 || *pv.DaysConsumed != 10 || pv.TrafficUsed != 3*GB || *pv.TrafficRemaining != 97*GB {
		t.Fatalf("active bucket after 10d/3GB: want remaining=20 consumed=10 used=3GB remaining=97GB, got %+v", pv)
	}

	// 新订阅到来 → 回切：桶结算 days_remaining=20、days_consumed=10、traffic_used=3GB，回到 waiting
	newSubEnd := h.clk.now().Add(days(30))
	h.apply(t, subEntry(user, face, "apple", "sub-2", newSubEnd, 50*GB))
	res = h.sync(t, user, face)
	if res.ActiveClass != models.EntitlementClassSubscription {
		t.Fatalf("new subscription must take over, got %s", res.ActiveClass)
	}
	pur = res.Profiles[models.EntitlementClassPurchase]
	if pur.Status != models.ProfileStatusWaiting || pur.DaysRemaining != 20 || pur.DaysConsumed != 10 || pur.TrafficUsed != 3*GB || pur.ExpireAt != nil {
		t.Fatalf("settled bucket want waiting remaining=20 consumed=10 used=3GB, got %+v", pur)
	}
	if !pur.EffectiveFrom.Equal(newSubEnd) {
		t.Fatalf("settled bucket effective_from want new subscription end %s, got %v", newSubEnd, pur.EffectiveFrom)
	}
	if p := h.otun.last(); !p.ExpireAt.Equal(newSubEnd) || p.TrafficLimit != 50*GB {
		t.Fatalf("otun after switch-back want subscription expire/limit, got %+v", p)
	}
}

// ==================== 3. 两笔一次性 → 天数与流量叠加 ====================

func TestEntitlement_TwoOneTime_Stack(t *testing.T) {
	h := newHarness(t, true)
	const user, face = "u3", models.ServiceFaceResidential
	h.addAccount(user, face, "uuid-res")

	h.apply(t, oneTimeEntry(user, face, "credit", "o-1", 30, 100*GB))
	h.apply(t, oneTimeEntry(user, face, "gift_card", "o-2", 60, 500*GB))
	res := h.sync(t, user, face)

	pur := res.Profiles[models.EntitlementClassPurchase]
	if pur.DaysRemaining != 90 || pur.TrafficLimit != 600*GB {
		t.Fatalf("bucket want 90d/600GB, got %+v", pur)
	}
	// 无订阅 → 桶立即生效，expire = now+90d
	if res.ActiveClass != models.EntitlementClassPurchase || !pur.ExpireAt.Equal(h.clk.now().Add(days(90))) {
		t.Fatalf("bucket must be active now+90d, got class=%s %+v", res.ActiveClass, pur)
	}
	// 生效中再追加 15 天 → 到期顺延
	h.apply(t, oneTimeEntry(user, face, "stripe", "o-3", 15, 10*GB))
	res = h.sync(t, user, face)
	pur = res.Profiles[models.EntitlementClassPurchase]
	if pur.DaysRemaining != 105 || !pur.ExpireAt.Equal(h.clk.now().Add(days(105))) || pur.TrafficLimit != 610*GB {
		t.Fatalf("active bucket + 15d want 105d/610GB, got %+v", pur)
	}
	// 重放同一笔（同 subscription_id）不重复加天
	if h.apply(t, oneTimeEntry(user, face, "stripe", "o-3", 15, 10*GB)) {
		t.Fatalf("replay of same one-time entry must be idempotent")
	}
	// kind 映射
	proj, _ := h.svc.Project(context.Background(), user, face, nil)
	if got := fmt.Sprint(proj.Profiles[0].Kinds); got != "[credit gift_card stripe_onetime]" {
		t.Fatalf("kinds want [credit gift_card stripe_onetime], got %s", got)
	}
}

// ==================== 6. 桶扣减不低于 0 ====================

func TestEntitlement_Revoke_FloorZero(t *testing.T) {
	h := newHarness(t, true)
	const user, face = "u6", models.ServiceFaceStandard
	h.addAccount(user, face, "uuid-std")
	ctx := context.Background()

	// 订阅有效 + 桶 waiting 30d
	h.apply(t, subEntry(user, face, "stripe", "sub-1", h.clk.now().Add(days(20)), 50*GB))
	h.apply(t, &EntryInput{UserID: user, ServiceFace: face, SubscriptionID: "gift-1", Channel: "gift",
		ChannelSubID: "gift-1", PurchaseType: models.PurchaseTypeGift, Days: 30, Traffic: 10 * GB})
	h.sync(t, user, face)

	// 撤销赠送 → 桶回 0
	ok, err := h.svc.RevokeEntry(ctx, user, face, "gift", "gift-1")
	if err != nil || !ok {
		t.Fatalf("revoke: ok=%v err=%v", ok, err)
	}
	pur := h.store.profile(user, face, models.EntitlementClassPurchase)
	if pur.DaysRemaining != 0 || pur.TrafficLimit != 0 {
		t.Fatalf("after revoke bucket must be 0/0, got %+v", pur)
	}
	// 再撤一次（幂等）/ 撤一个不存在的 → 不变、不为负
	ok, _ = h.svc.RevokeEntry(ctx, user, face, "gift", "gift-1")
	if ok {
		t.Fatalf("second revoke must be no-op")
	}
	pur = h.store.profile(user, face, models.EntitlementClassPurchase)
	if pur.DaysRemaining < 0 || pur.TrafficLimit < 0 {
		t.Fatalf("bucket must never go negative, got %+v", pur)
	}
	// 生效中的桶撤销部分：days 扣减且 expire 重算不早于 now
	h2 := newHarness(t, true)
	h2.addAccount("u6b", face, "uuid-b")
	h2.apply(t, oneTimeEntry("u6b", face, "credit", "c-1", 10, 10*GB))
	h2.apply(t, oneTimeEntry("u6b", face, "credit", "c-2", 30, 20*GB))
	h2.sync(t, "u6b", face)
	h2.clk.advance(days(20)) // 已用 20 天（40 天桶）
	h2.sync(t, "u6b", face)
	if _, err := h2.svc.RevokeEntry(ctx, "u6b", face, "credit", "chsub-c-2"); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	pur = h2.store.profile("u6b", face, models.EntitlementClassPurchase)
	if pur.DaysRemaining != 0 || pur.ExpireAt == nil || pur.ExpireAt.Before(h2.clk.now()) {
		t.Fatalf("revoking 30 of remaining 20 → remaining 0, expire clamped to now; got %+v", pur)
	}
}

// ==================== 7. 流量耗尽 + 桶有流量 → 不切换（非目标回归） ====================

func TestEntitlement_TrafficExhausted_NoSwitch(t *testing.T) {
	h := newHarness(t, true)
	const user, face = "u7", models.ServiceFaceStandard
	h.addAccount(user, face, "uuid-std")

	subEnd := h.clk.now().Add(days(20))
	h.apply(t, subEntry(user, face, "stripe", "sub-1", subEnd, 50*GB))
	h.apply(t, oneTimeEntry(user, face, "credit", "o-1", 30, 100*GB))
	h.sync(t, user, face)

	// 订阅流量用光（otun 会 disable），天数未到
	h.otun.used["uuid-std"] = 50 * GB
	h.clk.advance(days(5))
	res := h.sync(t, user, face)
	if res.ActiveClass != models.EntitlementClassSubscription {
		t.Fatalf("traffic exhaustion must NOT switch to purchase (time-only ruling), got %s", res.ActiveClass)
	}
	pur := res.Profiles[models.EntitlementClassPurchase]
	if pur.Status != models.ProfileStatusWaiting || pur.DaysRemaining != 30 || pur.TrafficUsed != 0 {
		t.Fatalf("bucket must stay untouched/waiting, got %+v", pur)
	}
	if p := h.otun.last(); p.TrafficLimit != 50*GB || !p.ExpireAt.Equal(subEnd) {
		t.Fatalf("otun must not receive bucket traffic while subscription is time-valid, got %+v", p)
	}
	// 契约 §3：profiles[].status 仍 active
	used := int64(50 * GB)
	proj, _ := h.svc.Project(context.Background(), user, face, &used)
	if proj.Profiles[0].Class != models.EntitlementClassSubscription || proj.Profiles[0].Status != models.ProfileStatusActive || proj.Profiles[0].TrafficUsed != 50*GB {
		t.Fatalf("subscription profile stays active with used=limit, got %+v", proj.Profiles[0])
	}
}

// ==================== 幂等 / 影子写 / trial ====================

// TestEntitlement_ShadowWrite_NoOtunPush：开关 false 时 ApplyEntry 落账、Sync 不推 otun 不动投影。
func TestEntitlement_ShadowWrite_NoOtunPush(t *testing.T) {
	h := newHarness(t, false)
	const user, face = "u8", models.ServiceFaceStandard
	vp := h.addAccount(user, face, "uuid-std")
	origExp := vp.ExpireAt

	h.apply(t, subEntry(user, face, "stripe", "sub-1", h.clk.now().Add(days(30)), 50*GB))
	h.apply(t, oneTimeEntry(user, face, "credit", "o-1", 30, 100*GB))
	h.sync(t, user, face)

	if len(h.otun.pushes) != 0 {
		t.Fatalf("switch off: Sync must not push to otun, got %d pushes", len(h.otun.pushes))
	}
	if vp.ExpireAt != origExp {
		t.Fatalf("switch off: projection row must be untouched")
	}
	if len(h.store.profiles) != 2 || len(h.store.entries) != 2 {
		t.Fatalf("switch off: shadow write must still record profiles/entries, got %d/%d", len(h.store.profiles), len(h.store.entries))
	}
}

// TestEntitlement_ResolveIdempotent：重复 Resolve/Sync 不改变状态、不重复推送。
func TestEntitlement_ResolveIdempotent(t *testing.T) {
	h := newHarness(t, true)
	const user, face = "u9", models.ServiceFaceStandard
	h.addAccount(user, face, "uuid-std")
	h.apply(t, oneTimeEntry(user, face, "credit", "o-1", 30, 100*GB))
	h.sync(t, user, face)
	n := len(h.otun.pushes)
	first := *h.store.profile(user, face, models.EntitlementClassPurchase)
	for i := 0; i < 3; i++ {
		h.sync(t, user, face)
	}
	if len(h.otun.pushes) != n {
		t.Fatalf("repeated Sync without change must not re-push, got %d → %d", n, len(h.otun.pushes))
	}
	again := *h.store.profile(user, face, models.EntitlementClassPurchase)
	if again.DaysRemaining != first.DaysRemaining || !again.ExpireAt.Equal(*first.ExpireAt) || again.Status != first.Status {
		t.Fatalf("repeated Resolve must be idempotent: %+v vs %+v", first, again)
	}
}

// TestEntitlement_TrialSupersededByPaid：trial profile 落账；付费到来作废 trial；无付费时 trial 生效。
func TestEntitlement_TrialSupersededByPaid(t *testing.T) {
	h := newHarness(t, true)
	const user, face = "u10", models.ServiceFaceStandard
	h.addAccount(user, face, "uuid-std")
	trialEnd := h.clk.now().Add(time.Hour)
	h.apply(t, &EntryInput{UserID: user, ServiceFace: face, SubscriptionID: "trial-1", Channel: "trial",
		BusinessType: models.BusinessTypeTrial, PeriodEnd: &trialEnd, Traffic: 1 * GB})
	res := h.sync(t, user, face)
	if res.ActiveClass != models.EntitlementClassTrial {
		t.Fatalf("only trial → active_class trial, got %s", res.ActiveClass)
	}
	h.apply(t, subEntry(user, face, "apple", "sub-1", h.clk.now().Add(days(30)), 50*GB))
	res = h.sync(t, user, face)
	if res.ActiveClass != models.EntitlementClassSubscription || h.store.profile(user, face, models.EntitlementClassTrial).Status != models.ProfileStatusExpired {
		t.Fatalf("paid must supersede trial, got %s / trial=%+v", res.ActiveClass, h.store.profile(user, face, models.EntitlementClassTrial))
	}
}

// TestEntitlement_NoneKeepsLastExpire：全部到期 → active_class none，投影 expire_at = 最后生效 profile 到期（不为 nil）。
func TestEntitlement_NoneKeepsLastExpire(t *testing.T) {
	h := newHarness(t, true)
	const user, face = "u11", models.ServiceFaceStandard
	h.addAccount(user, face, "uuid-std")
	subEnd := h.clk.now().Add(days(10))
	h.apply(t, subEntry(user, face, "stripe", "sub-1", subEnd, 50*GB))
	h.sync(t, user, face)
	h.clk.advance(days(11))
	res := h.sync(t, user, face)
	if res.ActiveClass != models.ActiveClassNone {
		t.Fatalf("expired subscription and no bucket → none, got %s", res.ActiveClass)
	}
	proj, _ := h.svc.Project(context.Background(), user, face, nil)
	if proj.ActiveClass != models.ActiveClassNone || proj.ExpireAt == nil || !proj.ExpireAt.Equal(subEnd) {
		t.Fatalf("none branch expire_at must be last profile expire %s, got %+v", subEnd, proj)
	}
	if proj.Profiles[0].Status != models.ProfileStatusExpired {
		t.Fatalf("subscription profile status want expired, got %s", proj.Profiles[0].Status)
	}
}

// TestClassifyEntry_KindMapping 契约 §4.3 映射 + 老上游缺 purchase_type 的推断。
func TestClassifyEntry_KindMapping(t *testing.T) {
	cases := []struct {
		ch, pt, bt, sub string
		class, kind     string
	}{
		{"apple", "subscription", "", "", "subscription", "apple"},
		{"google", "subscription", "", "", "subscription", "google"},
		{"stripe", "subscription", "", "", "subscription", "stripe"},
		{"stripe", "one_time", "", "", "purchase", "stripe_onetime"},
		{"credit", "one_time", "", "order-1", "purchase", "credit"},
		{"credit", "one_time", "", "campaign-claim-abc", "purchase", "campaign"},
		{"gift", "gift", "gift", "", "purchase", "gift"},
		{"gift_card", "one_time", "", "", "purchase", "gift_card"},
		{"manual", "one_time", "", "", "purchase", "manual"},
		{"trial", "trial", "trial", "", "trial", "trial"},
		// 老上游缺 purchase_type
		{"apple", "", "subscription", "", "subscription", "apple"},
		{"credit", "", "", "", "purchase", "credit"},
		{"gift", "", "gift", "", "purchase", "gift"},
	}
	for _, c := range cases {
		class, kind, _ := classifyEntry(c.ch, c.pt, c.bt, c.sub)
		if class != c.class || kind != c.kind {
			t.Errorf("classify(%s,%s,%s,%s) = %s/%s, want %s/%s", c.ch, c.pt, c.bt, c.sub, class, kind, c.class, c.kind)
		}
	}
}
