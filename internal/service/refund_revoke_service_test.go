package service

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/wenwu/saas-platform/fulfillment-service/internal/models"
	"github.com/wenwu/saas-platform/fulfillment-service/internal/repository"
)

type fakeRefundStore struct {
	mu   sync.Mutex
	recs map[string]*models.RefundRevocation
}

func (f *fakeRefundStore) Get(_ context.Context, ref string) (*models.RefundRevocation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r, ok := f.recs[ref]; ok {
		return r, nil
	}
	return nil, repository.ErrNotFound
}

func (f *fakeRefundStore) InsertIfAbsent(_ context.Context, v *models.RefundRevocation) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.recs[v.Ref]; ok {
		return false, nil
	}
	f.recs[v.Ref] = v
	return true, nil
}

const gib = int64(1024 * 1024 * 1024)

func paidRow(id, userID, uuid, face, tier string, exp time.Time, limit int64) *models.VPNProvision {
	return &models.VPNProvision{ID: id, UserID: userID, SubscriptionID: "sub-" + id, Channel: "stripe",
		ServiceTier: tier, ProductFace: face, OtunUUID: ptrStr(uuid), PlanTier: "basic",
		Status: models.VPNProvisionStatusActive, TrafficLimit: limit, ExpireAt: &exp, IsCurrent: true}
}

func newRefundSvc(t *testing.T, rows ...*models.VPNProvision) (*VPNService, *fakeOtun, *fakeRefundStore) {
	t.Helper()
	s, otun := newCampaignSvc(t, &fakeVPNStore{rows: rows}, nil)
	store := &fakeRefundStore{recs: map[string]*models.RefundRevocation{}}
	s.SetRefundRevocations(store)
	return s, otun, store
}

// 标准线:扣这笔订单的 30 天 / 50G(不整条禁用);同 ref 重复调用不再扣。
func TestRefundRevoke_ShortensStandardLine(t *testing.T) {
	ctx := context.Background()
	exp := time.Now().Add(60 * 24 * time.Hour).Truncate(time.Second)
	basic := paidRow("p-basic", "u1", "uuid-basic", models.ProductFaceBasic, models.ServiceTierStandard, exp, 100*gib)
	s, otun, _ := newRefundSvc(t, basic)

	res, err := s.RefundRevoke(ctx, &RefundRevokeRequest{UserID: "u1", Ref: "pay-1", PlanTier: "basic", Days: 30, TrafficBytes: 50 * gib})
	if err != nil {
		t.Fatal(err)
	}
	want := exp.AddDate(0, 0, -30)
	if res.Disabled || res.Noop || !res.NewExpireAt.Equal(want) || res.NewTrafficLimit != 50*gib {
		t.Fatalf("result = %+v", res)
	}
	if len(otun.puts) != 1 || otun.putUUID[0] != "uuid-basic" || otun.puts[0].ExpireAt != want.Format(time.RFC3339) ||
		otun.puts[0].TrafficLimit != 50*gib || otun.puts[0].Enabled != nil {
		t.Fatalf("otun PUT = %+v uuid=%v", otun.puts, otun.putUUID)
	}
	if !basic.ExpireAt.Equal(want) || basic.TrafficLimit != 50*gib || basic.Status != models.VPNProvisionStatusActive {
		t.Fatalf("row = expire %v limit %d status %s", basic.ExpireAt, basic.TrafficLimit, basic.Status)
	}
	again, err := s.RefundRevoke(ctx, &RefundRevokeRequest{UserID: "u1", Ref: "pay-1", PlanTier: "basic", Days: 30, TrafficBytes: 50 * gib})
	if err != nil || len(otun.puts) != 1 || !again.NewExpireAt.Equal(want) {
		t.Fatalf("replay must not deduct again: puts=%d err=%v", len(otun.puts), err)
	}
}

// 扣完已到期:到期设为现在并禁用(enabled=false)。
func TestRefundRevoke_DisablesWhenExhausted(t *testing.T) {
	ctx := context.Background()
	exp := time.Now().Add(10 * 24 * time.Hour)
	basic := paidRow("p-basic", "u2", "uuid-b2", models.ProductFaceBasic, models.ServiceTierStandard, exp, 10*gib)
	s, otun, _ := newRefundSvc(t, basic)
	res, err := s.RefundRevoke(ctx, &RefundRevokeRequest{UserID: "u2", Ref: "pay-2", PlanTier: "premium", Days: 30})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Disabled || otun.puts[0].Enabled == nil || *otun.puts[0].Enabled || basic.Status != models.VPNProvisionStatusDisabled {
		t.Fatalf("must disable: res=%+v put=%+v status=%s", res, otun.puts[0], basic.Status)
	}
	if otun.puts[0].TrafficLimit != 0 {
		t.Fatalf("traffic_bytes=0 must not touch the limit, got %d", otun.puts[0].TrafficLimit)
	}
	if res.NewExpireAt.After(time.Now().Add(time.Second)) {
		t.Fatalf("new expire must be now, got %v", res.NewExpireAt)
	}
}

// 住宅线订单只扣住宅线,标准线不动。
func TestRefundRevoke_ResidentialOnlyTouchesResidential(t *testing.T) {
	ctx := context.Background()
	exp := time.Now().Add(90 * 24 * time.Hour).Truncate(time.Second)
	basic := paidRow("p-b", "u3", "uuid-b3", models.ProductFaceBasic, models.ServiceTierStandard, exp, 100*gib)
	resi := paidRow("p-r", "u3", "uuid-r3", models.ProductFaceResidential, models.ServiceTierResidential, exp, 100*gib)
	s, otun, _ := newRefundSvc(t, basic, resi)
	if _, err := s.RefundRevoke(ctx, &RefundRevokeRequest{UserID: "u3", Ref: "pay-3", PlanTier: "residential", Days: 30}); err != nil {
		t.Fatal(err)
	}
	if len(otun.putUUID) != 1 || otun.putUUID[0] != "uuid-r3" || !basic.ExpireAt.Equal(exp) || !resi.ExpireAt.Equal(exp.AddDate(0, 0, -30)) {
		t.Fatalf("put uuids=%v basic=%v resi=%v", otun.putUUID, basic.ExpireAt, resi.ExpireAt)
	}
}

// otun 同步失败:整体失败,行不动、不写记录(可重试);无开通:只留 noop 记录;活动面拒绝。
func TestRefundRevoke_FailureNoopAndPromo(t *testing.T) {
	ctx := context.Background()
	exp := time.Now().Add(60 * 24 * time.Hour).Truncate(time.Second)
	basic := paidRow("p-b", "u4", "uuid-b4", models.ProductFaceBasic, models.ServiceTierStandard, exp, 100*gib)
	s, otun, store := newRefundSvc(t, basic)
	otun.putFail = 1
	if _, err := s.RefundRevoke(ctx, &RefundRevokeRequest{UserID: "u4", Ref: "pay-4", PlanTier: "basic", Days: 30}); err == nil {
		t.Fatal("otun failure must return an error")
	}
	if !basic.ExpireAt.Equal(exp) || len(store.recs) != 0 {
		t.Fatalf("row must be untouched and nothing recorded: expire=%v recs=%d", basic.ExpireAt, len(store.recs))
	}
	if res, err := s.RefundRevoke(ctx, &RefundRevokeRequest{UserID: "u4", Ref: "pay-4", PlanTier: "basic", Days: 30}); err != nil || res.Noop {
		t.Fatalf("retry should succeed: %+v %v", res, err)
	}
	res, err := s.RefundRevoke(ctx, &RefundRevokeRequest{UserID: "nobody", Ref: "pay-5", PlanTier: "basic", Days: 30})
	if err != nil || !res.Noop || store.recs["pay-5"] == nil {
		t.Fatalf("no provision → noop record: %+v %v", res, err)
	}
	if _, err := s.RefundRevoke(ctx, &RefundRevokeRequest{UserID: "u4", Ref: "pay-6", PlanTier: "promo", Days: 7}); err == nil {
		t.Fatal("promo line must be rejected")
	}
}
