package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/google/uuid"
	"github.com/wenwu/saas-platform/fulfillment-service/internal/client"
	"github.com/wenwu/saas-platform/fulfillment-service/internal/config"
	"github.com/wenwu/saas-platform/fulfillment-service/internal/models"
	"github.com/wenwu/saas-platform/fulfillment-service/internal/repository"
)

// VPNService handles VPN user provisioning operations
type VPNService struct {
	cfg                *config.Config
	vpnRepo            *repository.VPNProvisionRepository
	logRepo            *repository.LogRepository
	otunClient         *client.OTunClient
	subscriptionClient *client.SubscriptionClient
}

// NewVPNService creates a new VPN service
func NewVPNService(
	cfg *config.Config,
	vpnRepo *repository.VPNProvisionRepository,
	logRepo *repository.LogRepository,
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
	if req.SubscriptionID != "" {
		existingBySubID, _ := s.vpnRepo.GetBySubscriptionID(ctx, req.SubscriptionID)
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
	existing, err := s.vpnRepo.GetCurrentByUserAnyStatus(ctx, req.UserID)
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

		enabled := true
		updateReq := &client.UpdateVPNUserRequest{
			TrafficLimit: trafficLimit,
			ExpireAt:     expireAt.Format(time.RFC3339),
			Enabled:      &enabled,
			ServiceTier:  serviceTier, // 套餐升降级时更新 tier（修复 standard→residential 不变）
		}

		if err := s.otunClient.UpdateUser(ctx, vpnUserID, updateReq); err != nil {
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
	existingOtunUUID, _ := s.vpnRepo.GetOtunUUIDByUser(ctx, req.UserID)

	var actualVPNUserID string

	if existingOtunUUID != nil && *existingOtunUUID != "" {
		// Reuse existing otun_uuid (e.g., trial → purchase conversion)
		actualVPNUserID = *existingOtunUUID
		enabled := true
		updateReq := &client.UpdateVPNUserRequest{
			TrafficLimit: trafficLimit,
			ExpireAt:     expireAt.Format(time.RFC3339),
			Enabled:      &enabled,
			ServiceTier:  serviceTier, // 套餐升降级时更新 tier（修复 standard→residential 不变）
		}
		if err := s.otunClient.UpdateUser(ctx, actualVPNUserID, updateReq); err != nil {
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

// UpdateVPNUser updates a VPN user (extend/upgrade)
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

	// Use auth UUID as device_id
	deviceID := userID

	var protocols []models.VPNProtocol
	var expireAt string

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
		// node 字段的契约是 primary/backup（客户端按它找主节点，见 VPNProtocol.Node 注释
		// 及 REALM_BACKEND_API_SPEC §一）。这里固定 "primary"——residential 当前只下发一条
		// realm URL 作为主节点。egress 信息不丢：已在 URL 的 realm_id + fragment 里。
		// 误填 egress_id（如 realm-cn-sh-01）会让客户端找不到 primary 而报 "No primary node URL found"。
		protocols = append(protocols, models.VPNProtocol{
			Protocol: "hysteria2-realm",
			URL:      realmResp.ConnectURL,
			Node:     "primary",
		})
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
	}

	return &models.VPNSubscribeResponse{
		Status:       "active",
		Channel:      vp.Channel,
		PlanTier:     vp.PlanTier,
		ServiceTier:  vp.ServiceTier,
		SubscribeURL: fmt.Sprintf("%s/api/subscribe", s.cfg.Services.OTunManagerURL),
		DeviceID:     deviceID,
		Protocols:    protocols,
		TrafficLimit: vp.TrafficLimit,
		TrafficUsed:  vp.TrafficUsed,
		ExpireAt:     expireAt,
		Message:      "VPN configuration retrieved successfully",
	}, nil
}

// GetUserVPNQuickStatus returns lightweight VPN status (no protocols)
func (s *VPNService) GetUserVPNQuickStatus(ctx context.Context, userID string) (*models.VPNQuickStatus, error) {
	vp, err := s.vpnRepo.GetCurrentByUser(ctx, userID)
	if err != nil || vp == nil {
		return nil, fmt.Errorf("no active VPN subscription")
	}

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

	return resp, nil
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

// ListRealmEgresses 列出用户可选出口（BFF GET /resources/vpn/regions 的下游）。
// 未开通 otun（无 otun_uuid）的用户视为无分配：返回空列表 + ErrNoRealmAssignment 让 BFF 据此判定。
func (s *VPNService) ListRealmEgresses(ctx context.Context, userID string) (*client.RealmEgressListResponse, error) {
	otunUUID, err := s.vpnRepo.GetOtunUUIDByUser(ctx, userID)
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
	otunUUID, err := s.vpnRepo.GetOtunUUIDByUser(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("resolve otun uuid: %w", err)
	}
	if otunUUID == nil || *otunUUID == "" {
		return nil, ErrNoRealmAssignment
	}
	return s.otunClient.SelectRealmEgress(ctx, *otunUUID, egressID)
}

// GetRealmConnectURLForUser 取用户当前出口连接 URL（BFF 可选 GET /resources/vpn/connect-url 的下游）。
// 无 otun_uuid 或无分配（manager 404）→ ErrNoRealmAssignment。
func (s *VPNService) GetRealmConnectURLForUser(ctx context.Context, userID string) (*client.RealmConnectURLResponse, error) {
	otunUUID, err := s.vpnRepo.GetOtunUUIDByUser(ctx, userID)
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
	return resp, nil
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
