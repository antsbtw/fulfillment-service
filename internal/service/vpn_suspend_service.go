package service

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/wenwu/saas-platform/fulfillment-service/internal/models"
)

// 账号级封禁/解封联动（2026-09-06）。
//
// ★为什么需要它：auth-service 把 users.status 置为 suspended 只吊销 token；App 连接读的是
// 落盘配置、不走 API，节点侧凭证（otun users / realm_users）还活着，封禁后照样能连到到期日。
// 生产实证：封禁账号 2646059963@qq.com 后，其 basic / residential / promo 三个面全部仍在线。
//
// ★与既有回收路径的区别：
//   - DeprovisionVPNUser：一次一个面、is_current=false、通知 subscription 走 vpn_deleted——
//     那是"这笔订阅不再属于这个账号"，不可逆；
//   - 本路径：账号级、所有面一起、status=suspended 但 is_current 保持、订阅不动、
//     otun 只 disable 不删（uuid 保留），ResumeVPNByUser 翻回 active + otun enable 即恢复。
//
// ★节点侧生效链路（无需本服务额外动作）：
//   - 标准面：otun-manager /api/node/users 只下发 enabled=true，agent 下一轮同步（60s）差集 kick；
//   - 住宅面：realm_users.enabled 进 user_version，agent 热更时剔出配置；
//     otun-manager 迁移 038 起 PUT enabled=false 记 disable_reason='manual'，
//     不会再被 QuotaResetService 一分钟内自动恢复。

// AccessCascadeFace 是封禁/解封对单个面的处理结果。
type AccessCascadeFace struct {
	ProvisionID string `json:"provision_id"`
	ProductFace string `json:"product_face"`
	ServiceTier string `json:"service_tier"`
	OtunUUID    string `json:"otun_uuid,omitempty"`
	// Status 是处理后的投影行状态。
	Status string `json:"status"`
	// OtunOK：otun-manager 侧 disable/enable 是否成功；false 时 OtunError 带原因。
	// 投影行状态不因 otun 失败而回滚——封禁宁可"记了没停"也不能"停了没记"，
	// 调用方（auth）按 Partial 记告警，运维可按 provision_id 重试。
	OtunOK    bool   `json:"otun_ok"`
	OtunError string `json:"otun_error,omitempty"`
	// Skipped：本行无需处理（如封禁时已是 suspended、解封时不是 suspended、无 otun uuid）。
	Skipped string `json:"skipped,omitempty"`
}

// AccessCascadeResult 是封禁/解封的整体结果。
type AccessCascadeResult struct {
	UserID string              `json:"user_id"`
	Action string              `json:"action"` // suspend | resume
	Faces  []AccessCascadeFace `json:"faces"`
	// Partial：至少一个面 otun 侧失败。
	Partial bool `json:"partial"`
}

// SuspendVPNByUser 停掉该用户所有面的当前授权。幂等：已 suspended 的面跳过。
func (s *VPNService) SuspendVPNByUser(ctx context.Context, userID, reason string) (*AccessCascadeResult, error) {
	if userID == "" {
		return nil, fmt.Errorf("user_id required")
	}
	if reason == "" {
		reason = "account suspended"
	}
	rows, err := s.vpnRepo.ListCurrentByUser(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("list current provisions: %w", err)
	}
	res := &AccessCascadeResult{UserID: userID, Action: "suspend", Faces: make([]AccessCascadeFace, 0, len(rows))}
	for _, vp := range rows {
		face := AccessCascadeFace{
			ProvisionID: vp.ID,
			ProductFace: vp.EffectiveProductFace(),
			ServiceTier: vp.ServiceTier,
			Status:      vp.Status,
			OtunOK:      true,
		}
		if vp.OtunUUID != nil {
			face.OtunUUID = *vp.OtunUUID
		}
		if vp.Status == models.VPNProvisionStatusSuspended {
			face.Skipped = "already suspended"
			res.Faces = append(res.Faces, face)
			continue
		}
		// ① otun 侧先停（节点一分钟内踢人）。对 disabled/revoked/expired 行也停一次：幂等且
		//    能补上"投影行早已非 active 但 otun 账号仍 enabled"的漏网（到期无写路径，见读时判定注释）。
		if face.OtunUUID != "" {
			if derr := s.otunClient.DisableUser(ctx, face.OtunUUID); derr != nil {
				face.OtunOK = false
				face.OtunError = derr.Error()
				res.Partial = true
				log.Printf("[VPNService] suspend: otun disable failed user=%s uuid=%s face=%s: %v",
					userID, face.OtunUUID, face.ProductFace, derr)
			}
		} else {
			face.Skipped = "no otun uuid"
		}
		// ② 投影行：只有 active 行翻成 suspended（解封时只恢复这些）；其余状态原样保留，
		//    否则解封会把 disabled/revoked 行误翻成 active。
		if vp.Status == models.VPNProvisionStatusActive {
			vp.Status = models.VPNProvisionStatusSuspended
			if uerr := s.vpnRepo.Update(ctx, vp); uerr != nil {
				return nil, fmt.Errorf("update provision %s: %w", vp.ID, uerr)
			}
			face.Status = vp.Status
			if s.logRepo != nil {
				_ = s.logRepo.LogAction(ctx, vp.ID, "vpn", "vpn_user_suspended", "suspended", reason)
			}
		}
		res.Faces = append(res.Faces, face)
	}
	log.Printf("[VPNService] suspend cascade user=%s faces=%d partial=%v reason=%q", userID, len(res.Faces), res.Partial, reason)
	return res, nil
}

// ResumeVPNByUser 解封：suspended 行翻回 active，otun enable。已到期的面只翻状态不启用
// （读时判定会把它显示为 expired；节点侧本就按 expire_at 拒绝）。
func (s *VPNService) ResumeVPNByUser(ctx context.Context, userID, reason string) (*AccessCascadeResult, error) {
	if userID == "" {
		return nil, fmt.Errorf("user_id required")
	}
	if reason == "" {
		reason = "account reactivated"
	}
	rows, err := s.vpnRepo.ListCurrentByUser(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("list current provisions: %w", err)
	}
	now := time.Now()
	res := &AccessCascadeResult{UserID: userID, Action: "resume", Faces: make([]AccessCascadeFace, 0, len(rows))}
	for _, vp := range rows {
		face := AccessCascadeFace{
			ProvisionID: vp.ID,
			ProductFace: vp.EffectiveProductFace(),
			ServiceTier: vp.ServiceTier,
			Status:      vp.Status,
			OtunOK:      true,
		}
		if vp.OtunUUID != nil {
			face.OtunUUID = *vp.OtunUUID
		}
		if vp.Status != models.VPNProvisionStatusSuspended {
			face.Skipped = "not suspended"
			res.Faces = append(res.Faces, face)
			continue
		}
		vp.Status = models.VPNProvisionStatusActive
		if uerr := s.vpnRepo.Update(ctx, vp); uerr != nil {
			return nil, fmt.Errorf("update provision %s: %w", vp.ID, uerr)
		}
		face.Status = vp.Status
		switch {
		case face.OtunUUID == "":
			face.Skipped = "no otun uuid"
		case vp.ExpireAt != nil && !vp.ExpireAt.After(now):
			face.Skipped = "expired; not re-enabled"
		default:
			if eerr := s.otunClient.EnableUser(ctx, face.OtunUUID); eerr != nil {
				face.OtunOK = false
				face.OtunError = eerr.Error()
				res.Partial = true
				log.Printf("[VPNService] resume: otun enable failed user=%s uuid=%s face=%s: %v",
					userID, face.OtunUUID, face.ProductFace, eerr)
			}
		}
		if s.logRepo != nil {
			_ = s.logRepo.LogAction(ctx, vp.ID, "vpn", "vpn_user_resumed", "active", reason)
		}
		res.Faces = append(res.Faces, face)
	}
	log.Printf("[VPNService] resume cascade user=%s faces=%d partial=%v reason=%q", userID, len(res.Faces), res.Partial, reason)
	return res, nil
}
