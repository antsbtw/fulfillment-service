package models

import "time"

// VPN provision business_type constants
const (
	BusinessTypePurchase     = "purchase"
	BusinessTypeSubscription = "subscription"
	BusinessTypeTrial        = "trial"
	BusinessTypeGift         = "gift"
)

// VPN provision status constants
const (
	VPNProvisionStatusActive    = "active"
	VPNProvisionStatusExpired   = "expired"
	VPNProvisionStatusDisabled  = "disabled"
	VPNProvisionStatusRevoked   = "revoked"
	VPNProvisionStatusConverted = "converted"
)

// VPN service tier constants
const (
	ServiceTierStandard    = "standard"
	ServiceTierPremium     = "premium"
	ServiceTierResidential = "residential"
)

// VPNProvision represents a VPN user provision record (otun)
// Merges the old resources (vpn_user) and entitlements tables
type VPNProvision struct {
	ID             string
	UserID         string
	SubscriptionID string
	Channel        string

	// Business classification
	BusinessType string
	ServiceTier  string

	// otun-manager reference
	OtunUUID *string

	// Plan and status
	PlanTier string
	Status   string

	// Traffic and expiry
	TrafficLimit int64
	TrafficUsed  int64
	ExpireAt     *time.Time

	// Trial/gift fields
	Email     string
	DeviceID  string
	GrantedBy string
	Note      string

	// Current record marker
	IsCurrent bool

	CreatedAt time.Time
	UpdatedAt time.Time
}

// MapPlanToServiceTier maps plan_tier to service_tier
func MapPlanToServiceTier(planTier string) string {
	switch planTier {
	case "basic":
		return ServiceTierStandard
	case "premium":
		return ServiceTierPremium
	case "unlimited":
		return ServiceTierPremium
	default:
		return ServiceTierStandard
	}
}
