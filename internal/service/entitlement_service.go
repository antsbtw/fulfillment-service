package service

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/google/uuid"
	"github.com/wenwu/saas-platform/fulfillment-service/internal/client"
	"github.com/wenwu/saas-platform/fulfillment-service/internal/config"
	"github.com/wenwu/saas-platform/fulfillment-service/internal/models"
	"github.com/wenwu/saas-platform/fulfillment-service/internal/repository"
)

// EntitlementService handles trial, gift, and other entitlement operations
// Uses VPNProvisionRepository (vpn_provisions table) for all storage
type EntitlementService struct {
	cfg        *config.Config
	vpnRepo    *repository.VPNProvisionRepository
	otunClient *client.OTunClient
}

// NewEntitlementService creates a new entitlement service
func NewEntitlementService(
	cfg *config.Config,
	vpnRepo *repository.VPNProvisionRepository,
	otunClient *client.OTunClient,
) *EntitlementService {
	return &EntitlementService{
		cfg:        cfg,
		vpnRepo:    vpnRepo,
		otunClient: otunClient,
	}
}

// GetTrialConfig returns trial configuration (public, no auth)
func (s *EntitlementService) GetTrialConfig() *models.TrialConfigResponse {
	return &models.TrialConfigResponse{
		Enabled:       s.cfg.Trial.Enabled,
		DurationHours: s.cfg.Trial.DurationHours,
		TrafficGB:     s.cfg.Trial.TrafficGB,
	}
}

// GetTrialStatus checks trial status for a user (JWT auth required)
func (s *EntitlementService) GetTrialStatus(ctx context.Context, userID string) (*models.TrialStatusResponse, error) {
	// Query vpn_provisions for this user's trial
	vp, err := s.vpnRepo.GetByUserAndBusinessType(ctx, userID, models.BusinessTypeTrial)
	if err != nil {
		// No trial record found
		return &models.TrialStatusResponse{
			TrialAvailable: s.cfg.Trial.Enabled,
			TrialUsed:      false,
			ExistingTrial:  nil,
		}, nil
	}

	resp := &models.TrialStatusResponse{
		TrialAvailable: false,
		TrialUsed:      true,
	}

	// If otun_uuid is set, sync from otun-manager
	if vp.OtunUUID != nil && *vp.OtunUUID != "" {
		syncResp, err := s.otunClient.SyncUser(ctx, *vp.OtunUUID)
		if err != nil {
			log.Printf("[EntitlementService] Failed to sync from otun-manager for %s: %v", *vp.OtunUUID, err)
			resp.ExistingTrial = s.buildTrialInfoFromProvision(vp)
			return resp, nil
		}

		// Update local traffic_used
		if syncResp.TrafficUsed != vp.TrafficUsed {
			_ = s.vpnRepo.UpdateTrafficUsed(ctx, vp.ID, syncResp.TrafficUsed)
		}

		// Check expiration
		expired := repository.IsVPNExpired(vp)
		if vp.TrafficLimit > 0 && syncResp.TrafficUsed >= vp.TrafficLimit {
			expired = true
		}

		var protocols []models.TrialProtocol
		for _, p := range syncResp.Protocols {
			protocols = append(protocols, models.TrialProtocol{
				Protocol: p.Protocol,
				URL:      p.URL,
				Node:     p.Node,
			})
		}

		expireAtStr := ""
		if vp.ExpireAt != nil {
			expireAtStr = vp.ExpireAt.Format(time.RFC3339)
		}

		resp.ExistingTrial = &models.TrialAccountInfo{
			UUID:         syncResp.UUID,
			TrafficLimit: vp.TrafficLimit,
			TrafficUsed:  syncResp.TrafficUsed,
			ExpireAt:     expireAtStr,
			Enabled:      syncResp.Enabled,
			Expired:      expired,
			Protocols:    protocols,
		}
	} else {
		resp.ExistingTrial = s.buildTrialInfoFromProvision(vp)
	}

	return resp, nil
}

// ActivateTrial activates a trial for a user (JWT auth required)
func (s *EntitlementService) ActivateTrial(ctx context.Context, userID, email, deviceID string) (*models.ActivateTrialResponse, error) {
	if !s.cfg.Trial.Enabled {
		return nil, fmt.Errorf("trial is not available")
	}

	// 1. Check if user already has a trial
	existing, err := s.vpnRepo.GetByUserAndBusinessType(ctx, userID, models.BusinessTypeTrial)
	if err == nil && existing != nil {
		return nil, fmt.Errorf("trial already used")
	}

	// 1.5. Check email abuse
	if email != "" {
		emailUsed, err := s.vpnRepo.ExistsTrialByEmail(ctx, email)
		if err != nil {
			log.Printf("[EntitlementService] Warning: email check failed: %v", err)
		} else if emailUsed {
			return nil, fmt.Errorf("trial already used")
		}
	}

	// 1.6. Check device abuse
	deviceUsed, err := s.vpnRepo.ExistsTrialByDeviceID(ctx, deviceID)
	if err != nil {
		log.Printf("[EntitlementService] Warning: device_id check failed: %v", err)
	} else if deviceUsed {
		return nil, fmt.Errorf("trial already used")
	}

	// 2. Check if user has active purchase
	activePurchase, err := s.vpnRepo.GetCurrentByUser(ctx, userID)
	if err == nil && activePurchase != nil && activePurchase.BusinessType != models.BusinessTypeTrial {
		return nil, fmt.Errorf("user already has an active subscription")
	}

	// 3. Calculate trial parameters
	const GB = int64(1024 * 1024 * 1024)
	trafficLimit := int64(s.cfg.Trial.TrafficGB) * GB
	expireAt := time.Now().Add(time.Duration(s.cfg.Trial.DurationHours) * time.Hour)

	// 4. Check if user already has an otun_uuid from a previous provision
	existingOtunUUID, _ := s.vpnRepo.GetOtunUUIDByUser(ctx, userID)

	var otunUUID string
	var syncResp *client.SubscribeResponse

	if existingOtunUUID != nil && *existingOtunUUID != "" {
		// Reuse existing VPN account
		otunUUID = *existingOtunUUID
		enabled := true
		updateReq := &client.UpdateVPNUserRequest{
			TrafficLimit: trafficLimit,
			ExpireAt:     expireAt.Format(time.RFC3339),
			Enabled:      &enabled,
		}
		if err := s.otunClient.UpdateUser(ctx, otunUUID, updateReq); err != nil {
			return nil, fmt.Errorf("failed to update VPN user: %w", err)
		}

		syncResp, err = s.otunClient.SyncUser(ctx, otunUUID)
		if err != nil {
			log.Printf("[EntitlementService] Warning: sync after update failed: %v", err)
		}
	} else {
		// Create new VPN user
		vpnUserID := uuid.New().String()
		ssPassword := generateRandomPassword(16)

		createReq := &client.CreateVPNUserRequest{
			UUID:         vpnUserID,
			Email:        email,
			AuthUserID:   userID,
			Protocols:    []string{"vless", "shadowsocks"},
			SSPassword:   ssPassword,
			TrafficLimit: trafficLimit,
			ExpireAt:     expireAt.Format(time.RFC3339),
			ServiceTier:  models.ServiceTierStandard,
		}

		createResp, err := s.otunClient.CreateUser(ctx, createReq)
		if err != nil {
			return nil, fmt.Errorf("failed to create VPN user: %w", err)
		}
		otunUUID = createResp.UUID

		syncResp, err = s.otunClient.SyncUser(ctx, otunUUID)
		if err != nil {
			log.Printf("[EntitlementService] Warning: sync after create failed: %v", err)
		}
	}

	// 5. Insert VPN provision record (business_type=trial)
	provisionID := uuid.New().String()
	vp := &models.VPNProvision{
		ID:           provisionID,
		UserID:       userID,
		BusinessType: models.BusinessTypeTrial,
		ServiceTier:  models.ServiceTierStandard,
		OtunUUID:     &otunUUID,
		Status:       models.VPNProvisionStatusActive,
		TrafficLimit: trafficLimit,
		TrafficUsed:  0,
		ExpireAt:     &expireAt,
		Email:        email,
		DeviceID:     deviceID,
		GrantedBy:    "system",
		IsCurrent:    true,
	}

	if err := s.vpnRepo.Create(ctx, vp); err != nil {
		return nil, fmt.Errorf("failed to save vpn provision: %w", err)
	}

	// 6. Build response
	var protocols []models.TrialProtocol
	if syncResp != nil {
		for _, p := range syncResp.Protocols {
			protocols = append(protocols, models.TrialProtocol{
				Protocol: p.Protocol,
				URL:      p.URL,
				Node:     p.Node,
			})
		}
	}

	log.Printf("[EntitlementService] Trial activated: user=%s, otun_uuid=%s", userID, otunUUID)

	return &models.ActivateTrialResponse{
		UUID:         otunUUID,
		IsTrial:      true,
		TrafficLimit: trafficLimit,
		TrafficUsed:  0,
		ExpireAt:     expireAt.Format(time.RFC3339),
		Enabled:      true,
		Protocols:    protocols,
	}, nil
}

// GiftEntitlement creates a gift entitlement for a user (admin/internal)
func (s *EntitlementService) GiftEntitlement(ctx context.Context, req *models.GiftEntitlementRequest) (*models.GiftEntitlementResponse, error) {
	const GB = int64(1024 * 1024 * 1024)
	trafficLimit := int64(req.TrafficGB) * GB
	expireAt := time.Now().AddDate(0, 0, req.DurationDays)
	serviceTier := req.ServiceTier
	if serviceTier == "" {
		serviceTier = models.ServiceTierStandard
	}

	// 1. Check if user already has an otun_uuid
	existingOtunUUID, _ := s.vpnRepo.GetOtunUUIDByUser(ctx, req.UserID)

	var otunUUID string
	var syncResp *client.SubscribeResponse

	if existingOtunUUID != nil && *existingOtunUUID != "" {
		otunUUID = *existingOtunUUID
		enabled := true
		updateReq := &client.UpdateVPNUserRequest{
			TrafficLimit: trafficLimit,
			ExpireAt:     expireAt.Format(time.RFC3339),
			Enabled:      &enabled,
		}
		if err := s.otunClient.UpdateUser(ctx, otunUUID, updateReq); err != nil {
			return nil, fmt.Errorf("failed to update VPN user: %w", err)
		}

		syncResp, _ = s.otunClient.SyncUser(ctx, otunUUID)
	} else {
		vpnUserID := uuid.New().String()
		ssPassword := generateRandomPassword(16)

		createReq := &client.CreateVPNUserRequest{
			UUID:         vpnUserID,
			Email:        req.Email,
			AuthUserID:   req.UserID,
			Protocols:    []string{"vless", "shadowsocks"},
			SSPassword:   ssPassword,
			TrafficLimit: trafficLimit,
			ExpireAt:     expireAt.Format(time.RFC3339),
			ServiceTier:  serviceTier,
		}

		createResp, err := s.otunClient.CreateUser(ctx, createReq)
		if err != nil {
			return nil, fmt.Errorf("failed to create VPN user: %w", err)
		}
		otunUUID = createResp.UUID

		syncResp, _ = s.otunClient.SyncUser(ctx, otunUUID)
	}

	// 2. Insert VPN provision record (business_type=gift)
	provisionID := uuid.New().String()
	vp := &models.VPNProvision{
		ID:           provisionID,
		UserID:       req.UserID,
		BusinessType: models.BusinessTypeGift,
		ServiceTier:  serviceTier,
		OtunUUID:     &otunUUID,
		Status:       models.VPNProvisionStatusActive,
		TrafficLimit: trafficLimit,
		TrafficUsed:  0,
		ExpireAt:     &expireAt,
		Email:        req.Email,
		GrantedBy:    "admin",
		Note:         req.Note,
		IsCurrent:    true,
	}

	if err := s.vpnRepo.Create(ctx, vp); err != nil {
		return nil, fmt.Errorf("failed to save vpn provision: %w", err)
	}

	// 3. Build response
	var protocols []models.TrialProtocol
	if syncResp != nil {
		for _, p := range syncResp.Protocols {
			protocols = append(protocols, models.TrialProtocol{
				Protocol: p.Protocol,
				URL:      p.URL,
				Node:     p.Node,
			})
		}
	}

	log.Printf("[EntitlementService] Gift entitlement created: user=%s, otun_uuid=%s, traffic_gb=%d, days=%d",
		req.UserID, otunUUID, req.TrafficGB, req.DurationDays)

	return &models.GiftEntitlementResponse{
		EntitlementID: provisionID,
		OtunUUID:      otunUUID,
		TrafficLimit:  trafficLimit,
		ExpireAt:      expireAt.Format(time.RFC3339),
		Protocols:     protocols,
	}, nil
}

// ListEntitlements lists vpn provisions with optional filters (admin/internal)
func (s *EntitlementService) ListEntitlements(ctx context.Context, userID, businessType, status string) ([]*models.EntitlementInfo, error) {
	provisions, err := s.vpnRepo.ListByFilters(ctx, userID, businessType, status)
	if err != nil {
		return nil, fmt.Errorf("list entitlements: %w", err)
	}

	var results []*models.EntitlementInfo
	for _, vp := range provisions {
		info := &models.EntitlementInfo{
			ID:           vp.ID,
			UserID:       vp.UserID,
			Email:        vp.Email,
			OtunUUID:     vp.OtunUUID,
			BusinessType: vp.BusinessType,
			Status:       vp.Status,
			TrafficLimit: vp.TrafficLimit,
			TrafficUsed:  vp.TrafficUsed,
			ServiceTier:  vp.ServiceTier,
			GrantedBy:    vp.GrantedBy,
			Note:         vp.Note,
			DeviceID:     vp.DeviceID,
			CreatedAt:    vp.CreatedAt.Format(time.RFC3339),
			UpdatedAt:    vp.UpdatedAt.Format(time.RFC3339),
		}
		if vp.ExpireAt != nil {
			expireStr := vp.ExpireAt.Format(time.RFC3339)
			info.ExpireAt = &expireStr
		}
		results = append(results, info)
	}

	return results, nil
}

// buildTrialInfoFromProvision builds a TrialAccountInfo from a local vpn provision record
func (s *EntitlementService) buildTrialInfoFromProvision(vp *models.VPNProvision) *models.TrialAccountInfo {
	otunUUID := ""
	if vp.OtunUUID != nil {
		otunUUID = *vp.OtunUUID
	}
	expireAtStr := ""
	if vp.ExpireAt != nil {
		expireAtStr = vp.ExpireAt.Format(time.RFC3339)
	}
	expired := repository.IsVPNExpired(vp)
	enabled := vp.Status == models.VPNProvisionStatusActive && !expired

	return &models.TrialAccountInfo{
		UUID:         otunUUID,
		TrafficLimit: vp.TrafficLimit,
		TrafficUsed:  vp.TrafficUsed,
		ExpireAt:     expireAtStr,
		Enabled:      enabled,
		Expired:      expired,
		Protocols:    nil,
	}
}
