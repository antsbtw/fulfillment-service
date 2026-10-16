package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/wenwu/saas-platform/fulfillment-service/internal/client"
	"github.com/wenwu/saas-platform/fulfillment-service/internal/models"
	"github.com/wenwu/saas-platform/fulfillment-service/internal/repository"
)

// refundRevocationStore 服务类退款停服扣减记录(*repository.RefundRevocationRepository 满足)。
type refundRevocationStore interface {
	Get(ctx context.Context, ref string) (*models.RefundRevocation, error)
	InsertIfAbsent(ctx context.Context, v *models.RefundRevocation) (bool, error)
}

// SetRefundRevocations 注入退款扣减记录仓(main.go 接线;未接线时 RefundRevoke 报错)。
func (s *VPNService) SetRefundRevocations(store refundRevocationStore) { s.refundRevocations = store }

// ErrRefundRevocationsNotWired 未注入记录仓。
var ErrRefundRevocationsNotWired = errors.New("refund revocation store not wired")

// RefundRevokeRequest 服务类订单退款后,从该用户对应线路扣回这笔订单买到的天数/流量
// (POST /api/internal/vpn/user/:user_id/refund-revoke;ADMIN_REFUND_DESIGN §3.3)。
type RefundRevokeRequest struct {
	UserID       string `json:"-"`
	Ref          string `json:"ref" binding:"required"`       // 幂等键:payment 侧付款 id
	PlanTier     string `json:"plan_tier" binding:"required"` // basic/premium → 标准线;residential → 住宅线
	Days         int    `json:"days"`                         // 这笔订单买到的天数
	TrafficBytes int64  `json:"traffic_bytes"`                // 这笔订单买到的流量(0 = 不扣流量)
	Reason       string `json:"reason"`
}

// RefundRevoke 开通按"用户 + 线路"一份、多笔订单天数叠加,所以不能整条禁用(会误伤其它订单的时长):
// 以 otun 实时到期为基准减去这笔的天数、额度减去这笔的流量;减完 ≤ 现在才禁用(enabled=false)。
// 先同步 otun 成功再改投影行与写记录;同 ref 重复调用返回首次结果(幂等)。
func (s *VPNService) RefundRevoke(ctx context.Context, req *RefundRevokeRequest) (*models.RefundRevocation, error) {
	if s.refundRevocations == nil {
		return nil, ErrRefundRevocationsNotWired
	}
	if req.UserID == "" || strings.TrimSpace(req.Ref) == "" || req.Days < 0 || req.TrafficBytes < 0 {
		return nil, fmt.Errorf("user_id and ref are required; days/traffic_bytes must be >= 0")
	}
	if models.IsCampaignPlanTier(req.PlanTier) {
		return nil, fmt.Errorf("plan_tier %q is the promo line; use campaign revoke", req.PlanTier)
	}
	if prev, err := s.refundRevocations.Get(ctx, req.Ref); err == nil && prev != nil {
		return prev, nil
	} else if err != nil && !errors.Is(err, repository.ErrNotFound) {
		return nil, err
	}

	isResidential := models.MapPlanToProductFace(req.PlanTier) == models.ProductFaceResidential
	rec := &models.RefundRevocation{Ref: req.Ref, UserID: req.UserID, PlanTier: req.PlanTier,
		Days: req.Days, TrafficBytes: req.TrafficBytes, Reason: req.Reason}

	vp, err := s.vpnRepo.GetCurrentByUserAndServicePartition(ctx, req.UserID, isResidential)
	if err != nil && !errors.Is(err, repository.ErrNotFound) {
		return nil, fmt.Errorf("lookup provision: %w", err)
	}
	if vp == nil {
		// 该线路没有开通(未开通成功 / 已被其它方式停掉):无可扣,只留记录
		rec.Noop = true
		return s.saveRefundRevocation(ctx, rec)
	}
	rec.ProvisionID = vp.ID

	// 基准到期:otun 实时值优先(与续购叠加同口径),拿不到退回投影行
	base := vp.ExpireAt
	if vp.OtunUUID != nil && *vp.OtunUUID != "" {
		if info, gerr := s.otunClient.GetUser(ctx, *vp.OtunUUID); gerr == nil && info != nil && info.ExpireAt != "" {
			if t, perr := time.Parse(time.RFC3339, info.ExpireAt); perr == nil {
				base = &t
			}
		}
	}
	now := time.Now()
	newExpire := now
	if base != nil {
		newExpire = base.AddDate(0, 0, -req.Days)
	}
	disable := !newExpire.After(now)
	if disable {
		newExpire = now
	}
	newLimit := vp.TrafficLimit - req.TrafficBytes
	if newLimit < 0 {
		newLimit = 0
	}
	rec.OldExpireAt, rec.NewExpireAt, rec.NewTrafficLimit, rec.Disabled = base, &newExpire, newLimit, disable

	if vp.OtunUUID != nil && *vp.OtunUUID != "" {
		upd := &client.UpdateVPNUserRequest{ExpireAt: newExpire.Format(time.RFC3339)}
		if req.TrafficBytes > 0 {
			// otun:traffic_limit=0 表示"不限"且 omitempty 会丢 0 → 下发 1 字节 = 已耗尽(同 RevokeCampaign)
			upd.TrafficLimit = newLimit
			if upd.TrafficLimit <= 0 {
				upd.TrafficLimit = 1
			}
		}
		if disable {
			f := false
			upd.Enabled = &f
		}
		if err := s.otunClient.UpdateUser(ctx, *vp.OtunUUID, upd); err != nil {
			// otun 未改成 → 整体失败(不改行、不写记录),调用方重试
			return nil, fmt.Errorf("sync refund revoke to otun failed: %w", err)
		}
	}
	vp.ExpireAt = &newExpire
	vp.TrafficLimit = newLimit
	if disable {
		vp.Status = models.VPNProvisionStatusDisabled
	}
	if err := s.vpnRepo.Update(ctx, vp); err != nil {
		return nil, fmt.Errorf("update provision: %w", err)
	}
	if s.logRepo != nil {
		s.logRepo.LogActionWithMetadata(ctx, vp.ID, "vpn", "vpn_refund_revoked", vp.Status, "Service purchase refunded",
			map[string]interface{}{"ref": req.Ref, "days": req.Days, "traffic_bytes": req.TrafficBytes,
				"expire_at": newExpire.Format(time.RFC3339), "traffic_limit": newLimit, "disabled": disable})
	}
	return s.saveRefundRevocation(ctx, rec)
}

func (s *VPNService) saveRefundRevocation(ctx context.Context, rec *models.RefundRevocation) (*models.RefundRevocation, error) {
	rec.CreatedAt = time.Now()
	inserted, err := s.refundRevocations.InsertIfAbsent(ctx, rec)
	if err != nil {
		return nil, err
	}
	if !inserted {
		// 并发重复:返回先写入的那条
		if prev, gerr := s.refundRevocations.Get(ctx, rec.Ref); gerr == nil && prev != nil {
			return prev, nil
		}
	}
	return rec, nil
}
