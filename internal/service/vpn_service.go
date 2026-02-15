package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
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
	resourceRepo       *repository.ResourceRepository
	logRepo            *repository.LogRepository
	otunClient         *client.OTunClient
	subscriptionClient *client.SubscriptionClient
}

// NewVPNService creates a new VPN service
func NewVPNService(
	cfg *config.Config,
	resourceRepo *repository.ResourceRepository,
	logRepo *repository.LogRepository,
	otunClient *client.OTunClient,
	subscriptionClient *client.SubscriptionClient,
) *VPNService {
	return &VPNService{
		cfg:                cfg,
		resourceRepo:       resourceRepo,
		logRepo:            logRepo,
		otunClient:         otunClient,
		subscriptionClient: subscriptionClient,
	}
}

// ProvisionVPNUser creates a new VPN user in otun-manager
func (s *VPNService) ProvisionVPNUser(ctx context.Context, req *models.ProvisionRequest) (*models.ProvisionResponse, error) {
	// 日志脱敏: 仅记录必要的非敏感信息
	log.Printf("[VPNService] Provisioning VPN user for subscription=%s, plan=%s",
		req.SubscriptionID, req.PlanTier)

	// 1. Check if user already has an active VPN resource
	existing, err := s.resourceRepo.GetActiveByUserAndType(ctx, req.UserID, models.ResourceTypeVPNUser)
	if err == nil && existing != nil {
		// Already exists, return existing resource
		vpnUserID := ""
		if existing.InstanceID != nil {
			vpnUserID = *existing.InstanceID
		}
		return &models.ProvisionResponse{
			ResourceID: existing.ID,
			Status:     existing.Status,
			VPNUserID:  vpnUserID,
			Message:    "VPN user already exists",
		}, nil
	}

	// 2. Calculate traffic limit and expire time
	trafficLimit := s.calculateTrafficLimit(req.PlanTier, req.TrafficLimit)
	expireAt := s.calculateExpireAt(req.ExpireDays)

	// 3. Generate VPN user credentials
	vpnUserID := uuid.New().String()
	ssPassword := generateRandomPassword(16)

	// 4. Create user in otun-manager
	otunReq := &client.CreateVPNUserRequest{
		UUID:         vpnUserID,
		Email:        req.UserEmail,
		ExternalID:   req.UserID, // Platform user ID as external ID
		Protocols:    []string{"vless", "shadowsocks"},
		SSPassword:   ssPassword,
		TrafficLimit: trafficLimit,
		ExpireAt:     expireAt.Format(time.RFC3339),
		ServiceTier:  req.PlanTier, // Pass plan tier for node assignment
	}

	otunResp, err := s.otunClient.CreateUser(ctx, otunReq)
	if err != nil {
		s.logRepo.LogAction(ctx, "", "vpn_user_create_failed", "failed", err.Error())
		return nil, fmt.Errorf("failed to create VPN user in otun-manager: %w", err)
	}

	// 5. Create local resource record
	resourceID := uuid.New().String()
	now := time.Now()
	resource := &models.Resource{
		ID:             resourceID,
		SubscriptionID: req.SubscriptionID,
		UserID:         req.UserID,
		ResourceType:   models.ResourceTypeVPNUser,
		Provider:       "otun",
		Region:         "global", // VPN users are not region-specific
		Status:         models.StatusActive,
		PlanTier:       req.PlanTier,
		TrafficLimit:   trafficLimit,
		TrafficUsed:    0,
		InstanceID:     &vpnUserID,  // otun-manager user UUID
		APIKey:         &ssPassword, // SS password
		ReadyAt:        &now,
	}

	if err := s.resourceRepo.Create(ctx, resource); err != nil {
		// Rollback: delete user in otun-manager
		_ = s.otunClient.DeleteUser(ctx, vpnUserID)
		return nil, fmt.Errorf("failed to save resource: %w", err)
	}

	// 6. Log action
	s.logRepo.LogActionWithMetadata(ctx, resourceID, "vpn_user_created", "active",
		"VPN user created successfully",
		map[string]interface{}{
			"vpn_user_id":   vpnUserID,
			"plan_tier":     req.PlanTier,
			"traffic_limit": trafficLimit,
			"expire_at":     expireAt.Format(time.RFC3339),
		})

	// 7. Notify subscription-service
	s.notifyVPNActive(ctx, req.SubscriptionID, resourceID, vpnUserID)

	log.Printf("[VPNService] VPN user created successfully: resource=%s, vpn_user=%s", resourceID, otunResp.UUID)

	return &models.ProvisionResponse{
		ResourceID: resourceID,
		Status:     models.StatusActive,
		VPNUserID:  vpnUserID,
		Message:    "VPN user created successfully",
	}, nil
}

// DeprovisionVPNUser disables a VPN user
func (s *VPNService) DeprovisionVPNUser(ctx context.Context, resource *models.Resource, reason string) error {
	log.Printf("[VPNService] Deprovisioning VPN user: resource=%s, reason=%s", resource.ID, reason)

	// 1. Disable user in otun-manager
	if resource.InstanceID != nil && *resource.InstanceID != "" {
		if err := s.otunClient.DisableUser(ctx, *resource.InstanceID); err != nil {
			log.Printf("[VPNService] Warning: failed to disable VPN user in otun-manager: %v", err)
			// Continue execution, don't block the flow
		}
	}

	// 2. Update local status
	now := time.Now()
	resource.Status = models.StatusDeleted
	resource.DeletedAt = &now
	if err := s.resourceRepo.Update(ctx, resource); err != nil {
		return fmt.Errorf("failed to update resource: %w", err)
	}

	// 3. Log action
	s.logRepo.LogAction(ctx, resource.ID, "vpn_user_deprovisioned", "deleted", reason)

	// 4. Notify subscription-service
	if err := s.subscriptionClient.NotifyDeleted(ctx, resource.SubscriptionID, resource.ID); err != nil {
		log.Printf("[VPNService] Failed to notify subscription-service (deleted): %v", err)
	}

	log.Printf("[VPNService] VPN user deprovisioned successfully: %s", resource.ID)
	return nil
}

// UpdateVPNUser updates a VPN user (extend/upgrade)
func (s *VPNService) UpdateVPNUser(ctx context.Context, resourceID string, req *models.UpdateVPNUserRequest) error {
	log.Printf("[VPNService] Updating VPN user: resource=%s", resourceID)

	resource, err := s.resourceRepo.GetByID(ctx, resourceID)
	if err != nil {
		return fmt.Errorf("resource not found: %w", err)
	}

	if resource.InstanceID == nil || *resource.InstanceID == "" {
		return fmt.Errorf("VPN user ID not found in resource")
	}

	// Get current user info from otun-manager
	userInfo, err := s.otunClient.GetUser(ctx, *resource.InstanceID)
	if err != nil {
		return fmt.Errorf("failed to get VPN user from otun-manager: %w", err)
	}

	// Build update request
	updateReq := &client.UpdateVPNUserRequest{}
	needUpdate := false

	// Update traffic limit
	if req.TrafficLimit > 0 {
		updateReq.TrafficLimit = req.TrafficLimit
		resource.TrafficLimit = req.TrafficLimit
		needUpdate = true
	}

	// Extend expiration
	if req.ExtendDays > 0 {
		currentExpire, _ := time.Parse(time.RFC3339, userInfo.ExpireAt)
		if currentExpire.Before(time.Now()) {
			currentExpire = time.Now()
		}
		newExpire := currentExpire.AddDate(0, 0, req.ExtendDays)
		updateReq.ExpireAt = newExpire.Format(time.RFC3339)
		needUpdate = true
	}

	// Update plan tier
	if req.PlanTier != "" && req.PlanTier != resource.PlanTier {
		resource.PlanTier = req.PlanTier
		// Recalculate traffic limit based on new tier if not explicitly set
		if req.TrafficLimit == 0 {
			newLimit := s.calculateTrafficLimit(req.PlanTier, 0)
			updateReq.TrafficLimit = newLimit
			resource.TrafficLimit = newLimit
		}
		needUpdate = true
	}

	if !needUpdate {
		return nil
	}

	// Update in otun-manager
	if err := s.otunClient.UpdateUser(ctx, *resource.InstanceID, updateReq); err != nil {
		return fmt.Errorf("failed to update VPN user in otun-manager: %w", err)
	}

	// Update local resource
	if err := s.resourceRepo.Update(ctx, resource); err != nil {
		return fmt.Errorf("failed to update resource: %w", err)
	}

	s.logRepo.LogActionWithMetadata(ctx, resourceID, "vpn_user_updated", "active",
		"VPN user updated",
		map[string]interface{}{
			"traffic_limit": resource.TrafficLimit,
			"plan_tier":     resource.PlanTier,
			"extend_days":   req.ExtendDays,
		})

	log.Printf("[VPNService] VPN user updated successfully: %s", resourceID)
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

	// 2. Check VPN resource
	resource, _ := s.resourceRepo.GetActiveByUserAndType(ctx, userID, models.ResourceTypeVPNUser)

	// 3. Build response
	resp := &models.VPNStatusResponse{}

	// No subscription
	if subStatus == nil || !subStatus.HasActive {
		resp.VPNStatus = models.VPNStatusNoSubscription
		resp.HasSubscription = false
		resp.HasVPNUser = false
		resp.Message = "No active VPN subscription. Please subscribe to use VPN."
		return resp, nil
	}

	// Has subscription
	resp.HasSubscription = true
	resp.Subscription = &models.SubscriptionInfo{
		SubscriptionID: subStatus.SubscriptionID,
		Status:         subStatus.Status,
		PlanTier:       subStatus.PlanTier,
		ExpiresAt:      subStatus.ExpiresAt,
		AutoRenew:      subStatus.AutoRenew,
	}

	// No VPN user
	if resource == nil {
		resp.VPNStatus = models.VPNStatusExpired
		resp.HasVPNUser = false
		resp.Message = "VPN subscription active but no VPN user found. Please contact support."
		return resp, nil
	}

	// Has VPN user
	resp.HasVPNUser = true

	trafficLimitGB := float64(resource.TrafficLimit) / (1024 * 1024 * 1024)
	trafficUsedGB := float64(resource.TrafficUsed) / (1024 * 1024 * 1024)
	trafficPercent := 0.0
	if resource.TrafficLimit > 0 {
		trafficPercent = (float64(resource.TrafficUsed) / float64(resource.TrafficLimit)) * 100
	}

	vpnUserID := ""
	if resource.InstanceID != nil {
		vpnUserID = *resource.InstanceID
	}

	resp.VPNUser = &models.VPNUserInfo{
		ResourceID:     resource.ID,
		VPNUserID:      vpnUserID,
		Status:         resource.Status,
		PlanTier:       resource.PlanTier,
		TrafficLimitGB: trafficLimitGB,
		TrafficUsedGB:  trafficUsedGB,
		TrafficPercent: trafficPercent,
		CreatedAt:      resource.CreatedAt.Format(time.RFC3339),
	}

	switch resource.Status {
	case models.StatusActive:
		resp.VPNStatus = models.VPNStatusActive
		resp.Message = "VPN is active and ready to use."
	case models.StatusDeleted:
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
	// 1. Verify user has active VPN resource
	resource, err := s.resourceRepo.GetActiveByUserAndType(ctx, userID, models.ResourceTypeVPNUser)
	if err != nil || resource == nil {
		return nil, fmt.Errorf("no active VPN subscription")
	}

	// 2. Get subscription config from otun-manager
	// Use the VPN user UUID (instance_id) as device_id for otun-manager lookup
	if resource.InstanceID == nil || *resource.InstanceID == "" {
		return nil, fmt.Errorf("VPN resource has no instance_id (vpn_user_uuid)")
	}
	deviceID := *resource.InstanceID
	trafficGB := int(resource.TrafficLimit / (1024 * 1024 * 1024))
	if trafficGB < 1 {
		trafficGB = 100 // default
	}

	subscribeReq := &client.SubscribeRequest{
		DeviceID:  deviceID,
		TrafficGB: trafficGB,
		DaysValid: 30,
	}

	config, err := s.otunClient.GetSubscribeConfig(ctx, subscribeReq)
	if err != nil {
		return nil, fmt.Errorf("failed to get VPN config from otun-manager: %w", err)
	}

	// 3. Build response
	var protocols []models.VPNProtocol
	for _, p := range config.Protocols {
		protocols = append(protocols, models.VPNProtocol{
			Protocol: p.Protocol,
			URL:      p.URL,
			Node:     p.Node,
		})
	}

	return &models.VPNSubscribeResponse{
		SubscribeURL: fmt.Sprintf("%s/api/subscribe", s.cfg.Services.OTunManagerURL),
		DeviceID:     deviceID,
		Protocols:    protocols,
		ExpireAt:     config.ExpireAt,
		Message:      "VPN configuration retrieved successfully",
	}, nil
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
		return 10000 * GB // 10TB, effectively unlimited
	case "premium":
		return 500 * GB // 500GB
	case "standard":
		return 200 * GB // 200GB
	case "basic":
		return 50 * GB // 50GB
	default:
		return 100 * GB // Default 100GB
	}
}

// calculateExpireAt calculates expiration time
func (s *VPNService) calculateExpireAt(days int) time.Time {
	if days <= 0 {
		days = 30 // Default 30 days
	}
	return time.Now().AddDate(0, 0, days)
}

// notifyVPNActive notifies subscription-service that VPN is active
func (s *VPNService) notifyVPNActive(ctx context.Context, subscriptionID, resourceID, vpnUserID string) {
	callback := &models.SubscriptionCallback{
		SubscriptionID: subscriptionID,
		ResourceID:     resourceID,
		Status:         models.StatusActive,
	}
	if err := s.subscriptionClient.NotifyResourceStatus(ctx, callback); err != nil {
		log.Printf("[VPNService] Failed to notify subscription-service (active): %v", err)
	}
}

// generateRandomPassword generates a random password of given length
func generateRandomPassword(length int) string {
	bytes := make([]byte, length)
	if _, err := rand.Read(bytes); err != nil {
		// Fallback to UUID if crypto/rand fails
		return uuid.New().String()[:length]
	}
	return hex.EncodeToString(bytes)[:length]
}
