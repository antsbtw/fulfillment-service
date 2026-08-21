package service

import (
	"context"
	"errors"
	"testing"

	"github.com/wenwu/saas-platform/fulfillment-service/internal/models"
)

// 本文件钉死 iOS 前端 2026-08-21 提出的红线 R1/R2/R5（P2 修复的验收项）：
//
//	R1 活动面的任何操作，不得改动付费面的任何服务端状态
//	R2 活动面的任何操作，不得消耗付费面的配额/限流额度
//	R5 活动面不存在或异常时，付费面行为与活动功能上线前一致
//
// 服务端层面，R1/R2 归结为同一个不变量：**从活动面发起的 realm 操作，
// 解析出的 otun_uuid 必须是活动面那个账号，绝不能是付费住宅账号**。
// （限流计数存在 realm_user_assignment，键为 user_uuid ⇒ 只要 uuid 不串，计数天然不串。）

// dualFaceStore 造一个同时持有【付费住宅】与【活动住宅】两个面的用户。
func dualFaceStore(userID, paidUUID, promoUUID string) *fakeVPNStore {
	paid := &models.VPNProvision{
		ID: "p-paid-resi", UserID: userID,
		ServiceTier: models.ServiceTierResidential, PlanTier: "residential",
		ProductFace: models.ProductFaceResidential,
		OtunUUID:    ptrStr(paidUUID), IsCurrent: true, TrafficLimit: 100 << 30,
	}
	// 活动行后入列（created_at 更晚），确保"取最新"的实现会踩错。
	promo := &models.VPNProvision{
		ID: "p-promo-resi", UserID: userID,
		ServiceTier: models.ServiceTierResidential, PlanTier: models.PlanTierCampaign,
		ProductFace: models.ProductFaceCampaign,
		OtunUUID:    ptrStr(promoUUID), IsCurrent: true, TrafficLimit: 10 << 30,
	}
	return &fakeVPNStore{rows: []*models.VPNProvision{paid, promo}}
}

// ★R1/R2：从活动面发起 → 必须解析到活动面账号，绝不能碰付费住宅账号。
// 这是修复前的真实事故路径：用户在活动面点"切 US"，切走的是他付费买的订阅出口。
func TestRealmFace_CampaignNeverResolvesToPaidResidential(t *testing.T) {
	const (
		userID    = "u-dual"
		paidUUID  = "uuid-paid-residential"
		promoUUID = "uuid-promo-residential"
	)
	s := newSvc(true, dualFaceStore(userID, paidUUID, promoUUID))

	got, err := s.resolveRealmOtunUUID(context.Background(), userID, RealmFace{
		PlanTier: models.PlanTierCampaign, ServiceTier: models.ServiceTierResidential,
	})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got == nil {
		t.Fatal("★活动面解析不到 uuid")
	}
	if *got == paidUUID {
		t.Fatalf("★★违反红线 R1/R2：活动面操作解析到了【付费住宅账号】%q——"+
			"这会切走用户花钱买的出口、并消耗其 24h 限流额度", paidUUID)
	}
	if *got != promoUUID {
		t.Fatalf("want promo uuid %q, got %q", promoUUID, *got)
	}
}

// ★R5：不传面参数（老客户端 / 所有付费面调用方）→ 行为与修复前逐字节一致，
// 即便该用户同时持有活动面，也必须解析到付费住宅账号。
func TestRealmFace_DefaultStillResolvesPaidResidential(t *testing.T) {
	const (
		userID    = "u-dual2"
		paidUUID  = "uuid-paid-residential"
		promoUUID = "uuid-promo-residential"
	)
	s := newSvc(true, dualFaceStore(userID, paidUUID, promoUUID))

	// 三种"缺省"写法都必须落到付费住宅面。
	for _, face := range []RealmFace{
		{},                        // 完全不传
		{PlanTier: "residential"}, // 只传 plan_tier
		ResidentialFace(),         // 显式付费住宅
	} {
		got, err := s.resolveRealmOtunUUID(context.Background(), userID, face)
		if err != nil {
			t.Fatalf("face=%+v unexpected err: %v", face, err)
		}
		if got == nil || *got != paidUUID {
			t.Fatalf("★违反红线 R5：face=%+v 应解析到付费住宅 %q，实际 %v", face, paidUUID, got)
		}
	}
}

// 活动·标准线路没有 realm 分配 → 必须 ErrNoRealmAssignment，
// 不得把标准面 uuid 送进 realm 接口（否则 manager 侧 no_assignment 或串号）。
func TestRealmFace_CampaignStandardHasNoRealm(t *testing.T) {
	const userID = "u-promo-std"
	store := &fakeVPNStore{rows: []*models.VPNProvision{{
		ID: "p-promo-std", UserID: userID,
		ServiceTier: models.ServiceTierStandard, PlanTier: models.PlanTierCampaign,
		ProductFace: models.ProductFaceCampaign,
		OtunUUID:    ptrStr("uuid-promo-standard"), IsCurrent: true,
	}}}
	s := newSvc(true, store)

	got, err := s.resolveRealmOtunUUID(context.Background(), userID, RealmFace{
		PlanTier: models.PlanTierCampaign, ServiceTier: models.ServiceTierStandard,
	})
	if got != nil {
		t.Fatalf("★标准线路不该有 realm uuid，得到 %q", *got)
	}
	if !errors.Is(err, ErrNoRealmAssignment) {
		t.Fatalf("want ErrNoRealmAssignment, got %v", err)
	}
}

// 只持付费住宅面的用户，从活动面发起 → 无活动账号，必须 ErrNoRealmAssignment，
// ★绝不能回落到付费住宅账号（回落 = 违反 R1）。
func TestRealmFace_CampaignAbsentDoesNotFallBackToPaid(t *testing.T) {
	const userID = "u-paid-only"
	store := &fakeVPNStore{rows: []*models.VPNProvision{{
		ID: "p-paid", UserID: userID,
		ServiceTier: models.ServiceTierResidential, PlanTier: "residential",
		ProductFace: models.ProductFaceResidential,
		OtunUUID:    ptrStr("uuid-paid-residential"), IsCurrent: true,
	}}}
	s := newSvc(true, store)

	got, err := s.resolveRealmOtunUUID(context.Background(), userID, RealmFace{
		PlanTier: models.PlanTierCampaign, ServiceTier: models.ServiceTierResidential,
	})
	if got != nil {
		t.Fatalf("★★违反红线 R1：活动面无账号时回落到了付费账号 %q", *got)
	}
	if !errors.Is(err, ErrNoRealmAssignment) {
		t.Fatalf("want ErrNoRealmAssignment, got %v", err)
	}
}
