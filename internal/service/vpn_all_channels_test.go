package service

import (
	"context"
	"errors"
	"testing"

	"github.com/wenwu/saas-platform/fulfillment-service/internal/models"
	"github.com/wenwu/saas-platform/fulfillment-service/internal/repository"
)

// residentialRowNoUUID 模拟住宅面 current provision，OtunUUID 留空以避免触达 otunClient
// （buildQuickStatus 仅当 OtunUUID 非空才 SyncUser），从而在无 HTTP/无 mock 下断言分区扇出。
func residentialRowNoUUID(userID string) *models.VPNProvision {
	return &models.VPNProvision{
		ID:          "prov-res",
		UserID:      userID,
		ServiceTier: models.ServiceTierResidential,
		PlanTier:    "residential",
		Status:      models.VPNProvisionStatusActive,
		IsCurrent:   true,
	}
}

func standardRowNoUUID(userID string) *models.VPNProvision {
	return &models.VPNProvision{
		ID:          "prov-std",
		UserID:      userID,
		ServiceTier: models.ServiceTierStandard,
		PlanTier:    "basic",
		Status:      models.VPNProvisionStatusActive,
		IsCurrent:   true,
	}
}

// TestQuickStatusAll_BothFaces：持两面 → 返回两条，标准面与住宅面各一。
func TestQuickStatusAll_BothFaces(t *testing.T) {
	const userID = "u-both"
	store := &fakeVPNStore{rows: []*models.VPNProvision{
		standardRowNoUUID(userID),
		residentialRowNoUUID(userID),
	}}
	s := newSvc(true, store) // MULTI_SERVICE_ENABLED=true，真正按面分区

	out, err := s.GetUserVPNQuickStatusAll(context.Background(), userID)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("want 2 faces, got %d", len(out))
	}
	tiers := map[string]bool{}
	for _, q := range out {
		tiers[q.PlanTier] = true
	}
	if !tiers["basic"] || !tiers["residential"] {
		t.Fatalf("want basic+residential, got %+v", tiers)
	}
}

// TestQuickStatusAll_StandardOnly：只持标准面 → 仅一条，住宅面不进数组。
func TestQuickStatusAll_StandardOnly(t *testing.T) {
	const userID = "u-std"
	store := &fakeVPNStore{rows: []*models.VPNProvision{standardRowNoUUID(userID)}}
	s := newSvc(true, store)

	out, err := s.GetUserVPNQuickStatusAll(context.Background(), userID)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(out) != 1 || out[0].PlanTier != "basic" {
		t.Fatalf("want 1 standard face, got %+v", out)
	}
}

// TestQuickStatusAll_None：无任何 current provision → 空数组（非 nil、非错误）。
func TestQuickStatusAll_None(t *testing.T) {
	store := &fakeVPNStore{}
	s := newSvc(true, store)

	out, err := s.GetUserVPNQuickStatusAll(context.Background(), "nobody")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if out == nil || len(out) != 0 {
		t.Fatalf("want empty non-nil slice, got %#v", out)
	}
}

// TestResolveResidentialOtunUUID_PrefersResidentialOverNewerStandard 复现并锁定
// no_assignment 误报的修复：持双面用户，标准面 provision 更晚创建（如先订 residential
// 再买 basic）。realm/region 必须解析到【住宅面】uuid（有 realm assignment），而不是
// GetOtunUUIDByUser 取到的最新标准 uuid（无 assignment → select 报 no_assignment）。
func TestResolveResidentialOtunUUID_PrefersResidentialOverNewerStandard(t *testing.T) {
	const userID, resUUID, stdUUID = "u-both", "uuid-residential", "uuid-standard"
	res := &models.VPNProvision{
		ID: "p-res", UserID: userID, ServiceTier: models.ServiceTierResidential,
		PlanTier: "residential", OtunUUID: ptrStr(resUUID), IsCurrent: true,
	}
	std := &models.VPNProvision{
		ID: "p-std", UserID: userID, ServiceTier: models.ServiceTierStandard,
		PlanTier: "basic", OtunUUID: ptrStr(stdUUID), IsCurrent: true,
	}
	// std 在 res 之后入列，模拟 created_at 更晚（GetOtunUUIDByUser 会错误地取它）。
	store := &fakeVPNStore{rows: []*models.VPNProvision{res, std}}
	s := newSvc(true, store)

	// ★P2 后：缺省面（不传二元组）必须仍解析到【付费住宅】uuid——这条锁住"付费面零改动"。
	got, err := s.resolveRealmOtunUUID(context.Background(), userID, RealmFace{})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got == nil || *got != resUUID {
		t.Fatalf("want residential uuid %q (with assignment), got %v", resUUID, got)
	}
}

// TestResolveResidentialOtunUUID_StandardOnlyHasNoResidential：只持标准面 → 不会误取标准 uuid。
// ★P2 后语义微调：新解析器对"无住宅面"直接返回 ErrNoRealmAssignment（而非 nil,nil），
// 上层两种都映射成 404 no_assignment，对外行为不变。
func TestResolveResidentialOtunUUID_StandardOnlyHasNoResidential(t *testing.T) {
	const userID = "u-std-only"
	store := &fakeVPNStore{rows: []*models.VPNProvision{
		{ID: "p-std", UserID: userID, ServiceTier: models.ServiceTierStandard,
			PlanTier: "basic", OtunUUID: ptrStr("uuid-standard"), IsCurrent: true},
	}}
	s := newSvc(true, store)

	got, err := s.resolveRealmOtunUUID(context.Background(), userID, RealmFace{})
	if got != nil {
		t.Fatalf("want no residential face, got %v", *got)
	}
	if err != nil && !errors.Is(err, ErrNoRealmAssignment) {
		t.Fatalf("want nil or ErrNoRealmAssignment, got %v", err)
	}
}

// errVPNStore 让按面查询返回指定错误(模拟查库失败 / 未找到)。
type errVPNStore struct {
	*fakeVPNStore
	err error
}

func (f *errVPNStore) GetCurrentByUserAndServicePartition(context.Context, string, bool) (*models.VPNProvision, error) {
	return nil, f.err
}

// 定稿 BACKEND_REPLY_2026-11-19_CONNECT_TICKET_TOKEN_FINAL §2.2:查库出错必须报错(→5xx),
// 不能吞成"没有这一面"——客户端会把「200 且无住宅条目」当成无权益而停住宅线。
func TestQuickStatusAll_DBErrorIsError(t *testing.T) {
	s := newSvc(true, &errVPNStore{fakeVPNStore: &fakeVPNStore{}, err: errors.New("conn reset")})
	if out, err := s.GetUserVPNQuickStatusAll(context.Background(), "u1"); err == nil {
		t.Fatalf("db error must surface, got %+v", out)
	}
}

// 未找到(repository.ErrNotFound,真实仓库对无行的返回)仍是"没有这一面",200 空数组。
func TestQuickStatusAll_NotFoundIsEmpty(t *testing.T) {
	s := newSvc(true, &errVPNStore{fakeVPNStore: &fakeVPNStore{}, err: repository.ErrNotFound})
	out, err := s.GetUserVPNQuickStatusAll(context.Background(), "u1")
	if err != nil || out == nil || len(out) != 0 {
		t.Fatalf("not found must be empty slice, got %#v %v", out, err)
	}
}
