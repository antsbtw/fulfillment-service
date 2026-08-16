package service

import (
	"context"
	"errors"
	"testing"

	"github.com/wenwu/saas-platform/fulfillment-service/internal/models"
	"github.com/wenwu/saas-platform/fulfillment-service/internal/repository"
)

// TestDeprovisionByUser_StandardDoesNotTouchResidential（P0，规则 §1-#5）：
// 持双面的用户换绑 standard 订阅 → 只定位到 standard 面的 provision，residential 不被选中。
func TestDeprovisionByUser_StandardDoesNotTouchResidential(t *testing.T) {
	const userID = "u-both"
	store := &fakeVPNStore{rows: []*models.VPNProvision{
		standardRowNoUUID(userID),
		residentialRowNoUUID(userID), // 后建 → 旧的不分区查询会先命中它（created_at DESC）
	}}
	s := newSvc(true, store)
	ctx := context.Background()

	isRes := false
	vp, err := s.resolveDeprovisionTarget(ctx, userID, &isRes)
	if err != nil {
		t.Fatalf("resolve standard face: %v", err)
	}
	if vp.ServiceTier != models.ServiceTierStandard || vp.ID != "prov-std" {
		t.Fatalf("standard reassign must target the standard provision, got %s/%s", vp.ID, vp.ServiceTier)
	}

	// 反向：residential 面定位只命中 residential。
	isRes = true
	vp, err = s.resolveDeprovisionTarget(ctx, userID, &isRes)
	if err != nil {
		t.Fatalf("resolve residential face: %v", err)
	}
	if vp.ServiceTier != models.ServiceTierResidential || vp.ID != "prov-res" {
		t.Fatalf("residential reassign must target the residential provision, got %s/%s", vp.ID, vp.ServiceTier)
	}

	// 老调用方（未传面）→ 旧行为：不分区取最新一条（这里是 residential），保持兼容。
	vp, err = s.resolveDeprovisionTarget(ctx, userID, nil)
	if err != nil {
		t.Fatalf("legacy resolve: %v", err)
	}
	if vp.ID != "prov-res" {
		t.Fatalf("legacy (no face) should keep old any-status behaviour (latest row), got %s", vp.ID)
	}
}

// TestDeprovisionByUser_FaceMissingIsNotFound：该面没有 provision → ErrNotFound（调用方 404 幂等）。
func TestDeprovisionByUser_FaceMissingIsNotFound(t *testing.T) {
	const userID = "u-std-only"
	store := &fakeVPNStore{rows: []*models.VPNProvision{standardRowNoUUID(userID)}}
	s := newSvc(true, store)

	isRes := true
	_, err := s.resolveDeprovisionTarget(context.Background(), userID, &isRes)
	if !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("want ErrNotFound for missing residential face, got %v", err)
	}
}
