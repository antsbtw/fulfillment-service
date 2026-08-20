package service

import (
	"context"
	"testing"

	"github.com/wenwu/saas-platform/fulfillment-service/internal/config"
	"github.com/wenwu/saas-platform/fulfillment-service/internal/models"
)

// fakeVPNStore 是 vpnProvisionStore 的内存实现，模拟 fulfillment.vpn_provisions 表，
// 用于在无 DB / 无 mock 框架的前提下断言 MULTI_SERVICE 开关下的并行/互斥 provision 语义。
// 只实现 resolveExisting* 用到的查询；其余方法记账或空实现（本测试不触达 otun/HTTP 路径）。
type fakeVPNStore struct {
	rows []*models.VPNProvision
}

// faceOf 模拟迁移 010 的分区键：优先行上的 ProductFace，空则按 (plan_tier, service_tier) 推导
//（与回填口径一致）。老测试构造的行没填 ProductFace，仍按 service_tier 落到 basic/residential。
func faceOf(r *models.VPNProvision) string { return r.EffectiveProductFace() }

// GetCurrentByUserAndFace / GetOtunUUIDByUserAndFace / GetBySubscriptionIDAndFace：按产品面分区。
func (f *fakeVPNStore) GetCurrentByUserAndFace(_ context.Context, userID, face string) (*models.VPNProvision, error) {
	for i := len(f.rows) - 1; i >= 0; i-- {
		r := f.rows[i]
		if r.UserID == userID && r.IsCurrent && faceOf(r) == face {
			return r, nil
		}
	}
	return nil, nil
}

// GetCurrentByUserFaceAndTier 在面内再按线路细分（镜像 SQL 的 COALESCE(service_tier,'standard')）。
func (f *fakeVPNStore) GetCurrentByUserFaceAndTier(_ context.Context, userID, face, serviceTier string) (*models.VPNProvision, error) {
	for i := len(f.rows) - 1; i >= 0; i-- {
		r := f.rows[i]
		tier := r.ServiceTier
		if tier == "" {
			tier = models.ServiceTierStandard
		}
		if r.UserID == userID && r.IsCurrent && faceOf(r) == face && tier == serviceTier {
			return r, nil
		}
	}
	return nil, nil
}

func (f *fakeVPNStore) GetOtunUUIDByUserAndFace(_ context.Context, userID, face string) (*string, error) {
	for i := len(f.rows) - 1; i >= 0; i-- {
		r := f.rows[i]
		if r.UserID == userID && r.OtunUUID != nil && *r.OtunUUID != "" && faceOf(r) == face {
			return r.OtunUUID, nil
		}
	}
	return nil, nil
}

func (f *fakeVPNStore) GetBySubscriptionIDAndFace(_ context.Context, subID, face string) (*models.VPNProvision, error) {
	for i := len(f.rows) - 1; i >= 0; i-- {
		r := f.rows[i]
		if r.SubscriptionID == subID && r.IsCurrent && faceOf(r) == face {
			return r, nil
		}
	}
	return nil, nil
}

// 原（非分区）查询：取该 user 任意 service_tier 的 current 记录（最后一条优先，模拟 created_at DESC）。
func (f *fakeVPNStore) GetCurrentByUserAnyStatus(_ context.Context, userID string) (*models.VPNProvision, error) {
	for i := len(f.rows) - 1; i >= 0; i-- {
		r := f.rows[i]
		if r.UserID == userID && r.IsCurrent && faceOf(r) != models.ProductFaceCampaign {
			return r, nil
		}
	}
	return nil, nil
}

func (f *fakeVPNStore) GetBySubscriptionID(_ context.Context, subID string) (*models.VPNProvision, error) {
	for i := len(f.rows) - 1; i >= 0; i-- {
		r := f.rows[i]
		if r.SubscriptionID == subID && r.IsCurrent && faceOf(r) != models.ProductFaceCampaign {
			return r, nil
		}
	}
	return nil, nil
}

func (f *fakeVPNStore) GetOtunUUIDByUser(_ context.Context, userID string) (*string, error) {
	for i := len(f.rows) - 1; i >= 0; i-- {
		r := f.rows[i]
		if r.UserID == userID && r.OtunUUID != nil && *r.OtunUUID != "" && faceOf(r) != models.ProductFaceCampaign {
			return r.OtunUUID, nil
		}
	}
	return nil, nil
}

// 分区查询：在原条件上叠加 (service_tier='residential')==isResidential。
func (f *fakeVPNStore) GetCurrentByUserAndServicePartition(ctx context.Context, userID string, isResidential bool) (*models.VPNProvision, error) {
	return f.GetCurrentByUserAndFace(ctx, userID, models.PartitionFace(isResidential))
}

func (f *fakeVPNStore) GetBySubscriptionIDAndServicePartition(ctx context.Context, subID string, isResidential bool) (*models.VPNProvision, error) {
	return f.GetBySubscriptionIDAndFace(ctx, subID, models.PartitionFace(isResidential))
}

func (f *fakeVPNStore) GetOtunUUIDByUserAndServicePartition(ctx context.Context, userID string, isResidential bool) (*string, error) {
	return f.GetOtunUUIDByUserAndFace(ctx, userID, models.PartitionFace(isResidential))
}

// GetCurrentByUser：不分面取最新 active current（模拟 created_at DESC；供单条 /status、/vpn 测试）。
func (f *fakeVPNStore) GetCurrentByUser(_ context.Context, userID string) (*models.VPNProvision, error) {
	for i := len(f.rows) - 1; i >= 0; i-- {
		r := f.rows[i]
		if r.UserID == userID && r.IsCurrent && r.Status == models.VPNProvisionStatusActive && faceOf(r) != models.ProductFaceCampaign {
			return r, nil
		}
	}
	return nil, nil
}

// 未被本测试触达的方法（resolveExisting* 不调用它们）。
func (f *fakeVPNStore) GetByID(_ context.Context, id string) (*models.VPNProvision, error) {
	for _, r := range f.rows {
		if r.ID == id {
			return r, nil
		}
	}
	return nil, nil
}
func (f *fakeVPNStore) Create(_ context.Context, vp *models.VPNProvision) error {
	f.rows = append(f.rows, vp)
	return nil
}
func (f *fakeVPNStore) Update(_ context.Context, _ *models.VPNProvision) error          { return nil }
func (f *fakeVPNStore) MarkNotCurrent(_ context.Context, _ string) error                { return nil }
func (f *fakeVPNStore) UpdateEmailByUserID(_ context.Context, _, _ string) error        { return nil }

func ptrStr(s string) *string { return &s }

func newSvc(multiEnabled bool, store vpnProvisionStore) *VPNService {
	return &VPNService{
		cfg:     &config.Config{MultiService: config.MultiServiceConfig{Enabled: multiEnabled}},
		vpnRepo: store,
	}
}

// standardRow 模拟一个 basic 用户已有的 standard provision（service_tier=standard）。
func standardRow(userID, subID, uuid string) *models.VPNProvision {
	return &models.VPNProvision{
		ID:             "prov-std",
		UserID:         userID,
		SubscriptionID: subID,
		ServiceTier:    models.ServiceTierStandard,
		PlanTier:       "basic",
		Status:         models.VPNProvisionStatusActive,
		OtunUUID:       ptrStr(uuid),
		IsCurrent:      true,
	}
}

// TestResolve_SwitchOff_BasicResidentialMutexOverwrite 证明开关 false 时零回归：
// 已有 basic(standard) 记录的 user 来 residential 请求，user 级查询仍命中那条 standard 记录
// （→ ProvisionVPNUser 会走 renewal 分支把它覆盖成 residential，即现状互斥行为），
// 且 otun_uuid 仍复用 standard 的旧 uuid（现状）。
func TestResolve_SwitchOff_BasicResidentialMutexOverwrite(t *testing.T) {
	const userID, stdUUID = "u1", "uuid-standard"
	store := &fakeVPNStore{rows: []*models.VPNProvision{standardRow(userID, "sub-basic", stdUUID)}}
	s := newSvc(false, store)
	ctx := context.Background()

	// residential 请求（isResidential=true）在开关 false 下仍命中 standard 记录 → 会被覆盖。
	existing, _ := s.resolveExistingCurrent(ctx, userID, true)
	if existing == nil {
		t.Fatalf("switch off: residential request should still hit the existing standard record (mutex overwrite), got nil")
	}
	if existing.ServiceTier != models.ServiceTierStandard {
		t.Fatalf("switch off: expected to hit standard record, got tier=%s", existing.ServiceTier)
	}

	// otun_uuid 复用 standard 的旧 uuid（现状：trial/standard→... 复用）。
	uuid, _ := s.resolveExistingOtunUUID(ctx, userID, true)
	if uuid == nil || *uuid != stdUUID {
		t.Fatalf("switch off: residential request should reuse standard otun_uuid %q, got %v", stdUUID, uuid)
	}
}

// TestResolve_SwitchOn_ParallelNoOverwrite 证明开关 true 时并行不覆盖：
// 已有 basic(standard) 记录的 user 来 residential 请求时，分区查询【取不到】那条 standard 记录
// （→ ProvisionVPNUser 走全新 create 分支，新增一条 residential 记录，与 standard 并存，basic 不被覆盖），
// 且 residential 不复用 standard 的 otun_uuid（→ CreateUser 新建独立 UUID）。
// 反向：standard 请求仍只看到 standard 记录（正常续期，不被 residential 干扰）。
func TestResolve_SwitchOn_ParallelNoOverwrite(t *testing.T) {
	const userID, stdUUID = "u1", "uuid-standard"
	store := &fakeVPNStore{rows: []*models.VPNProvision{standardRow(userID, "sub-basic", stdUUID)}}
	s := newSvc(true, store)
	ctx := context.Background()

	// residential 请求 → 不命中 standard 记录 → 不覆盖 basic。
	existing, _ := s.resolveExistingCurrent(ctx, userID, true)
	if existing != nil {
		t.Fatalf("switch on: residential request must NOT see the standard record (no overwrite), got %+v", existing)
	}

	// residential 请求 → 不复用 standard 的 otun_uuid → 走 CreateUser 新建独立 UUID。
	uuid, _ := s.resolveExistingOtunUUID(ctx, userID, true)
	if uuid != nil {
		t.Fatalf("switch on: residential must NOT reuse standard otun_uuid, got %v", uuid)
	}

	// 反向：standard 请求仍命中自己的 standard 记录（不受 residential 影响）。
	stdExisting, _ := s.resolveExistingCurrent(ctx, userID, false)
	if stdExisting == nil || stdExisting.ServiceTier != models.ServiceTierStandard {
		t.Fatalf("switch on: standard request should still hit its own standard record, got %+v", stdExisting)
	}

	// 模拟 residential provision 落库后，两条 current 记录并存且 basic 仍是 standard 未被改。
	store.rows = append(store.rows, &models.VPNProvision{
		ID: "prov-res", UserID: userID, SubscriptionID: "sub-residential",
		ServiceTier: models.ServiceTierResidential, PlanTier: "residential",
		Status: models.VPNProvisionStatusActive, OtunUUID: ptrStr("uuid-residential"), IsCurrent: true,
	})

	currentCount := 0
	var stdTier string
	for _, r := range store.rows {
		if r.UserID == userID && r.IsCurrent {
			currentCount++
			if r.ServiceTier == models.ServiceTierStandard {
				stdTier = r.ServiceTier
			}
		}
	}
	if currentCount != 2 {
		t.Fatalf("switch on: expected 2 coexisting current provisions (standard + residential), got %d", currentCount)
	}
	if stdTier != models.ServiceTierStandard {
		t.Fatalf("switch on: standard record's service_tier must remain 'standard' (not overwritten), got %q", stdTier)
	}

	// 分区取回两条各自的 otun_uuid，互不串。
	resUUID, _ := s.resolveExistingOtunUUID(ctx, userID, true)
	if resUUID == nil || *resUUID != "uuid-residential" {
		t.Fatalf("switch on: residential partition should resolve residential uuid, got %v", resUUID)
	}
	gotStdUUID, _ := s.resolveExistingOtunUUID(ctx, userID, false)
	if gotStdUUID == nil || *gotStdUUID != stdUUID {
		t.Fatalf("switch on: standard partition should still resolve standard uuid %q, got %v", stdUUID, gotStdUUID)
	}
}

// TestResolve_SubIDIdempotency_Partition 证明幂等短路查询也按分区分流（开关 true）。
func TestResolve_SubIDIdempotency_Partition(t *testing.T) {
	const userID = "u1"
	store := &fakeVPNStore{rows: []*models.VPNProvision{standardRow(userID, "sub-shared", "uuid-standard")}}
	ctx := context.Background()

	// 开关 false：按 subID 命中（不分 tier）。
	off := newSvc(false, store)
	if got, _ := off.resolveExistingBySubID(ctx, "sub-shared", true); got == nil {
		t.Fatalf("switch off: GetBySubscriptionID should hit regardless of tier")
	}

	// 开关 true：residential 请求查同一 subID 但分区为 residential → 不命中那条 standard 记录。
	on := newSvc(true, store)
	if got, _ := on.resolveExistingBySubID(ctx, "sub-shared", true); got != nil {
		t.Fatalf("switch on: residential partition must not idempotently short-circuit on a standard subID record, got %+v", got)
	}
	// standard 请求查同一 subID（分区 standard）→ 命中。
	if got, _ := on.resolveExistingBySubID(ctx, "sub-shared", false); got == nil {
		t.Fatalf("switch on: standard partition should hit its own subID record")
	}
}
