package service

import (
	"context"
	"testing"
	"time"

	"github.com/wenwu/saas-platform/fulfillment-service/internal/client"
	"github.com/wenwu/saas-platform/fulfillment-service/internal/config"
	"github.com/wenwu/saas-platform/fulfillment-service/internal/models"
)

// 账号级封禁/解封联动（vpn_suspend_service.go）。
//
// 锁定的行为：
//   1. 封禁：basic / residential / promo 三个面一起 otun disable，active 行翻 suspended，
//      is_current 保持；已是 disabled 的行 otun 也停一次但状态不变；
//   2. 解封：suspended 行翻回 active + otun enable；已到期的面只翻状态不启用；
//      非 suspended 行（disabled）不动；
//   3. otun 失败：投影行照样记 suspended，结果 Partial=true 带原因（宁可记了没停，不能停了没记）；
//   4. 幂等：重复封禁跳过已 suspended 的面、不再打 otun；
//   5. /vpn/all 不下发 suspended 面。

func suspendRow(id, userID, uuid, face, tier, status string, exp time.Time) *models.VPNProvision {
	return &models.VPNProvision{
		ID:             id,
		UserID:         userID,
		SubscriptionID: "sub-" + id,
		OtunUUID:       ptrStr(uuid),
		PlanTier:       face,
		ServiceTier:    tier,
		ProductFace:    face,
		Status:         status,
		IsCurrent:      true,
		ExpireAt:       &exp,
	}
}

func newSuspendSvc(t *testing.T, store vpnProvisionStore) (*VPNService, *fakeOtun) {
	t.Helper()
	otun := newFakeOtun(t)
	cfg := &config.Config{MultiService: config.MultiServiceConfig{Enabled: true}}
	return &VPNService{cfg: cfg, vpnRepo: store, otunClient: client.NewOTunClient(otun.srv.URL, "test-secret")}, otun
}

func enabledFlags(puts []client.UpdateVPNUserRequest) []bool {
	out := make([]bool, 0, len(puts))
	for _, p := range puts {
		if p.Enabled == nil {
			out = append(out, true) // 不该出现：标记成 true 让断言失败
			continue
		}
		out = append(out, *p.Enabled)
	}
	return out
}

func TestSuspendCascade_AllFacesDisabledAndMarked(t *testing.T) {
	const user = "u-ban"
	future := time.Now().Add(7 * 24 * time.Hour)
	store := &fakeVPNStore{rows: []*models.VPNProvision{
		// 老的 disabled 行放最前：fake 的按面取 current 是"最后一条优先"（模拟 created_at DESC），
		// 让 basic 面的 current 解析到 p-basic，与生产"一面一条 current"的形态一致。
		suspendRow("p-old", user, "uuid-old", models.ProductFaceBasic, models.ServiceTierStandard, models.VPNProvisionStatusDisabled, future),
		suspendRow("p-basic", user, "uuid-basic", models.ProductFaceBasic, models.ServiceTierStandard, models.VPNProvisionStatusActive, future),
		suspendRow("p-resi", user, "uuid-resi", models.ProductFaceResidential, models.ServiceTierResidential, models.VPNProvisionStatusActive, future),
		suspendRow("p-promo", user, "uuid-promo", models.ProductFaceCampaign, models.ServiceTierResidential, models.VPNProvisionStatusActive, future),
		suspendRow("p-other", "someone-else", "uuid-other", models.ProductFaceBasic, models.ServiceTierStandard, models.VPNProvisionStatusActive, future),
	}}
	svc, otun := newSuspendSvc(t, store)

	res, err := svc.SuspendVPNByUser(context.Background(), user, "abuse")
	if err != nil {
		t.Fatalf("suspend: %v", err)
	}
	if res.Partial {
		t.Fatalf("unexpected partial: %+v", res)
	}
	if len(res.Faces) != 4 {
		t.Fatalf("faces = %d, want 4 (3 active + 1 disabled): %+v", len(res.Faces), res.Faces)
	}
	// otun：四个 uuid 都 PUT enabled=false（disabled 行也补停一次），别人的行不碰。
	if len(otun.putUUID) != 4 {
		t.Fatalf("otun PUT count = %d, want 4: %v", len(otun.putUUID), otun.putUUID)
	}
	for i, u := range otun.putUUID {
		if u == "uuid-other" {
			t.Fatalf("touched another user's uuid")
		}
		if otun.puts[i].Enabled == nil || *otun.puts[i].Enabled {
			t.Fatalf("PUT %s must carry enabled=false", u)
		}
		if otun.puts[i].TrafficLimit != 0 || otun.puts[i].ExpireAt != "" {
			t.Fatalf("PUT %s must only flip enabled, got %+v", u, otun.puts[i])
		}
	}
	// 投影行：active → suspended 且 is_current 保持；disabled 保持 disabled；别人不动。
	for _, r := range store.rows {
		switch r.ID {
		case "p-basic", "p-resi", "p-promo":
			if r.Status != models.VPNProvisionStatusSuspended || !r.IsCurrent {
				t.Fatalf("%s: status=%s is_current=%v, want suspended/true", r.ID, r.Status, r.IsCurrent)
			}
		case "p-old":
			if r.Status != models.VPNProvisionStatusDisabled {
				t.Fatalf("disabled row changed to %s", r.Status)
			}
		case "p-other":
			if r.Status != models.VPNProvisionStatusActive {
				t.Fatalf("other user's row changed to %s", r.Status)
			}
		}
	}

	// 幂等：再封一次，三个已 suspended 的面跳过；只有 disabled 行会再补停一次。
	before := len(otun.putUUID)
	res2, err := svc.SuspendVPNByUser(context.Background(), user, "abuse")
	if err != nil {
		t.Fatalf("suspend again: %v", err)
	}
	skipped := 0
	for _, f := range res2.Faces {
		if f.Skipped == "already suspended" {
			skipped++
		}
	}
	if skipped != 3 || len(otun.putUUID)-before != 1 {
		t.Fatalf("idempotency: skipped=%d extraPUT=%d, want 3/1", skipped, len(otun.putUUID)-before)
	}

	// /vpn/all 不下发 suspended 面（basic/residential 分区路径）。
	// hasActive 门控依赖 subscriptionClient；nil 时 GetUserVPNAll 视为 hasActive=true（与既有测试一致）。
	all, err := svc.GetUserVPNSubscribeConfigAll(context.Background(), user)
	if err != nil {
		t.Fatalf("vpn/all: %v", err)
	}
	if len(all) != 0 {
		t.Fatalf("suspended user must get no /vpn/all elements, got %d", len(all))
	}
}

func TestResumeCascade_RestoresOnlySuspendedAndSkipsExpired(t *testing.T) {
	const user = "u-unban"
	future := time.Now().Add(7 * 24 * time.Hour)
	past := time.Now().Add(-time.Hour)
	store := &fakeVPNStore{rows: []*models.VPNProvision{
		suspendRow("p-basic", user, "uuid-basic", models.ProductFaceBasic, models.ServiceTierStandard, models.VPNProvisionStatusSuspended, future),
		suspendRow("p-resi", user, "uuid-resi", models.ProductFaceResidential, models.ServiceTierResidential, models.VPNProvisionStatusSuspended, past), // 封禁期间到期
		suspendRow("p-old", user, "uuid-old", models.ProductFaceBasic, models.ServiceTierStandard, models.VPNProvisionStatusDisabled, future),
	}}
	svc, otun := newSuspendSvc(t, store)

	res, err := svc.ResumeVPNByUser(context.Background(), user, "")
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if res.Partial {
		t.Fatalf("unexpected partial: %+v", res)
	}
	// 只有未到期的 suspended 面打 otun enable=true。
	if len(otun.putUUID) != 1 || otun.putUUID[0] != "uuid-basic" || otun.puts[0].Enabled == nil || !*otun.puts[0].Enabled {
		t.Fatalf("otun enable calls = %v / %v, want only uuid-basic enabled=true", otun.putUUID, enabledFlags(otun.puts))
	}
	for _, r := range store.rows {
		switch r.ID {
		case "p-basic", "p-resi":
			if r.Status != models.VPNProvisionStatusActive {
				t.Fatalf("%s: status=%s, want active", r.ID, r.Status)
			}
		case "p-old":
			if r.Status != models.VPNProvisionStatusDisabled {
				t.Fatalf("disabled row must not be resumed, got %s", r.Status)
			}
		}
	}
	// 到期面：状态翻回 active（读时判定显示 expired），但标记跳过启用。
	for _, f := range res.Faces {
		if f.ProvisionID == "p-resi" && f.Skipped != "expired; not re-enabled" {
			t.Fatalf("expired face skipped=%q", f.Skipped)
		}
		if f.ProvisionID == "p-old" && f.Skipped != "not suspended" {
			t.Fatalf("disabled face skipped=%q", f.Skipped)
		}
	}
	// 读时判定：到期面即便 active 也回 expired。
	if got := effectiveProvisionStatus(models.VPNProvisionStatusActive, &past); got != models.VPNProvisionStatusExpired {
		t.Fatalf("effective status of resumed-but-expired = %s", got)
	}
}

func TestSuspendCascade_OtunFailureStillMarksAndReportsPartial(t *testing.T) {
	const user = "u-partial"
	future := time.Now().Add(7 * 24 * time.Hour)
	store := &fakeVPNStore{rows: []*models.VPNProvision{
		suspendRow("p-basic", user, "uuid-basic", models.ProductFaceBasic, models.ServiceTierStandard, models.VPNProvisionStatusActive, future),
		suspendRow("p-resi", user, "uuid-resi", models.ProductFaceResidential, models.ServiceTierResidential, models.VPNProvisionStatusActive, future),
	}}
	svc, otun := newSuspendSvc(t, store)
	otun.putFail = 1 // 第一个 PUT 500

	res, err := svc.SuspendVPNByUser(context.Background(), user, "abuse")
	if err != nil {
		t.Fatalf("suspend: %v", err)
	}
	if !res.Partial {
		t.Fatalf("expected partial result, got %+v", res)
	}
	failed, ok := 0, 0
	for _, f := range res.Faces {
		if f.OtunOK {
			ok++
		} else {
			failed++
			if f.OtunError == "" {
				t.Fatalf("failed face must carry otun_error")
			}
		}
		if f.Status != models.VPNProvisionStatusSuspended {
			t.Fatalf("%s must be marked suspended regardless of otun result, got %s", f.ProvisionID, f.Status)
		}
	}
	if failed != 1 || ok != 1 {
		t.Fatalf("failed=%d ok=%d, want 1/1", failed, ok)
	}
}
