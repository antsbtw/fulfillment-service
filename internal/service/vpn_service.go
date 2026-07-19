package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/wenwu/saas-platform/fulfillment-service/internal/client"
	"github.com/wenwu/saas-platform/fulfillment-service/internal/config"
	"github.com/wenwu/saas-platform/fulfillment-service/internal/models"
	"github.com/wenwu/saas-platform/fulfillment-service/internal/repository"
)

// vpnProvisionStore 是 VPNService 依赖的 vpn_provisions 读写子集（*repository.VPNProvisionRepository
// 满足它）。抽成接口只为可测：开关分流的并行 provision 逻辑（residential 与 standard 是否互相
// 覆盖）可以用 fake store 在无 DB 环境下断言，而不引入 mock 框架。生产仍注入真实 repo。
type vpnProvisionStore interface {
	GetBySubscriptionID(ctx context.Context, subscriptionID string) (*models.VPNProvision, error)
	GetBySubscriptionIDAndServicePartition(ctx context.Context, subscriptionID string, isResidential bool) (*models.VPNProvision, error)
	GetCurrentByUserAnyStatus(ctx context.Context, userID string) (*models.VPNProvision, error)
	GetCurrentByUserAndServicePartition(ctx context.Context, userID string, isResidential bool) (*models.VPNProvision, error)
	GetCurrentByUser(ctx context.Context, userID string) (*models.VPNProvision, error)
	GetByID(ctx context.Context, id string) (*models.VPNProvision, error)
	GetOtunUUIDByUser(ctx context.Context, userID string) (*string, error)
	GetOtunUUIDByUserAndServicePartition(ctx context.Context, userID string, isResidential bool) (*string, error)
	Create(ctx context.Context, vp *models.VPNProvision) error
	Update(ctx context.Context, vp *models.VPNProvision) error
	MarkNotCurrent(ctx context.Context, id string) error
	UpdateEmailByUserID(ctx context.Context, userID, email string) error
}

// VPNService handles VPN user provisioning operations
type VPNService struct {
	cfg                *config.Config
	vpnRepo            vpnProvisionStore
	logRepo            *repository.LogRepository
	otunClient         *client.OTunClient
	subscriptionClient *client.SubscriptionClient
}

// NewVPNService creates a new VPN service
func NewVPNService(
	cfg *config.Config,
	vpnRepo *repository.VPNProvisionRepository,
	logRepo *repository.LogRepository,
	// 注：vpnRepo 形参仍是具体 *repository.VPNProvisionRepository（main.go 不变），
	// 内部以 vpnProvisionStore 接口持有，仅为可测。

	otunClient *client.OTunClient,
	subscriptionClient *client.SubscriptionClient,
) *VPNService {
	return &VPNService{
		cfg:                cfg,
		vpnRepo:            vpnRepo,
		logRepo:            logRepo,
		otunClient:         otunClient,
		subscriptionClient: subscriptionClient,
	}
}

// ProvisionVPNUser creates or renews a VPN user in otun-manager
func (s *VPNService) ProvisionVPNUser(ctx context.Context, req *models.ProvisionRequest) (*models.ProvisionResponse, error) {
	log.Printf("[VPNService] Provisioning VPN user for subscription=%s, plan=%s, channel=%s",
		req.SubscriptionID, req.PlanTier, req.Channel)

	// Idempotency check: if this subscription has already been provisioned, return existing result.
	// 短路条件必须同时满足【套餐没变】且【到期时间没有要延长】——只比 tier 是不够的：
	//   - 套餐升降级（如 Apple 同一订阅先发 basic 再发 residential 升级）：tier 变，要放行，
	//     否则新 tier 永远下发不到 otun-manager（residential 卡在 standard，realm 链断）。
	//   - 同档续期 / 赠送续期（gift 把周期改成 1 年）：tier 不变但 expire_at 要推后，也要放行，
	//     否则被当成重复请求短路丢弃，新 expire_at 永远到不了 otun-manager（订阅表已是 1 年、
	//     但节点仍按旧周期到期）。判据用与下面 renewal 分支一致的 calculateExpireAt 预算，
	//     保证“是否延长”的判断和真正执行的 expireAt 同源。
	// 只有 tier 没变【且】新算出的 expireAt 不晚于现有到期时间，才是真正的重复请求，才短路。

	// 本次请求的 service_tier 分区（residential vs 非 residential）。MULTI_SERVICE_ENABLED=true 时
	// 用于按分区取该 user 的同分区记录，避免 residential 命中 standard 记录（反之亦然）→ 不再互相覆盖。
	reqIsResidential := models.MapPlanToServiceTier(req.PlanTier) == models.ServiceTierResidential

	if req.SubscriptionID != "" {
		existingBySubID, _ := s.resolveExistingBySubID(ctx, req.SubscriptionID, reqIsResidential)
		if existingBySubID != nil && existingBySubID.Status == models.VPNProvisionStatusActive && existingBySubID.OtunUUID != nil {
			incomingServiceTier := models.MapPlanToServiceTier(req.PlanTier)
			tierUnchanged := existingBySubID.ServiceTier == incomingServiceTier &&
				existingBySubID.PlanTier == req.PlanTier

			// 用与 renewal 分支同源的算法预估本次请求想要的 expireAt，判断是否在延长周期。
			// gift/trial/apple/google 都是 fresh period（calculateExpireAt），与下方 switch 一致。
			prospectiveExpireDays := s.calculateExpireDays(req.Channel, req.ExpireDays)
			prospectiveExpireAt := s.calculateExpireAt(prospectiveExpireDays)
			// 容差 1 分钟，避免“同一秒重复回放”因纳秒级差异被误判为延长而反复下发。
			extendsExpiry := existingBySubID.ExpireAt == nil ||
				prospectiveExpireAt.After(existingBySubID.ExpireAt.Add(time.Minute))

			if tierUnchanged && !extendsExpiry {
				log.Printf("[VPNService] Already provisioned for subscription=%s (provision=%s), same tier & no expiry extension, skipping",
					req.SubscriptionID, existingBySubID.ID)
				return &models.ProvisionResponse{
					ResourceID: existingBySubID.ID,
					Status:     models.StatusActive,
					VPNUserID:  *existingBySubID.OtunUUID,
					Message:    "Already provisioned (idempotent)",
				}, nil
			}
			reason := "tier change"
			if tierUnchanged {
				reason = "expiry extension (renewal/gift)"
			}
			log.Printf("[VPNService] Provision update for subscription=%s (provision=%s) [%s]: "+
				"%s/%s → %s/%s, prospectiveExpire=%s, proceeding to update",
				req.SubscriptionID, existingBySubID.ID, reason,
				existingBySubID.PlanTier, existingBySubID.ServiceTier,
				req.PlanTier, incomingServiceTier, prospectiveExpireAt.Format(time.RFC3339))
		}
	}

	// Determine business_type from request
	businessType := req.BusinessType
	if businessType == "" {
		businessType = models.BusinessTypeSubscription
	}

	// Determine service_tier from plan_tier
	serviceTier := models.MapPlanToServiceTier(req.PlanTier)

	// 1. Check if user already has a current VPN provision
	existing, err := s.resolveExistingCurrent(ctx, req.UserID, reqIsResidential)
	if err == nil && existing != nil && existing.OtunUUID != nil && *existing.OtunUUID != "" {
		// Renewal scenario: update expire_at and traffic_limit
		vpnUserID := *existing.OtunUUID
		expireDays := s.calculateExpireDays(req.Channel, req.ExpireDays)
		trafficLimit := s.calculateTrafficLimit(req.PlanTier, req.TrafficLimit)

		// Determine expiration strategy by channel:
		// - apple/google: platform manages renewal cycle, always fresh period
		// - trial/gift: independent grants, always fresh period
		// - trial/gift → stripe: channel upgrade, fresh period (don't stack free trial time)
		// - stripe → stripe: user paid money, stack on remaining time if not expired
		var expireAt time.Time
		switch req.Channel {
		case "apple", "google", "trial", "gift":
			expireAt = s.calculateExpireAt(expireDays)
			log.Printf("[VPNService] %s: fresh period, expire=%s", req.Channel, expireAt.Format(time.RFC3339))
		default:
			if existing.Channel == "trial" || existing.Channel == "gift" {
				// Channel upgrade from free to paid: don't stack free trial/gift remaining time
				expireAt = s.calculateExpireAt(expireDays)
				log.Printf("[VPNService] Channel upgrade %s → %s: fresh period, expire=%s",
					existing.Channel, req.Channel, expireAt.Format(time.RFC3339))
			} else {
				// Same paid channel renewal (e.g., stripe → stripe): stack on remaining time
				expireAt = s.calculateExpireAtWithStacking(ctx, vpnUserID, expireDays)
			}
		}

		if err := s.syncOtunUserQuota(ctx, vpnUserID, serviceTier, req.UserID, req.UserEmail, trafficLimit, expireAt); err != nil {
			log.Printf("[VPNService] Warning: failed to update existing VPN user: %v", err)
		} else {
			log.Printf("[VPNService] Updated existing VPN user %s: expire=%s, traffic=%d",
				vpnUserID, expireAt.Format(time.RFC3339), trafficLimit)
		}

		// If channel changed (e.g., trial → apple), preserve old record as history
		if existing.Channel != req.Channel {
			s.vpnRepo.MarkNotCurrent(ctx, existing.ID)
			log.Printf("[VPNService] Channel changed %s → %s, creating new provision record", existing.Channel, req.Channel)

			newProvisionID := uuid.New().String()
			newExpireAt := expireAt
			newVP := &models.VPNProvision{
				ID:             newProvisionID,
				UserID:         req.UserID,
				SubscriptionID: req.SubscriptionID,
				Channel:        req.Channel,
				BusinessType:   businessType,
				ServiceTier:    serviceTier,
				OtunUUID:       existing.OtunUUID,
				PlanTier:       req.PlanTier,
				Status:         models.VPNProvisionStatusActive,
				TrafficLimit:   trafficLimit,
				TrafficUsed:    0,
				ExpireAt:       &newExpireAt,
				Email:          req.UserEmail,
				DeviceID:       req.DeviceID,
				IsCurrent:      true,
			}
			if err := s.vpnRepo.Create(ctx, newVP); err != nil {
				log.Printf("[VPNService] Warning: failed to create new provision: %v", err)
			}

			return &models.ProvisionResponse{
				ResourceID: newProvisionID,
				Status:     models.StatusActive,
				VPNUserID:  vpnUserID,
				Message:    "VPN user updated (channel upgrade)",
			}, nil
		}

		// Same channel renewal: update existing record in-place
		existing.TrafficLimit = trafficLimit
		existing.SubscriptionID = req.SubscriptionID
		existing.BusinessType = businessType
		existing.ServiceTier = serviceTier
		existing.PlanTier = req.PlanTier
		existing.Status = models.VPNProvisionStatusActive
		s.vpnRepo.Update(ctx, existing)

		return &models.ProvisionResponse{
			ResourceID: existing.ID,
			Status:     models.StatusActive,
			VPNUserID:  vpnUserID,
			Message:    "VPN user updated (renewal)",
		}, nil
	}

	// 2. Calculate traffic limit and expire time
	trafficLimit := s.calculateTrafficLimit(req.PlanTier, req.TrafficLimit)
	expireDays := s.calculateExpireDays(req.Channel, req.ExpireDays)
	expireAt := s.calculateExpireAt(expireDays)
	log.Printf("[VPNService] ProvisionVPNUser: expireDays=%d, trafficLimit=%d, expireAt=%s",
		expireDays, trafficLimit, expireAt.Format(time.RFC3339))

	// 3. Check if user has an existing otun_uuid from any previous provision (e.g., trial)
	existingOtunUUID, _ := s.resolveExistingOtunUUID(ctx, req.UserID, reqIsResidential)

	var actualVPNUserID string

	if existingOtunUUID != nil && *existingOtunUUID != "" {
		// Reuse existing otun_uuid (e.g., trial → purchase conversion)
		actualVPNUserID = *existingOtunUUID
		if err := s.syncOtunUserQuota(ctx, actualVPNUserID, serviceTier, req.UserID, req.UserEmail, trafficLimit, expireAt); err != nil {
			return nil, fmt.Errorf("failed to update existing VPN user: %w", err)
		}

		// Mark old provision as not current (trial → converted)
		if existing != nil {
			s.vpnRepo.MarkNotCurrent(ctx, existing.ID)
		}
	} else {
		// Create new VPN user in otun-manager
		vpnUserID := uuid.New().String()
		ssPassword := generateRandomPassword(16)

		otunReq := &client.CreateVPNUserRequest{
			UUID:         vpnUserID,
			Email:        req.UserEmail,
			AuthUserID:   req.UserID,
			Protocols:    []string{"vless", "shadowsocks"},
			SSPassword:   ssPassword,
			TrafficLimit: trafficLimit,
			ExpireAt:     expireAt.Format(time.RFC3339),
			ServiceTier:  serviceTier,
		}

		otunResp, err := s.otunClient.CreateUser(ctx, otunReq)
		if err != nil {
			s.logRepo.LogAction(ctx, "", "vpn", "vpn_user_create_failed", "failed", err.Error())
			return nil, fmt.Errorf("failed to create VPN user in otun-manager: %w", err)
		}

		actualVPNUserID = otunResp.UUID
		if actualVPNUserID == "" {
			actualVPNUserID = vpnUserID
		}
	}

	// 4. Create local VPN provision record
	provisionID := uuid.New().String()
	vp := &models.VPNProvision{
		ID:             provisionID,
		UserID:         req.UserID,
		SubscriptionID: req.SubscriptionID,
		Channel:        req.Channel,
		BusinessType:   businessType,
		ServiceTier:    serviceTier,
		OtunUUID:       &actualVPNUserID,
		PlanTier:       req.PlanTier,
		Status:         models.VPNProvisionStatusActive,
		TrafficLimit:   trafficLimit,
		TrafficUsed:    0,
		ExpireAt:       &expireAt,
		Email:          req.UserEmail,
		DeviceID:       req.DeviceID,
		IsCurrent:      true,
	}

	if err := s.vpnRepo.Create(ctx, vp); err != nil {
		_ = s.otunClient.DeleteUser(ctx, actualVPNUserID)
		return nil, fmt.Errorf("failed to save vpn provision: %w", err)
	}

	// 5. Log action
	s.logRepo.LogActionWithMetadata(ctx, provisionID, "vpn", "vpn_user_created", "active",
		"VPN user created successfully",
		map[string]interface{}{
			"vpn_user_id":   actualVPNUserID,
			"plan_tier":     req.PlanTier,
			"service_tier":  serviceTier,
			"traffic_limit": trafficLimit,
			"expire_at":     expireAt.Format(time.RFC3339),
			"channel":       req.Channel,
		})

	// 6. Notify subscription-service
	s.notifyVPNActive(ctx, req.SubscriptionID, provisionID, actualVPNUserID)

	log.Printf("[VPNService] VPN user created successfully: provision=%s, vpn_user=%s", provisionID, actualVPNUserID)

	return &models.ProvisionResponse{
		ResourceID: provisionID,
		Status:     models.StatusActive,
		VPNUserID:  actualVPNUserID,
		Message:    "VPN user created successfully",
	}, nil
}

// ==================== MULTI_SERVICE 开关：provision 查询分流 ====================
//
// 三个 resolveExisting* 是 ProvisionVPNUser 里"会因 service_tier 区分而改变结果"的 user/订阅级
// 查询的唯一收口。开关 false 分支逐字节调用原 repo 方法（GetBySubscriptionID /
// GetCurrentByUserAnyStatus / GetOtunUUIDByUser），与开关引入前完全等价（互斥语义不变）。
// 开关 true 分支调用新增的 *AndServicePartition 方法，按 reqIsResidential 分区取记录，使
// residential 与 standard 两条 current provision 互不命中、互不覆盖；且 residential 不复用
// standard 的 otun_uuid（partition 查询在 residential 分区内查不到 standard uuid → 走 CreateUser
// 新建独立 UUID → otun-manager 据 service_tier=residential 写独立 realm_users 表）。

// resolveExistingBySubID 取该订阅的 current provision（幂等短路用）。
func (s *VPNService) resolveExistingBySubID(ctx context.Context, subscriptionID string, isResidential bool) (*models.VPNProvision, error) {
	if s.cfg.MultiService.Enabled {
		return s.vpnRepo.GetBySubscriptionIDAndServicePartition(ctx, subscriptionID, isResidential)
	}
	return s.vpnRepo.GetBySubscriptionID(ctx, subscriptionID)
}

// resolveExistingCurrent 取该 user 的 current provision（renewal/覆盖判定用）。
func (s *VPNService) resolveExistingCurrent(ctx context.Context, userID string, isResidential bool) (*models.VPNProvision, error) {
	if s.cfg.MultiService.Enabled {
		return s.vpnRepo.GetCurrentByUserAndServicePartition(ctx, userID, isResidential)
	}
	return s.vpnRepo.GetCurrentByUserAnyStatus(ctx, userID)
}

// resolveExistingOtunUUID 取该 user 可复用的 otun_uuid（trial→purchase 复用 / residential 不复用）。
func (s *VPNService) resolveExistingOtunUUID(ctx context.Context, userID string, isResidential bool) (*string, error) {
	if s.cfg.MultiService.Enabled {
		return s.vpnRepo.GetOtunUUIDByUserAndServicePartition(ctx, userID, isResidential)
	}
	return s.vpnRepo.GetOtunUUIDByUser(ctx, userID)
}

// DeprovisionVPNUser disables a VPN user
func (s *VPNService) DeprovisionVPNUser(ctx context.Context, provisionID, reason string) error {
	log.Printf("[VPNService] Deprovisioning VPN user: provision=%s, reason=%s", provisionID, reason)

	vp, err := s.vpnRepo.GetByID(ctx, provisionID)
	if err != nil {
		return fmt.Errorf("vpn provision not found: %w", err)
	}

	// Disable user in otun-manager
	if vp.OtunUUID != nil && *vp.OtunUUID != "" {
		if err := s.otunClient.DisableUser(ctx, *vp.OtunUUID); err != nil {
			log.Printf("[VPNService] Warning: failed to disable VPN user in otun-manager: %v", err)
		}
	}

	// Update local status
	vp.Status = models.VPNProvisionStatusDisabled
	vp.IsCurrent = false
	if err := s.vpnRepo.Update(ctx, vp); err != nil {
		return fmt.Errorf("failed to update vpn provision: %w", err)
	}

	s.logRepo.LogAction(ctx, vp.ID, "vpn", "vpn_user_deprovisioned", "disabled", reason)

	// Notify subscription-service
	if vp.SubscriptionID != "" {
		if err := s.subscriptionClient.NotifyVPNDeleted(ctx, vp.SubscriptionID, vp.ID); err != nil {
			log.Printf("[VPNService] Failed to notify subscription-service (deleted): %v", err)
		}
	}

	log.Printf("[VPNService] VPN user deprovisioned successfully: %s", vp.ID)
	return nil
}

// DeprovisionVPNByUser 停用某用户当前的 VPN 用户（按 user_id 解析出 current provision 再走
// DeprovisionVPNUser）。用于订阅换绑：把交易从旧登录账号迁到新登录账号时，回收旧账号正在跑的
// otun-manager VPN 用户，避免一笔订阅养两个 VPN 用户。
// 用户本就没有 current provision 时返回 repository.ErrNotFound（调用方据此回 404 + 幂等处理）。
func (s *VPNService) DeprovisionVPNByUser(ctx context.Context, userID, reason string) error {
	vp, err := s.vpnRepo.GetCurrentByUserAnyStatus(ctx, userID)
	if err != nil {
		// 含 repository.ErrNotFound：无可回收的 VPN 用户。
		return err
	}
	return s.DeprovisionVPNUser(ctx, vp.ID, reason)
}

// UpdateVPNUser updates a VPN user (extend/upgrade)
// syncOtunUserQuota 把新的额度/到期下发到 otun-manager,按 service_tier 分流:
//   - standard → PUT /api/users/:uuid(原路径不变);
//   - residential → POST /api/users(otun 侧按 auth_user_id UPSERT:已存在只更新
//     额度/到期,uuid 稳定不变,并幂等 EnsureDefaultAssignment)。
//
// ★为什么必须分流(freely.gx 10GB 案,2026-07-08):WP-2 basic/residential 解耦后
// residential 用户迁出 users 表(独立 realm_users),PUT /api/users/:uuid 对其恒 404。
// 此前续期/trial→购买转换两条路径对 residential 也打 PUT,404 被 Warning 吞掉,
// 导致 realm_users.traffic_limit 永远停在首开(trial)值——正式 100GB 下发不进真源,
// App 流量回显(真源=realm_users)一直显示 trial 的 10GB。
func (s *VPNService) syncOtunUserQuota(ctx context.Context, otunUUID, serviceTier, authUserID, email string, trafficLimit int64, expireAt time.Time) error {
	if serviceTier == models.ServiceTierResidential {
		_, err := s.otunClient.CreateUser(ctx, &client.CreateVPNUserRequest{
			UUID:         otunUUID, // 兼容携带;UPSERT 实际按 auth_user_id 定位,uuid 不变
			AuthUserID:   authUserID,
			Email:        email,
			TrafficLimit: trafficLimit,
			ExpireAt:     expireAt.Format(time.RFC3339),
			ServiceTier:  serviceTier,
		})
		return err
	}
	enabled := true
	return s.otunClient.UpdateUser(ctx, otunUUID, &client.UpdateVPNUserRequest{
		TrafficLimit: trafficLimit,
		ExpireAt:     expireAt.Format(time.RFC3339),
		Enabled:      &enabled,
		ServiceTier:  serviceTier, // 套餐升降级时更新 tier（修复 standard→residential 不变）
	})
}

func (s *VPNService) UpdateVPNUser(ctx context.Context, provisionID string, req *models.UpdateVPNUserRequest) error {
	log.Printf("[VPNService] Updating VPN user: provision=%s", provisionID)

	vp, err := s.vpnRepo.GetByID(ctx, provisionID)
	if err != nil {
		return fmt.Errorf("vpn provision not found: %w", err)
	}

	if vp.OtunUUID == nil || *vp.OtunUUID == "" {
		return fmt.Errorf("VPN user ID not found in provision")
	}

	// Get current user info from otun-manager
	userInfo, err := s.otunClient.GetUser(ctx, *vp.OtunUUID)
	if err != nil {
		return fmt.Errorf("failed to get VPN user from otun-manager: %w", err)
	}

	updateReq := &client.UpdateVPNUserRequest{}
	needUpdate := false

	if req.TrafficLimit > 0 {
		updateReq.TrafficLimit = req.TrafficLimit
		vp.TrafficLimit = req.TrafficLimit
		needUpdate = true
	}

	if req.ExtendDays > 0 {
		currentExpire, _ := time.Parse(time.RFC3339, userInfo.ExpireAt)
		if currentExpire.Before(time.Now()) {
			currentExpire = time.Now()
		}
		newExpire := currentExpire.AddDate(0, 0, req.ExtendDays)
		updateReq.ExpireAt = newExpire.Format(time.RFC3339)
		needUpdate = true
	}

	if req.PlanTier != "" && req.PlanTier != vp.PlanTier {
		vp.PlanTier = req.PlanTier
		vp.ServiceTier = models.MapPlanToServiceTier(req.PlanTier)
		if req.TrafficLimit == 0 {
			newLimit := s.calculateTrafficLimit(req.PlanTier, 0)
			updateReq.TrafficLimit = newLimit
			vp.TrafficLimit = newLimit
		}
		needUpdate = true
	}

	if !needUpdate {
		return nil
	}

	if err := s.otunClient.UpdateUser(ctx, *vp.OtunUUID, updateReq); err != nil {
		return fmt.Errorf("failed to update VPN user in otun-manager: %w", err)
	}

	if err := s.vpnRepo.Update(ctx, vp); err != nil {
		return fmt.Errorf("failed to update vpn provision: %w", err)
	}

	s.logRepo.LogActionWithMetadata(ctx, provisionID, "vpn", "vpn_user_updated", "active",
		"VPN user updated",
		map[string]interface{}{
			"traffic_limit": vp.TrafficLimit,
			"plan_tier":     vp.PlanTier,
			"extend_days":   req.ExtendDays,
		})

	log.Printf("[VPNService] VPN user updated successfully: %s", provisionID)
	return nil
}

// GetUserVPNStatus gets VPN status for a user
func (s *VPNService) GetUserVPNStatus(ctx context.Context, userID string) (*models.VPNStatusResponse, error) {
	// 1. Check subscription status
	subStatus, err := s.subscriptionClient.GetUserVPNSubscription(ctx, userID)
	if err != nil {
		log.Printf("[VPNService] Error checking subscription: %v", err)
		subStatus = nil
	}

	// 2. Check VPN provision
	vp, _ := s.vpnRepo.GetCurrentByUser(ctx, userID)

	// 3. Build response
	resp := &models.VPNStatusResponse{}

	if subStatus == nil || !subStatus.HasActive {
		resp.VPNStatus = models.VPNStatusNoSubscription
		resp.HasSubscription = false
		resp.HasVPNUser = false
		resp.Message = "No active VPN subscription. Please subscribe to use VPN."
		return resp, nil
	}

	resp.HasSubscription = true
	resp.Subscription = &models.SubscriptionInfo{
		SubscriptionID: subStatus.SubscriptionID,
		Status:         subStatus.Status,
		PlanTier:       subStatus.PlanTier,
		ExpiresAt:      subStatus.ExpiresAt,
		AutoRenew:      subStatus.AutoRenew,
	}

	if vp == nil {
		resp.VPNStatus = models.VPNStatusExpired
		resp.HasVPNUser = false
		resp.Message = "VPN subscription active but no VPN user found. Please contact support."
		return resp, nil
	}

	resp.HasVPNUser = true

	trafficLimitGB := float64(vp.TrafficLimit) / (1024 * 1024 * 1024)
	trafficUsedGB := float64(vp.TrafficUsed) / (1024 * 1024 * 1024)
	trafficPercent := 0.0
	if vp.TrafficLimit > 0 {
		trafficPercent = (float64(vp.TrafficUsed) / float64(vp.TrafficLimit)) * 100
	}

	vpnUserID := ""
	if vp.OtunUUID != nil {
		vpnUserID = *vp.OtunUUID
	}

	expireAtStr := ""
	if vp.ExpireAt != nil {
		expireAtStr = vp.ExpireAt.Format(time.RFC3339)
	}

	resp.VPNUser = &models.VPNUserInfo{
		ResourceID:     vp.ID,
		VPNUserID:      vpnUserID,
		Status:         vp.Status,
		PlanTier:       vp.PlanTier,
		TrafficLimitGB: trafficLimitGB,
		TrafficUsedGB:  trafficUsedGB,
		TrafficPercent: trafficPercent,
		ExpireAt:       expireAtStr,
		CreatedAt:      vp.CreatedAt.Format(time.RFC3339),
	}

	switch vp.Status {
	case models.VPNProvisionStatusActive:
		resp.VPNStatus = models.VPNStatusActive
		resp.Message = "VPN is active and ready to use."
	case models.VPNProvisionStatusExpired:
		resp.VPNStatus = models.VPNStatusExpired
		resp.Message = "VPN subscription expired."
	default:
		resp.VPNStatus = models.VPNStatusDisabled
		resp.Message = "VPN is currently disabled."
	}

	return resp, nil
}

// GetUserVPNSubscribeConfig gets VPN subscription configuration for a user
func (s *VPNService) GetUserVPNSubscribeConfig(ctx context.Context, userID string) (*models.VPNSubscribeResponse, error) {
	// Verify active subscription in subscription-service (source of truth)
	if s.subscriptionClient != nil {
		subStatus, err := s.subscriptionClient.GetUserVPNSubscription(ctx, userID)
		if err != nil {
			log.Printf("[VPNService] Error checking subscription for config: %v", err)
			return nil, fmt.Errorf("failed to verify subscription status")
		}
		if subStatus == nil || !subStatus.HasActive {
			return nil, fmt.Errorf("no active VPN subscription")
		}
	}

	vp, err := s.vpnRepo.GetCurrentByUser(ctx, userID)
	if err != nil || vp == nil {
		return nil, fmt.Errorf("no active VPN provision")
	}

	return s.buildSubscribeResponse(ctx, vp, userID)
}

// GetUserConfigVersion 返回用户当前 VPN 配置的 config_version（§8.3 轻量版本端点）。
// 复用完整装配（config_version 依赖 protocols + smart_strategy，无法比全量更省），
// 只回 version 串，让前端拉取体积小、可高频轮询。无有效订阅/provision → 与全量端点同语义报错。
//
// ★分面聚合（2026-07-17，前端真机坐实契约 §3 偏差）：旧实现走不分面的 GetUserVPNSubscribeConfig
// （GetCurrentByUser 取 created_at 最新一条），双面用户取到标准面时住宅授权集准入/撤销永不翻转
// 版本 → 前端版本驱动刷新全瘫。本端点语义 =「/vpn/all 会变吗」，故直接镜像 GetUserVPNSubscribeConfigAll
// 的内容（同一装配、同一跳面口径）：
//   - 单面用户：返回该面 config_version 原值（与旧行为、与 /vpn/all 对应项逐字节一致，零回归）；
//   - 双面用户：各面 version 合并 hash（任一面结构性变化、面增减都翻转，见 combineConfigVersions）。
func (s *VPNService) GetUserConfigVersion(ctx context.Context, userID string) (string, error) {
	all, err := s.GetUserVPNSubscribeConfigAll(ctx, userID)
	if err != nil {
		return "", err
	}
	if len(all) == 0 {
		return "", fmt.Errorf("no active VPN provision")
	}
	versions := make([]string, 0, len(all))
	for _, r := range all {
		versions = append(versions, r.ConfigVersion)
	}
	return combineConfigVersions(versions), nil
}

// buildSubscribeResponse 根据【单条】provision 构造该面的订阅配置（residential→realm
// connect-url；标准→otun-manager /api/subscribe）。从 GetUserVPNSubscribeConfig 抽出，
// 供 GetUserVPNSubscribeConfigAll 按服务面分别复用，逻辑零改动。deviceID 入参 = auth user_id。
func (s *VPNService) buildSubscribeResponse(ctx context.Context, vp *models.VPNProvision, deviceID string) (*models.VPNSubscribeResponse, error) {
	var protocols []models.VPNProtocol
	var expireAt string
	// P0：仅 residential(realm) 分支填充；标准套餐留空（前端对缺字段降级）。
	var exitCountry string
	var smartStrategy json.RawMessage
	var realmNodes []models.RealmNodeSummary // ★2c：N=2 出口摘要（region 出口级），仅 realm 分支填
	var vpnRegions []models.VPNRegion       // ★阶段2：授权集区域包（仅 realm 分支填）

	// 用量默认取 vp，两个分支下方各用自己面的真源覆盖（vpn_provisions 的 traffic_used
	// 无人回写恒 0，只作老 otun 缺字段时的兜底）。
	trafficUsed := vp.TrafficUsed
	trafficLimit := vp.TrafficLimit

	if vp.ServiceTier == models.ServiceTierResidential {
		// residential 套餐：只返回该套餐自己的 realm 连接 URL（hysteria2-realm://...），
		// 不返标准节点协议。前端解析这一条 url 即可，与解析标准套餐 url 同一流程，无需感知 realm。
		// URL 由 otun-manager 按用户【当前出口】生成（GET /api/v1/internal/realm/connect-url）。
		if vp.OtunUUID == nil || *vp.OtunUUID == "" {
			return nil, fmt.Errorf("residential user has no otun uuid")
		}
		realmResp, rerr := s.otunClient.GetRealmConnectURL(ctx, *vp.OtunUUID)
		if rerr != nil {
			return nil, fmt.Errorf("failed to get realm connect-url from otun-manager: %w", rerr)
		}
		if realmResp == nil || realmResp.ConnectURL == "" {
			// manager 未配默认出口 / 用户未分配出口。
			return nil, fmt.Errorf("no realm egress assigned for residential user")
		}
		// ★2c：manager 下发 nodes[]（N=2 出口，primary + backup，各六协议 URL）→ 展成 primary6 + backup6
		// （最多 12 条）VPNProtocol，node 区分主备，App 选出口再选协议。region 不挂 protocol（出口级归属），
		// 改由顶层 nodes[] 摘要表达（每出口 1 次），前端按 protocol.node == nodes[].role 匹配取 region。
		// 兜底链：nodes[] 空（老 otun / 单出口）→ connect_urls[]（六协议单出口）→ 单 hy2。逐级零回归。
		protocols = append(protocols, buildRealmProtocolsN2(realmResp.Nodes, realmResp.ConnectURLs, realmResp.ConnectURL)...)
		realmNodes = buildRealmNodeSummaries(realmResp.Nodes) // nodes 空→nil（前端不读也不坏）
		// ★阶段2（契约 §2.1）：授权集区域包透传（每区域 protocols 由该区域各节点 connect_urls
		// 展开；strategy 整包同源）。老 otun 不下发 → nil → omitempty 不输出（零回归）。
		vpnRegions = buildVPNRegions(realmResp.Regions)
		// P0：透传 manager 下发的出口国家 + 分流策略（仅 realm 分支）。
		exitCountry = realmResp.ExitCountry
		smartStrategy = realmResp.SmartStrategy
		// ★residential 用量取 realm 真源（realm_users，agent 上报按 uuid 跨出口聚合），
		// 覆盖 vp.TrafficUsed（vpn_provisions 那列开通后恒 0 从不回写 → 前端永远显示 0）。
		// 老 otun 不下发这两个字段 → realmResp.Traffic* 为 0 → 退回 vp 值（向后兼容）。
		// TrafficLimit 同源覆盖：realm 额度真源在 realm_users，与踢人判定同一口径。
		if realmResp.TrafficUsed > 0 {
			trafficUsed = realmResp.TrafficUsed
		}
		if realmResp.TrafficLimit > 0 {
			trafficLimit = realmResp.TrafficLimit
		}
		if vp.ExpireAt != nil {
			expireAt = vp.ExpireAt.Format(time.RFC3339)
		}
	} else {
		// 标准套餐：原逻辑不变——从 otun-manager /api/subscribe 取节点协议配置。
		subscribeReq := &client.SubscribeRequest{
			DeviceID: deviceID,
		}
		config, err := s.otunClient.GetSubscribeConfig(ctx, subscribeReq)
		if err != nil {
			return nil, fmt.Errorf("failed to get VPN config from otun-manager: %w", err)
		}
		for _, p := range config.Protocols {
			protocols = append(protocols, models.VPNProtocol{
				Protocol: p.Protocol,
				URL:      p.URL,
				Node:     p.Node,
			})
		}
		expireAt = config.ExpireAt
		// ★标准面用量取 otun-manager 真源（users.traffic_used，节点上报累加），覆盖
		// vp 的死值——与 residential 分支读 realm 真源同一口径（8ead707 修 residential
		// 时漏了这里）。0 值不覆盖：保持与老 otun 缺字段时的兜底语义一致。
		if config.TrafficUsed > 0 {
			trafficUsed = config.TrafficUsed
		}
		if config.TrafficLimit > 0 {
			trafficLimit = config.TrafficLimit
		}
		// B2（2026-07-08 整改）：standard 面同样透传 exit_country + smart_strategy
		//（rules 模型，manager 按 primary 节点区域生成境外模板）。老 otun 缺字段 → 零值，
		// omitempty 不输出，前端按"无此字段"降级（与升级前一致，部署时序安全）。
		exitCountry = config.ExitCountry
		smartStrategy = config.SmartStrategy
	}

	return &models.VPNSubscribeResponse{
		Status:        "active",
		Channel:       vp.Channel,
		PlanTier:      vp.PlanTier,
		ServiceTier:   vp.ServiceTier,
		// B6（2026-07-08 整改）：subscribe_url 是【下发给客户端】的刷新地址，必须公网可达——
		// 指本服务自己的公网路由 /api/v1/my/vpn/subscribe（portal nginx /api/v1/my/ 已分发，
		// JWT 同 App 现有调用），双面通用。★别再拼 OTunManagerURL：那是内网互调地址
		//（localhost→172.26.7.44 的老坑，otun /api/subscribe 还挂内部鉴权，公网本就不可调）。
		SubscribeURL:  fmt.Sprintf("%s/api/v1/my/vpn/subscribe", s.cfg.Services.PublicBaseURL),
		DeviceID:      deviceID,
		Protocols:     protocols,
		TrafficLimit:  trafficLimit,
		TrafficUsed:   trafficUsed,
		ExpireAt:      expireAt,
		Message:       "VPN configuration retrieved successfully",
		ExitCountry:   exitCountry,
		SmartStrategy: smartStrategy,
		Nodes:         realmNodes, // ★2c：N=2 出口摘要（region 出口级；标准分支 nil→omitempty 不输出）
		// ★Batch 3 + 阶段2：config_version 覆盖 protocols + smart_strategy + regions[] 全量
		//（授权集任何结构性变化必翻转，契约 §2.4）；无 regions 时哈希与现状逐字节一致（零回归）。
		ConfigVersion: computeConfigVersionWithRegions(protocols, smartStrategy, vpnRegions),
		Regions:       vpnRegions, // ★阶段2：授权集区域包（标准分支 nil→omitempty 不输出）
	}, nil
}

// buildVPNRegions 把 otun 下发的授权集区域包映射成 /vpn/all 的 regions[]（契约 §2.1）。
// 每区域 protocols = 该区域各节点 connect_urls 展开（node=role，复用 realmSchemeOf 口径）；
// smart_strategy 原样整包透传（方向性红线：不拆包不混装）。空 → nil（老 otun 零回归）。
func buildVPNRegions(regions []client.RealmRegion) []models.VPNRegion {
	if len(regions) == 0 {
		return nil
	}
	out := make([]models.VPNRegion, 0, len(regions))
	for _, r := range regions {
		protos := make([]models.VPNProtocol, 0, len(r.Nodes)*6)
		nodes := make([]models.RealmNodeSummary, 0, len(r.Nodes))
		for _, n := range r.Nodes {
			role := n.Role
			if role == "" {
				role = "primary"
			}
			nodes = append(nodes, models.RealmNodeSummary{Role: role, Region: n.Region, EgressID: n.EgressID})
			urls := n.ConnectURLs
			if len(urls) == 0 && n.ConnectURL != "" {
				urls = []string{n.ConnectURL} // 单 hy2 兜底（异常形态）
			}
			for _, u := range urls {
				if proto := realmSchemeOf(u); proto != "" {
					protos = append(protos, models.VPNProtocol{Protocol: proto, URL: u, Node: role})
				}
			}
		}
		out = append(out, models.VPNRegion{
			Country:       r.Country,
			State:         r.State,
			IsCurrent:     r.IsCurrent,
			Nodes:         nodes,
			Protocols:     protos,
			SmartStrategy: r.SmartStrategy,
		})
	}
	return out
}

// buildRealmProtocolsN2 把 manager 下发的 N=2 nodes[] 展成 primary6 + backup6（最多 12 条）VPNProtocol。
// ★2c：每个 node 的六协议 URL 各展一条，node 字段 = 该出口的 role（primary/backup），region 带上供展示。
// 客户端选出口（primary/backup urltest 容灾）再选协议。逐级 fail-soft：
//   - nodes 非空 → 展 N=2×六协议；
//   - nodes 空 → 退回 buildRealmProtocols(connectURLs)（单出口六协议，全 primary，2b-1 前的老形态）；
//   - connectURLs 也空 → 单条 fallbackHY2。
func buildRealmProtocolsN2(nodes []client.RealmNode, connectURLs []string, fallbackHY2 string) []models.VPNProtocol {
	if len(nodes) == 0 {
		return buildRealmProtocols(connectURLs, fallbackHY2)
	}
	out := make([]models.VPNProtocol, 0, len(nodes)*6)
	for _, n := range nodes {
		role := n.Role
		if role == "" {
			role = "primary"
		}
		urls := n.ConnectURLs
		if len(urls) == 0 && n.ConnectURL != "" {
			urls = []string{n.ConnectURL} // 该出口只有单 hy2（异常兜底）
		}
		for _, u := range urls {
			proto := realmSchemeOf(u)
			if proto == "" {
				continue
			}
			out = append(out, models.VPNProtocol{Protocol: proto, URL: u, Node: role})
		}
	}
	if len(out) == 0 {
		// 所有 node 的 URL 都无法识别（异常）→ 退回单出口/单 hy2，避免空 protocols。
		return buildRealmProtocols(connectURLs, fallbackHY2)
	}
	return out
}

// buildRealmNodeSummaries 把 otun 下发的 nodes[] 收敛成顶层出口摘要（§2c region 结构）。
// region 挂出口级（每出口 1 次），前端按 protocol.node == role 匹配取 region。
// nodes 空（单出口/老 otun）→ 返 nil（前端不读 nodes 也不坏，零回归）。
func buildRealmNodeSummaries(nodes []client.RealmNode) []models.RealmNodeSummary {
	if len(nodes) == 0 {
		return nil
	}
	out := make([]models.RealmNodeSummary, 0, len(nodes))
	for _, n := range nodes {
		role := n.Role
		if role == "" {
			role = "primary"
		}
		out = append(out, models.RealmNodeSummary{Role: role, Region: n.Region, EgressID: n.EgressID})
	}
	return out
}

// buildRealmProtocols 把 manager 下发的六协议 connect_urls 展成 App-facing 的 VPNProtocol 列表
// （residential 照标准节点范式返回多条 protocols[]，App 用已有多协议选择器选协议）。
//   - protocol 值 = 各 URL 的 scheme（如 hysteria2-realm / reality-realm / ss-realm …），
//     客户端按 <proto>-realm scheme 路由进打洞栈（与现有 residential 单协议契约一致）。
//   - node 全填 "primary"（同一出口的六个协议面都是主节点；egress 信息在各 URL 的 realm_id 里）。
//   - 兜底：connectURLs 为空（旧 manager 未启用六协议）时退回单条 fallbackHY2，保证零回归。
func buildRealmProtocols(connectURLs []string, fallbackHY2 string) []models.VPNProtocol {
	if len(connectURLs) == 0 {
		// 旧 manager：只有单条 hy2 URL。protocol 值沿用历史 "hysteria2-realm"。
		return []models.VPNProtocol{{Protocol: "hysteria2-realm", URL: fallbackHY2, Node: "primary"}}
	}
	out := make([]models.VPNProtocol, 0, len(connectURLs))
	for _, u := range connectURLs {
		proto := realmSchemeOf(u)
		if proto == "" {
			continue // 无法识别 scheme 的 URL 跳过（不塞进无 protocol 的坏项）
		}
		out = append(out, models.VPNProtocol{Protocol: proto, URL: u, Node: "primary"})
	}
	if len(out) == 0 {
		// 全部 URL 都无法识别 scheme（异常）→ 兜底单条 hy2，避免返回空 protocols。
		return []models.VPNProtocol{{Protocol: "hysteria2-realm", URL: fallbackHY2, Node: "primary"}}
	}
	return out
}

// realmSchemeOf 从 <proto>-realm://... 取出 scheme 段作 protocol 值。非 realm URL 返回空串。
func realmSchemeOf(rawURL string) string {
	i := strings.Index(rawURL, "://")
	if i <= 0 {
		return ""
	}
	scheme := rawURL[:i]
	// 只接受 realm 打洞 scheme（<proto>-realm），防误把标准 URL 混入。
	if !strings.HasSuffix(scheme, "-realm") {
		return ""
	}
	return scheme
}

// GetUserVPNQuickStatus returns lightweight VPN status (no protocols)
func (s *VPNService) GetUserVPNQuickStatus(ctx context.Context, userID string) (*models.VPNQuickStatus, error) {
	vp, err := s.vpnRepo.GetCurrentByUser(ctx, userID)
	if err != nil || vp == nil {
		return nil, fmt.Errorf("no active VPN subscription")
	}
	return s.buildQuickStatus(ctx, vp), nil
}

// buildQuickStatus 根据【单条】provision 构造轻量状态（含 otun-manager 实时流量/到期回填）。
// 从 GetUserVPNQuickStatus 抽出，供 GetUserVPNQuickStatusAll 按服务面分别复用。
func (s *VPNService) buildQuickStatus(ctx context.Context, vp *models.VPNProvision) *models.VPNQuickStatus {
	resp := &models.VPNQuickStatus{
		Status:       vp.Status,
		Channel:      vp.Channel,
		PlanTier:     vp.PlanTier,
		TrafficLimit: vp.TrafficLimit,
		TrafficUsed:  vp.TrafficUsed,
	}

	// Get real-time traffic_used and expire_at from otun-manager
	if vp.OtunUUID != nil && *vp.OtunUUID != "" {
		syncResp, err := s.otunClient.SyncUser(ctx, *vp.OtunUUID)
		if err == nil && syncResp != nil {
			resp.ExpireAt = syncResp.ExpireAt
			resp.TrafficUsed = syncResp.TrafficUsed
		}
	}

	return resp
}

// servicePartitions 是两个服务面的分区谓词（false=标准面 basic/premium/standard，
// true=住宅面 residential）。GetUserVPNSubscribeConfigAll / GetUserVPNQuickStatusAll
// 按它分别取该 user 的同分区 current provision。依赖 MULTI_SERVICE_ENABLED=true 才真正分区；
// 开关 false 时分区查询退化为单条，两面取到同一条 → 结果与老单条接口等价（不报错）。
var servicePartitions = []bool{false, true}

// GetUserVPNSubscribeConfigAll 一次返回该 user【所有持有的服务面】的订阅配置（方案 C）。
// 标准面与住宅面各取一条 current provision，分别构造，组成数组。未持有的面不进数组；
// 无任何有效 provision 时返回空数组（非错误）。先校验有 active 订阅（与单条接口一致）。
func (s *VPNService) GetUserVPNSubscribeConfigAll(ctx context.Context, userID string) ([]*models.VPNSubscribeResponse, error) {
	if s.subscriptionClient != nil {
		subStatus, err := s.subscriptionClient.GetUserVPNSubscription(ctx, userID)
		if err != nil {
			log.Printf("[VPNService] Error checking subscription for config-all: %v", err)
			return nil, fmt.Errorf("failed to verify subscription status")
		}
		if subStatus == nil || !subStatus.HasActive {
			return []*models.VPNSubscribeResponse{}, nil
		}
	}

	out := make([]*models.VPNSubscribeResponse, 0, len(servicePartitions))
	for _, isResidential := range servicePartitions {
		vp, err := s.vpnRepo.GetCurrentByUserAndServicePartition(ctx, userID, isResidential)
		if err != nil || vp == nil {
			continue // 该面未持有
		}
		resp, err := s.buildSubscribeResponse(ctx, vp, userID)
		if err != nil {
			// 单面构造失败（如 residential 未分配出口）不应拖垮另一面：记录并跳过。
			log.Printf("[VPNService] build subscribe config failed (user=%s residential=%v): %v", userID, isResidential, err)
			continue
		}
		out = append(out, resp)
	}
	return out, nil
}

// GetUserVPNQuickStatusAll 一次返回该 user【所有持有的服务面】的轻量状态（方案 C）。
// 标准面与住宅面各一条，未持有的面不进数组。
func (s *VPNService) GetUserVPNQuickStatusAll(ctx context.Context, userID string) ([]*models.VPNQuickStatus, error) {
	out := make([]*models.VPNQuickStatus, 0, len(servicePartitions))
	for _, isResidential := range servicePartitions {
		vp, err := s.vpnRepo.GetCurrentByUserAndServicePartition(ctx, userID, isResidential)
		if err != nil || vp == nil {
			continue
		}
		out = append(out, s.buildQuickStatus(ctx, vp))
	}
	return out, nil
}

// UpdateUserEmail 更新用户邮箱（subscription-service 邮箱绑定事件触发）
// 更新 vpn_provisions 表中的 email，并转发给 otun-manager
func (s *VPNService) UpdateUserEmail(ctx context.Context, userID, email string) error {
	log.Printf("[VPNService] Updating user email: user=%s, email=%s", userID, email)

	// 1. 更新 vpn_provisions
	if err := s.vpnRepo.UpdateEmailByUserID(ctx, userID, email); err != nil {
		log.Printf("[VPNService] Failed to update vpn provision email: %v", err)
	}

	// 2. 转发给 otun-manager
	otunUUID, err := s.vpnRepo.GetOtunUUIDByUser(ctx, userID)
	if err != nil || otunUUID == nil || *otunUUID == "" {
		log.Printf("[VPNService] No otun UUID found for user %s, skipping otun-manager update", userID)
		return nil
	}

	if err := s.otunClient.UpdateUser(ctx, *otunUUID, &client.UpdateVPNUserRequest{
		Email: &email,
	}); err != nil {
		log.Printf("[VPNService] Failed to update email in otun-manager for user %s (otun=%s): %v", userID, *otunUUID, err)
	} else {
		log.Printf("[VPNService] Email updated in otun-manager: user=%s, otun=%s", userID, *otunUUID)
	}

	return nil
}

// ==================== Realm 区域选择/切换（BFF 链路，§4.5）====================
//
// user-portal BFF 只认 auth user_id；otun-manager realm 接口只认 otun users.uuid
// （= vpn_provisions.OtunUUID）。这层 service 负责 user_id → otun_uuid 解析后再调
// otun-manager，是这条链路上唯一持有该映射的地方（与 GetUserVPNSubscribeConfig 一致）。
// 业务级失败（client.RealmAPIError）原样上抛，由 handler 透传原始状态码 + error 字符串给前端。

// ErrNoRealmAssignment 表示该用户没有 realm 分配（未订阅 residential / 未开通），
// 对应前端 no_assignment（404）。
var ErrNoRealmAssignment = errors.New("no_assignment")

// resolveResidentialOtunUUID 解析【住宅面】的 otun_uuid。realm/region 能力只对 residential
// 有意义（标准面没有 realm 分配），因此这里【无条件】按住宅分区取 uuid，而不是用
// GetOtunUUIDByUser（取 created_at 最新的任意面）。
//
// 修复 no_assignment 误报：持双面的用户若标准面 provision 后建（如先订 residential 再买
// basic/积分），GetOtunUUIDByUser 会取到标准 uuid → manager egresses 能列全局候选（200），
// 但 select 在 realm_user_assignment 找不到该标准 uuid → no_assignment（404），表现为
// "list 成功但 select 失败" 的自相矛盾。强制住宅分区即可锁定真正有 assignment 的住宅 uuid。
func (s *VPNService) resolveResidentialOtunUUID(ctx context.Context, userID string) (*string, error) {
	return s.vpnRepo.GetOtunUUIDByUserAndServicePartition(ctx, userID, true)
}

// ListRealmEgresses 列出用户可选出口（BFF GET /resources/vpn/regions 的下游）。
// 未开通 residential（无住宅面 otun_uuid）的用户视为无分配：ErrNoRealmAssignment 让 BFF 据此判定。
func (s *VPNService) ListRealmEgresses(ctx context.Context, userID string) (*client.RealmEgressListResponse, error) {
	otunUUID, err := s.resolveResidentialOtunUUID(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("resolve otun uuid: %w", err)
	}
	if otunUUID == nil || *otunUUID == "" {
		return nil, ErrNoRealmAssignment
	}
	return s.otunClient.ListRealmEgresses(ctx, *otunUUID)
}

// SelectRealmEgress 切换用户当前出口（BFF POST /resources/vpn/region 的下游）。
// 业务级失败以 *client.RealmAPIError 返回，handler 据此透传状态码。
func (s *VPNService) SelectRealmEgress(ctx context.Context, userID, egressID string) (*client.RealmSelectResponse, error) {
	otunUUID, err := s.resolveResidentialOtunUUID(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("resolve otun uuid: %w", err)
	}
	if otunUUID == nil || *otunUUID == "" {
		return nil, ErrNoRealmAssignment
	}
	return s.otunClient.SelectRealmEgress(ctx, *otunUUID, egressID)
}

// ListRealmCountries 按国家聚合 online 出口（BFF GET /resources/vpn/countries 的下游，§2c/§7.2）。
func (s *VPNService) ListRealmCountries(ctx context.Context, userID string) (*client.RealmCountriesResponse, error) {
	otunUUID, err := s.resolveResidentialOtunUUID(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("resolve otun uuid: %w", err)
	}
	if otunUUID == nil || *otunUUID == "" {
		return nil, ErrNoRealmAssignment
	}
	return s.otunClient.ListRealmCountries(ctx, *otunUUID)
}

// RealmResponseWithRegions 在 otun 原始响应之上把 regions[] 归一化成契约 §2 形态
//（nodes 摘要 + protocols[] 展开 + smart_strategy 整包），select-country / connect-url 两读口共用，
// 与 /vpn/all 的 regions[]（buildVPNRegions 同源）同构。外层 Regions 与嵌入结构同名字段冲突时
// encoding/json 取最浅层 → otun 原始骨架（nodes[].connect_urls 形态、无 protocols[]）被完全遮蔽。
// 此前把骨架直接透传，前端按契约 §5.2 读 regions[].protocols 恒为 0 → 集内本地秒切存包无料可用
//（2026-07-16 真机坐实）。
type RealmResponseWithRegions struct {
	*client.RealmConnectURLResponse
	Regions []models.VPNRegion `json:"regions,omitempty"`
}

// regionizeRealmResponse 包装 otun 响应，regions[] 过 buildVPNRegions 归一化。
// 老 otun 不下发 regions → buildVPNRegions 返 nil → omitempty 不输出（零回归）。
func regionizeRealmResponse(resp *client.RealmConnectURLResponse) *RealmResponseWithRegions {
	return &RealmResponseWithRegions{
		RealmConnectURLResponse: resp,
		Regions:                 buildVPNRegions(resp.Regions),
	}
}

// SelectRealmCountry 切目的国（BFF POST /resources/vpn/select-country 的下游，§2c/§7.2）。
// 业务级失败(403/409/429)以 *client.RealmAPIError 返回，handler 据此透传状态码。
func (s *VPNService) SelectRealmCountry(ctx context.Context, userID, country string) (*RealmResponseWithRegions, error) {
	otunUUID, err := s.resolveResidentialOtunUUID(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("resolve otun uuid: %w", err)
	}
	if otunUUID == nil || *otunUUID == "" {
		return nil, ErrNoRealmAssignment
	}
	resp, err := s.otunClient.SelectRealmCountry(ctx, *otunUUID, country)
	if err != nil {
		return nil, err
	}
	return regionizeRealmResponse(resp), nil
}

// GetRealmSwitchReady 查目标出口是否已确认应用该用户（BFF GET /resources/vpn/region/status 的下游，
// 切换确认握手 P4）。egressID 可空（manager 缺省取当前 assignment 出口）。
func (s *VPNService) GetRealmSwitchReady(ctx context.Context, userID, egressID string) (*client.RealmReadyResponse, error) {
	otunUUID, err := s.resolveResidentialOtunUUID(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("resolve otun uuid: %w", err)
	}
	if otunUUID == nil || *otunUUID == "" {
		return nil, ErrNoRealmAssignment
	}
	resp, err := s.otunClient.GetRealmUserReady(ctx, *otunUUID, egressID)
	if err != nil {
		return nil, err
	}
	if resp == nil { // manager 404 no_assignment
		return nil, ErrNoRealmAssignment
	}
	return resp, nil
}

// GetRealmConnectURLForUser 取用户当前出口连接 URL（BFF 可选 GET /resources/vpn/connect-url 的下游）。
// 无住宅面 otun_uuid 或无分配（manager 404）→ ErrNoRealmAssignment。
// regions[] 归一化口径与 select-country 一致（RealmResponseWithRegions）。
func (s *VPNService) GetRealmConnectURLForUser(ctx context.Context, userID string) (*RealmResponseWithRegions, error) {
	otunUUID, err := s.resolveResidentialOtunUUID(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("resolve otun uuid: %w", err)
	}
	if otunUUID == nil || *otunUUID == "" {
		return nil, ErrNoRealmAssignment
	}
	resp, err := s.otunClient.GetRealmConnectURL(ctx, *otunUUID)
	if err != nil {
		return nil, err
	}
	if resp == nil { // manager 404 no_assignment → 已就绪但还没分配出口
		return nil, ErrNoRealmAssignment
	}
	return regionizeRealmResponse(resp), nil
}

// Helper functions

// calculateTrafficLimit calculates traffic limit based on plan tier
func (s *VPNService) calculateTrafficLimit(planTier string, override int64) int64 {
	if override > 0 {
		return override
	}

	const GB = int64(1024 * 1024 * 1024)

	switch planTier {
	case "unlimited":
		return 10000 * GB
	case "premium":
		return 500 * GB
	case "standard":
		return 200 * GB
	case "basic":
		return 50 * GB
	case "residential":
		// residential 是多档套餐（100/500/1000GB），流量只能由上游 override 决定
		// （payment → event.TrafficGB → subscription → req.TrafficLimit）。
		// 走到这里说明 override 缺失（如礼包/人工开通漏传 traffic），属于配置错误：
		// 落最小档并告警，避免 default 的 100GB 把高档用户悄悄限流且无任何痕迹。
		log.Printf("[VPNService] WARNING: residential plan with no traffic override; "+
			"falling back to smallest tier 100GB — upstream should pass traffic_gb (planTier=%s)", planTier)
		return 100 * GB
	default:
		return 100 * GB
	}
}

// calculateExpireDays determines expire days based on channel
func (s *VPNService) calculateExpireDays(channel string, requestedDays int) int {
	switch channel {
	case "apple", "google":
		// Subscription-based: platform manages renewal cycle, fixed 30 days
		return 30
	default:
		// Purchase-based (Stripe etc): use requested days
		if requestedDays > 0 {
			return requestedDays
		}
		return 30
	}
}

// calculateExpireAt calculates expiration time from now (for new users)
func (s *VPNService) calculateExpireAt(days int) time.Time {
	if days <= 0 {
		days = 30
	}
	return time.Now().AddDate(0, 0, days)
}

// calculateExpireAtWithStacking queries otun-manager for the user's current expire_at,
// and stacks the new days on top if the subscription hasn't expired yet.
// Used for paid purchases (Stripe etc) to protect the user's remaining time.
func (s *VPNService) calculateExpireAtWithStacking(ctx context.Context, vpnUserID string, days int) time.Time {
	if days <= 0 {
		days = 30
	}

	userInfo, err := s.otunClient.GetUser(ctx, vpnUserID)
	if err != nil {
		log.Printf("[VPNService] Failed to get current user info for stacking, using time.Now(): %v", err)
		return time.Now().AddDate(0, 0, days)
	}

	if userInfo.ExpireAt == "" {
		return time.Now().AddDate(0, 0, days)
	}

	currentExpire, err := time.Parse(time.RFC3339, userInfo.ExpireAt)
	if err != nil {
		log.Printf("[VPNService] Failed to parse expire_at '%s': %v", userInfo.ExpireAt, err)
		return time.Now().AddDate(0, 0, days)
	}

	// If still active, stack new days on top of remaining time
	if currentExpire.After(time.Now()) {
		newExpire := currentExpire.AddDate(0, 0, days)
		log.Printf("[VPNService] Paid purchase stacking: current expires %s + %d days = %s",
			currentExpire.Format(time.RFC3339), days, newExpire.Format(time.RFC3339))
		return newExpire
	}

	// Already expired, start fresh
	return time.Now().AddDate(0, 0, days)
}

// notifyVPNActive notifies subscription-service that VPN is active
func (s *VPNService) notifyVPNActive(ctx context.Context, subscriptionID, resourceID, vpnUserID string) {
	if subscriptionID == "" {
		return
	}
	callback := &models.SubscriptionCallback{
		SubscriptionID: subscriptionID,
		App:            "otun",
		Status:         models.StatusActive,
		Message:        fmt.Sprintf("VPN resource %s is active", resourceID),
	}
	if err := s.subscriptionClient.NotifyResourceStatus(ctx, callback); err != nil {
		log.Printf("[VPNService] Failed to notify subscription-service (active): %v", err)
	}
}

// generateRandomPassword generates a random password of given length
func generateRandomPassword(length int) string {
	bytes := make([]byte, length)
	if _, err := rand.Read(bytes); err != nil {
		return uuid.New().String()[:length]
	}
	return hex.EncodeToString(bytes)[:length]
}
