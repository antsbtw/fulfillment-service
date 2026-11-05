package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wenwu/saas-platform/fulfillment-service/internal/models"
	"github.com/wenwu/saas-platform/fulfillment-service/internal/repository"
)

// 订阅换绑（同一笔 Apple 交易换了登录账号）的回归用例，锁定 2026-10-18 / 2026-11-05 x26262468 两案：
//   - 新账号要拿到自己的线路（不能因为按订阅号查到旧账号那条就短路返回）；
//   - 同一期重新开通沿用原到期和剩余流量（不能按 now+30d、满额重新给一期）；
//   - 换绑回收只停这笔订阅的那条记录（不能停掉对方账号自己的 trial）。

func newLegacyReassignStack(t *testing.T) (*VPNService, *otunStub, *fakeVPNStore, func()) {
	t.Helper()
	svc, stub, _, _, done := newSwitchOnStack(t)
	svc.cfg.Entitlement.Enabled = false // 生产现状：旧路径 + 影子写
	svc.entitlement.enabled = false
	return svc, stub, svc.vpnRepo.(*fakeVPNStore), done
}

func appleResidentialReq(user string, start, end time.Time) *models.ProvisionRequest {
	return &models.ProvisionRequest{
		AppSource: "otun", BusinessType: "subscription", Channel: "apple", SubscriptionID: "sub-x",
		UserID: user, UserEmail: user + "@example.test", PlanTier: "residential", PurchaseType: "subscription",
		ChannelSubID: "530003157613675", PeriodStart: &start, PeriodEnd: &end, ExpireDays: 30,
	}
}

func lastRowOf(store *fakeVPNStore, user string) *models.VPNProvision {
	for i := len(store.rows) - 1; i >= 0; i-- {
		if store.rows[i].UserID == user {
			return store.rows[i]
		}
	}
	return nil
}

// 11-05 16:25 形态：换绑的开通先于旧账号回收跑完（旧账号那条仍 active/current）。
func TestLegacy_Reassign_NewUserGetsOwnLineAndInheritsPeriod(t *testing.T) {
	svc, stub, store, done := newLegacyReassignStack(t)
	defer done()
	ctx := context.Background()

	now := time.Now().UTC().Truncate(time.Second)
	pStart, pEnd := now.Add(-21*24*time.Hour), now.Add(9*24*time.Hour)

	r1, err := svc.ProvisionVPNUser(ctx, appleResidentialReq("u-gmail", pStart, pEnd))
	if err != nil {
		t.Fatalf("initial provision: %v", err)
	}
	old := lastRowOf(store, "u-gmail")
	// 模拟开通已过 21 天：原到期 = 现在 + 12 天（早于 now+30d）
	old.ExpireAt = ptrTime(now.Add(12 * 24 * time.Hour))
	origExpire, origLimit := *old.ExpireAt, old.TrafficLimit
	stub.setUsed(r1.VPNUserID, 36*GB)

	r2, err := svc.ProvisionVPNUser(ctx, appleResidentialReq("u-icloud", pStart, pEnd))
	if err != nil {
		t.Fatalf("reassign provision: %v", err)
	}
	if r2.Message == "Already provisioned (idempotent)" || r2.VPNUserID == r1.VPNUserID {
		t.Fatalf("new account must get its own line, got %+v (old uuid %s)", r2, r1.VPNUserID)
	}
	nw := lastRowOf(store, "u-icloud")
	if nw == nil || nw.ID != r2.ResourceID || nw.Status != models.VPNProvisionStatusActive || !nw.IsCurrent {
		t.Fatalf("new account must have an active current provision, got %+v", nw)
	}
	if !nw.ExpireAt.Equal(origExpire) {
		t.Fatalf("reassign must keep this period's expiry %s, got %s (would extend the paid period)", origExpire, *nw.ExpireAt)
	}
	if want := origLimit - 36*GB; nw.TrafficLimit != want {
		t.Fatalf("reassign must carry remaining traffic %d, got %d (would reset the quota)", want, nw.TrafficLimit)
	}

	// 旧账号回收：只按这笔订阅回收
	if err := svc.DeprovisionVPNByUser(ctx, "u-gmail", "subscription reassigned to u-icloud", ptrBool(true), "sub-x"); err != nil {
		t.Fatalf("deprovision old: %v", err)
	}
	if old.Status != models.VPNProvisionStatusDisabled || old.IsCurrent {
		t.Fatalf("old account's line must be disabled, got status=%s current=%v", old.Status, old.IsCurrent)
	}
	if nw.Status != models.VPNProvisionStatusActive {
		t.Fatalf("deprovisioning the old account must not touch the new account's line")
	}
}

// 11-05 17:50 形态：换走后又换回同一账号（旧记录已停用、非 current）——不得延长、不得清零已用。
func TestLegacy_ReassignBack_SameUserKeepsExpiryAndUsed(t *testing.T) {
	svc, stub, store, done := newLegacyReassignStack(t)
	defer done()
	ctx := context.Background()

	now := time.Now().UTC().Truncate(time.Second)
	pStart, pEnd := now.Add(-21*24*time.Hour), now.Add(9*24*time.Hour)

	if _, err := svc.ProvisionVPNUser(ctx, appleResidentialReq("u-gmail", pStart, pEnd)); err != nil {
		t.Fatalf("initial provision: %v", err)
	}
	old := lastRowOf(store, "u-gmail")
	old.ExpireAt = ptrTime(now.Add(12 * 24 * time.Hour))
	origExpire := *old.ExpireAt
	// 换走：本账号这条被回收
	old.Status, old.IsCurrent = models.VPNProvisionStatusDisabled, false

	postsBefore := len(stub.posts)
	if _, err := svc.ProvisionVPNUser(ctx, appleResidentialReq("u-gmail", pStart, pEnd)); err != nil {
		t.Fatalf("reassign back: %v", err)
	}
	back := lastRowOf(store, "u-gmail")
	if back == old || !back.IsCurrent || back.Status != models.VPNProvisionStatusActive {
		t.Fatalf("reassign back must create an active current provision, got %+v", back)
	}
	if !back.ExpireAt.Equal(origExpire) {
		t.Fatalf("reassign back must keep expiry %s, got %s", origExpire, *back.ExpireAt)
	}
	if len(stub.posts) == postsBefore {
		t.Fatalf("residential re-provision must upsert otun")
	}
	last := stub.posts[len(stub.posts)-1]
	if p, _ := last["preserve_traffic_used"].(bool); !p {
		t.Fatalf("same-account re-provision of the same period must preserve used traffic, body=%v", last)
	}
	if got, _ := time.Parse(time.RFC3339, last["expire_at"].(string)); !got.Equal(origExpire) {
		t.Fatalf("otun expire_at must be the inherited expiry %s, got %v", origExpire, last["expire_at"])
	}
}

// 真续费（新 period_end）不受影响：照旧给新一期。
func TestLegacy_Reassign_NewPeriodIsNotInherited(t *testing.T) {
	svc, _, store, done := newLegacyReassignStack(t)
	defer done()
	ctx := context.Background()

	now := time.Now().UTC().Truncate(time.Second)
	p1Start, p1End := now.Add(-29*24*time.Hour), now.Add(1*24*time.Hour)
	if _, err := svc.ProvisionVPNUser(ctx, appleResidentialReq("u-gmail", p1Start, p1End)); err != nil {
		t.Fatalf("initial provision: %v", err)
	}
	old := lastRowOf(store, "u-gmail")
	old.ExpireAt = ptrTime(now.Add(1 * 24 * time.Hour))
	old.Status, old.IsCurrent = models.VPNProvisionStatusDisabled, false

	p2Start, p2End := p1End, p1End.Add(30*24*time.Hour)
	if _, err := svc.ProvisionVPNUser(ctx, appleResidentialReq("u-icloud", p2Start, p2End)); err != nil {
		t.Fatalf("new period provision: %v", err)
	}
	nw := lastRowOf(store, "u-icloud")
	if !nw.ExpireAt.After(now.Add(25 * 24 * time.Hour)) {
		t.Fatalf("a new paid period must get a fresh expiry, got %s", *nw.ExpireAt)
	}
}

// 11-05 17:50：换回 gmail 时，按"icloud 该面当前记录"回收，停掉了 icloud 自己的 trial。
func TestDeprovisionByUser_OnlyTouchesThatSubscription(t *testing.T) {
	svc, _, store, done := newLegacyReassignStack(t)
	defer done()
	ctx := context.Background()

	trialUUID := "uuid-trial"
	exp := time.Now().Add(24 * time.Hour)
	trial := &models.VPNProvision{
		ID: "prov-trial", UserID: "u-icloud", SubscriptionID: "sub-trial", Channel: "trial",
		BusinessType: models.BusinessTypeTrial, ServiceTier: models.ServiceTierResidential, PlanTier: "residential",
		OtunUUID: &trialUUID, Status: models.VPNProvisionStatusActive, IsCurrent: true, ExpireAt: &exp,
		TrafficLimit: 10 * GB,
	}
	store.rows = append(store.rows, trial)

	err := svc.DeprovisionVPNByUser(ctx, "u-icloud", "subscription reassigned to u-gmail", ptrBool(true), "sub-x")
	if !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("a provision of another subscription must be left alone (ErrNotFound), got %v", err)
	}
	if trial.Status != models.VPNProvisionStatusActive || !trial.IsCurrent {
		t.Fatalf("trial must stay active, got status=%s current=%v", trial.Status, trial.IsCurrent)
	}
}

func ptrBool(b bool) *bool { return &b }
