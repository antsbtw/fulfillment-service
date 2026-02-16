package models

import "time"

// Entitlement source constants
const (
	EntitlementSourceTrial    = "trial"
	EntitlementSourceGift     = "gift"
	EntitlementSourcePurchase = "purchase"
	EntitlementSourcePromo    = "promo"
)

// Entitlement status constants
const (
	EntitlementStatusActive  = "active"
	EntitlementStatusExpired = "expired"
	EntitlementStatusRevoked = "revoked"
)

// Entitlement represents a user's entitlement record
type Entitlement struct {
	ID           string
	UserID       string
	Email        string
	OtunUUID     *string
	Source       string
	Status       string
	TrafficLimit int64
	TrafficUsed  int64
	ExpireAt     *time.Time
	ServiceTier  string
	GrantedBy    string
	Note         string
	DeviceID     string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// ==================== Trial DTOs ====================

// TrialConfigResponse is returned by GET /api/v1/public/trial/config
type TrialConfigResponse struct {
	Enabled       bool `json:"enabled"`
	DurationHours int  `json:"duration_hours"`
	TrafficGB     int  `json:"traffic_gb"`
}

// TrialStatusResponse is returned by GET /api/v1/my/trial/status
type TrialStatusResponse struct {
	TrialAvailable bool               `json:"trial_available"`
	TrialUsed      bool               `json:"trial_used"`
	ExistingTrial  *TrialAccountInfo  `json:"existing_trial"`
}

// TrialAccountInfo contains details of an existing trial
type TrialAccountInfo struct {
	UUID         string             `json:"uuid"`
	TrafficLimit int64              `json:"traffic_limit"`
	TrafficUsed  int64              `json:"traffic_used"`
	ExpireAt     string             `json:"expire_at"`
	Enabled      bool               `json:"enabled"`
	Expired      bool               `json:"expired"`
	Protocols    []TrialProtocol    `json:"protocols"`
}

// TrialProtocol represents a VPN protocol in trial response
type TrialProtocol struct {
	Protocol string `json:"protocol"`
	URL      string `json:"url"`
	Node     string `json:"node"`
}

// ActivateTrialRequest is the request for POST /api/v1/my/trial/activate
type ActivateTrialRequest struct {
	DeviceID string `json:"device_id" binding:"required"`
}

// ActivateTrialResponse is returned by POST /api/v1/my/trial/activate
type ActivateTrialResponse struct {
	UUID         string          `json:"uuid"`
	IsTrial      bool            `json:"is_trial"`
	TrafficLimit int64           `json:"traffic_limit"`
	TrafficUsed  int64           `json:"traffic_used"`
	ExpireAt     string          `json:"expire_at"`
	Enabled      bool            `json:"enabled"`
	Protocols    []TrialProtocol `json:"protocols"`
}

// ==================== Admin Gift DTOs ====================

// GiftEntitlementRequest is the request for POST /api/internal/entitlements/gift
type GiftEntitlementRequest struct {
	UserID      string `json:"user_id" binding:"required"`
	Email       string `json:"email"`
	TrafficGB   int    `json:"traffic_gb" binding:"required"`
	DurationDays int   `json:"duration_days" binding:"required"`
	ServiceTier string `json:"service_tier"`
	Note        string `json:"note"`
}

// GiftEntitlementResponse is returned by POST /api/internal/entitlements/gift
type GiftEntitlementResponse struct {
	EntitlementID string          `json:"entitlement_id"`
	OtunUUID      string          `json:"otun_uuid"`
	TrafficLimit  int64           `json:"traffic_limit"`
	ExpireAt      string          `json:"expire_at"`
	Protocols     []TrialProtocol `json:"protocols"`
}

// ==================== Admin Query DTOs ====================

// EntitlementInfo is the admin view of an entitlement
type EntitlementInfo struct {
	ID           string  `json:"id"`
	UserID       string  `json:"user_id"`
	Email        string  `json:"email"`
	OtunUUID     *string `json:"otun_uuid"`
	Source       string  `json:"source"`
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
