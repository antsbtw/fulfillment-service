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

// EntitlementService handles gift and other entitlement operations
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
	var protocols []models.VPNProtocol
	if syncResp != nil {
		for _, p := range syncResp.Protocols {
			protocols = append(protocols, models.VPNProtocol{
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

