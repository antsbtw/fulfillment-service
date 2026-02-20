package models

// ==================== Trial Config DTO ====================

// TrialConfigResponse is returned by GET /api/v1/public/trial/config
type TrialConfigResponse struct {
	Enabled       bool `json:"enabled"`
	DurationHours int  `json:"duration_hours"`
	TrafficGB     int  `json:"traffic_gb"`
}

// ==================== Admin Gift DTOs ====================

// GiftEntitlementRequest is the request for POST /api/internal/entitlements/gift
type GiftEntitlementRequest struct {
	UserID       string `json:"user_id" binding:"required"`
	Email        string `json:"email"`
	TrafficGB    int    `json:"traffic_gb" binding:"required"`
	DurationDays int    `json:"duration_days" binding:"required"`
	ServiceTier  string `json:"service_tier"`
	Note         string `json:"note"`
}

// GiftEntitlementResponse is returned by POST /api/internal/entitlements/gift
type GiftEntitlementResponse struct {
	EntitlementID string          `json:"entitlement_id"`
	OtunUUID      string          `json:"otun_uuid"`
	TrafficLimit  int64           `json:"traffic_limit"`
	ExpireAt      string          `json:"expire_at"`
	Protocols     []VPNProtocol `json:"protocols"`
}

// ==================== Admin Query DTOs ====================

// EntitlementInfo is the admin view of a vpn provision
type EntitlementInfo struct {
	ID           string  `json:"id"`
	UserID       string  `json:"user_id"`
	Email        string  `json:"email"`
	OtunUUID     *string `json:"otun_uuid"`
	BusinessType string  `json:"business_type"`
	Status       string  `json:"status"`
	TrafficLimit int64   `json:"traffic_limit"`
	TrafficUsed  int64   `json:"traffic_used"`
	ExpireAt     *string `json:"expire_at"`
	ServiceTier  string  `json:"service_tier"`
	GrantedBy    string  `json:"granted_by"`
	Note         string  `json:"note"`
	DeviceID     string  `json:"device_id"`
	CreatedAt    string  `json:"created_at"`
	UpdatedAt    string  `json:"updated_at"`
}
