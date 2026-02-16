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
type EntitlementService struct {
	cfg             *config.Config
	entitlementRepo *repository.EntitlementRepository
	otunClient      *client.OTunClient
}

// NewEntitlementService creates a new entitlement service
func NewEntitlementService(
	cfg *config.Config,
	entitlementRepo *repository.EntitlementRepository,
	otunClient *client.OTunClient,
) *EntitlementService {
	return &EntitlementService{
		cfg:             cfg,
		entitlementRepo: entitlementRepo,
		otunClient:      otunClient,
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
	// 1. Query entitlements table for this user's trial
	entitlement, err := s.entitlementRepo.GetByUserIDAndSource(ctx, userID, models.EntitlementSourceTrial)
	if err != nil {
		// No trial record found
		return &models.TrialStatusResponse{
			TrialAvailable: s.cfg.Trial.Enabled,
			TrialUsed:      false,
			ExistingTrial:  nil,
		}, nil
	}

	// 2. Trial record exists
	resp := &models.TrialStatusResponse{
		TrialAvailable: false,
		TrialUsed:      true,
	}

	// 3. If otun_uuid is set, sync from otun-manager to get current status and protocols
	if entitlement.OtunUUID != nil && *entitlement.OtunUUID != "" {
		syncResp, err := s.otunClient.SyncUser(ctx, *entitlement.OtunUUID)
		if err != nil {
			log.Printf("[EntitlementService] Failed to sync from otun-manager for %s: %v", *entitlement.OtunUUID, err)
			// Return basic info from local record
			resp.ExistingTrial = s.buildTrialInfoFromEntitlement(entitlement)
			return resp, nil
		}

		// Update local traffic_used from otun-manager
		if syncResp.TrafficUsed != entitlement.TrafficUsed {
			_ = s.entitlementRepo.UpdateTrafficUsed(ctx, entitlement.ID, syncResp.TrafficUsed)
		}

		// Check expiration
		expired := repository.IsExpired(entitlement)
		if entitlement.ExpireAt != nil && time.Now().After(*entitlement.ExpireAt) {
			expired = true
		}
		if entitlement.TrafficLimit > 0 && syncResp.TrafficUsed >= entitlement.TrafficLimit {
			expired = true
		}

		// Build protocols
		var protocols []models.TrialProtocol
		for _, p := range syncResp.Protocols {
			protocols = append(protocols, models.TrialProtocol{
				Protocol: p.Protocol,
				URL:      p.URL,
				Node:     p.Node,
			})
		}

		expireAtStr := ""
		if entitlement.ExpireAt != nil {
			expireAtStr = entitlement.ExpireAt.Format(time.RFC3339)
		}

		resp.ExistingTrial = &models.TrialAccountInfo{
			UUID:         syncResp.UUID,
			TrafficLimit: entitlement.TrafficLimit,
			TrafficUsed:  syncResp.TrafficUsed,
			ExpireAt:     expireAtStr,
			Enabled:      syncResp.Enabled,
			Expired:      expired,
			Protocols:    protocols,
		}
	} else {
		// otun_uuid not set (shouldn't happen normally, but handle gracefully)
		resp.ExistingTrial = s.buildTrialInfoFromEntitlement(entitlement)
	}

	return resp, nil
}

// ActivateTrial activates a trial for a user (JWT auth required)
func (s *EntitlementService) ActivateTrial(ctx context.Context, userID, email, deviceID string) (*models.ActivateTrialResponse, error) {
	if !s.cfg.Trial.Enabled {
		return nil, fmt.Errorf("trial is not available")
	}

	// 1. Check if user already has a trial -> 409 Conflict
	existing, err := s.entitlementRepo.GetByUserIDAndSource(ctx, userID, models.EntitlementSourceTrial)
	if err == nil && existing != nil {
		return nil, fmt.Errorf("trial already used")
	}

	// 1.5. Check if email already used a trial (prevent abuse via delete & re-register)
	if email != "" {
		emailUsed, err := s.entitlementRepo.ExistsTrialByEmail(ctx, email)
		if err != nil {
			log.Printf("[EntitlementService] Warning: email check failed: %v", err)
		} else if emailUsed {
			return nil, fmt.Errorf("trial already used")
		}
	}

	// 1.6. Check if device already used a trial (prevent abuse via new account on same device)
	deviceUsed, err := s.entitlementRepo.ExistsTrialByDeviceID(ctx, deviceID)
	if err != nil {
		log.Printf("[EntitlementService] Warning: device_id check failed: %v", err)
	} else if deviceUsed {
		return nil, fmt.Errorf("trial already used")
	}

	// 2. Check if user has active purchase -> 403 Already subscribed
	activePurchase, err := s.entitlementRepo.GetActiveByUserIDAndSource(ctx, userID, models.EntitlementSourcePurchase)
	if err == nil && activePurchase != nil {
		return nil, fmt.Errorf("user already has an active subscription")
	}

	// 3. Calculate trial parameters
	const GB = int64(1024 * 1024 * 1024)
	trafficLimit := int64(s.cfg.Trial.TrafficGB) * GB
	expireAt := time.Now().Add(time.Duration(s.cfg.Trial.DurationHours) * time.Hour)

	// 4. Check if user already has an otun_uuid from a previous entitlement
	existingOtunUUID, _ := s.entitlementRepo.GetOtunUUIDByUserID(ctx, userID)

	var otunUUID string
	var syncResp *client.SubscribeResponse

	if existingOtunUUID != nil && *existingOtunUUID != "" {
		// User already has a VPN account, update it
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

		// Get protocols via sync
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
			ServiceTier:  "standard",
		}

		createResp, err := s.otunClient.CreateUser(ctx, createReq)
		if err != nil {
			return nil, fmt.Errorf("failed to create VPN user: %w", err)
		}
		otunUUID = createResp.UUID

		// Get protocols via sync
		syncResp, err = s.otunClient.SyncUser(ctx, otunUUID)
		if err != nil {
			log.Printf("[EntitlementService] Warning: sync after create failed: %v", err)
		}
	}

	// 5. Insert entitlement record
	entitlementID := uuid.New().String()
	entitlement := &models.Entitlement{
		ID:           entitlementID,
		UserID:       userID,
		Email:        email,
		OtunUUID:     &otunUUID,
		Source:       models.EntitlementSourceTrial,
		Status:       models.EntitlementStatusActive,
		TrafficLimit: trafficLimit,
		TrafficUsed:  0,
		ExpireAt:     &expireAt,
		ServiceTier:  "standard",
		GrantedBy:    "system",
		Note:         "",
		DeviceID:     deviceID,
	}

	if err := s.entitlementRepo.Create(ctx, entitlement); err != nil {
		return nil, fmt.Errorf("failed to save entitlement: %w", err)
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
		serviceTier = "standard"
	}

	// 1. Check if user already has an otun_uuid
	existingOtunUUID, _ := s.entitlementRepo.GetOtunUUIDByUserID(ctx, req.UserID)

	var otunUUID string
	var syncResp *client.SubscribeResponse

	if existingOtunUUID != nil && *existingOtunUUID != "" {
		// Update existing VPN user
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
		// Create new VPN user
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

	// 2. Insert entitlement record
	entitlementID := uuid.New().String()
	entitlement := &models.Entitlement{
		ID:           entitlementID,
		UserID:       req.UserID,
		Email:        req.Email,
		OtunUUID:     &otunUUID,
		Source:       models.EntitlementSourceGift,
		Status:       models.EntitlementStatusActive,
		TrafficLimit: trafficLimit,
		TrafficUsed:  0,
		ExpireAt:     &expireAt,
		ServiceTier:  serviceTier,
		GrantedBy:    "admin",
		Note:         req.Note,
	}

	if err := s.entitlementRepo.Create(ctx, entitlement); err != nil {
		return nil, fmt.Errorf("failed to save entitlement: %w", err)
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
		EntitlementID: entitlementID,
		OtunUUID:      otunUUID,
		TrafficLimit:  trafficLimit,
		ExpireAt:      expireAt.Format(time.RFC3339),
		Protocols:     protocols,
	}, nil
}

// ListEntitlements lists entitlements with optional filters (admin/internal)
func (s *EntitlementService) ListEntitlements(ctx context.Context, userID, source, status string) ([]*models.EntitlementInfo, error) {
	entitlements, err := s.entitlementRepo.ListByFilters(ctx, userID, source, status)
	if err != nil {
		return nil, fmt.Errorf("list entitlements: %w", err)
	}

	var results []*models.EntitlementInfo
	for _, e := range entitlements {
		info := &models.EntitlementInfo{
			ID:           e.ID,
			UserID:       e.UserID,
			Email:        e.Email,
			OtunUUID:     e.OtunUUID,
			Source:       e.Source,
			Status:       e.Status,
			TrafficLimit: e.TrafficLimit,
			TrafficUsed:  e.TrafficUsed,
			ServiceTier:  e.ServiceTier,
			GrantedBy:    e.GrantedBy,
			Note:         e.Note,
			DeviceID:     e.DeviceID,
			CreatedAt:    e.CreatedAt.Format(time.RFC3339),
			UpdatedAt:    e.UpdatedAt.Format(time.RFC3339),
		}
		if e.ExpireAt != nil {
			expireStr := e.ExpireAt.Format(time.RFC3339)
			info.ExpireAt = &expireStr
		}
		results = append(results, info)
	}

	return results, nil
}

// buildTrialInfoFromEntitlement builds a TrialAccountInfo from a local entitlement record
func (s *EntitlementService) buildTrialInfoFromEntitlement(e *models.Entitlement) *models.TrialAccountInfo {
	otunUUID := ""
	if e.OtunUUID != nil {
		otunUUID = *e.OtunUUID
	}
	expireAtStr := ""
	if e.ExpireAt != nil {
		expireAtStr = e.ExpireAt.Format(time.RFC3339)
	}
	expired := repository.IsExpired(e)
	enabled := e.Status == models.EntitlementStatusActive && !expired

	return &models.TrialAccountInfo{
		UUID:         otunUUID,
		TrafficLimit: e.TrafficLimit,
		TrafficUsed:  e.TrafficUsed,
		ExpireAt:     expireAtStr,
		Enabled:      enabled,
		Expired:      expired,
		Protocols:    nil,
	}
}
