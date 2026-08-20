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

// Product face（vpn_provisions 分区键，迁移 010）。与 service_tier（节点面）不同：
// campaign 的节点面仍是 standard，但产品面独立，与 basic/residential 零交集。
const (
	ProductFaceBasic       = "basic"
	ProductFaceResidential = "residential"
	ProductFaceCampaign    = "promo" // ★2026-08-20 改名：与 credit-service GiftCampaign 区分（Q3）；DB 存量由迁移 011 UPDATE
)

// PlanTierCampaign 第三产品面的 plan_tier / channel 枚举（契约 §8 冻结）。
const PlanTierCampaign = "promo"

// PlanTierCampaignLegacy 是改名前的旧值（2026-08-20 之前）。
// ★只用于【入参识别】，绝不用于下发或落库：改名与各服务滚动上线之间存在窗口，期间
// campaign-service 可能仍发 plan_tier="campaign"。若不认这个旧值，MapPlanToProductFace 会
// 把它 default 成 basic → 活动请求落进 basic 面（正是本次改名要防的串面）。
// 各服务全部滚完且确认无旧值流量后可移除。
const PlanTierCampaignLegacy = "campaign"

// IsCampaignPlanTier 判定 plan_tier/channel 是否指第三产品面（含改名前旧值）。
func IsCampaignPlanTier(v string) bool {
	return v == PlanTierCampaign || v == PlanTierCampaignLegacy
}

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

	// ProductFace 产品面分区键（basic | residential | campaign，迁移 010）。空值视为按
	// plan_tier/service_tier 推导（见 EffectiveProductFace），兼容未回填/测试构造的行。
	ProductFace string

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
	case PlanTierCampaign:
		// 活动 profile：节点面一期固定 standard（概要 §5.1），产品面由 MapPlanToProductFace 区分。
		return ServiceTierStandard
	case "residential":
		// 私宅IP套餐 → residential 服务等级。
		// otun-manager 据此把用户开通为 residential，并触发 realm 默认出口分配
		// （EnsureDefaultAssignment）。漏掉此 case 会让 residential 被静默降级为
		// standard，realm 链路在上游即断。与 otun-manager MapPlanTierToNodeTier 一致。
		return ServiceTierResidential
	default:
		return ServiceTierStandard
	}
}

// MapPlanToProductFace maps plan_tier to the vpn_provisions partition key（迁移 010）：
// basic/premium/unlimited → basic；residential → residential；campaign → campaign。
func MapPlanToProductFace(planTier string) string {
	switch {
	case planTier == "residential":
		return ProductFaceResidential
	case IsCampaignPlanTier(planTier): // 含改名前旧值，滚动窗口内不串面
		return ProductFaceCampaign
	default:
		return ProductFaceBasic
	}
}

// ProductFaceFor 由 (plan_tier, service_tier) 推导产品面：campaign 优先看 plan_tier；其余沿旧分区
// 谓词 (service_tier='residential') 的口径——与迁移 010 的存量回填完全一致。
func ProductFaceFor(planTier, serviceTier string) string {
	if IsCampaignPlanTier(planTier) { // 含改名前旧值
		return ProductFaceCampaign
	}
	if serviceTier == ServiceTierResidential {
		return ProductFaceResidential
	}
	return ProductFaceBasic
}

// EffectiveProductFace 返回行的产品面：优先列值，空则推导。
func (vp *VPNProvision) EffectiveProductFace() string {
	if vp.ProductFace != "" {
		return vp.ProductFace
	}
	return ProductFaceFor(vp.PlanTier, vp.ServiceTier)
}

// PartitionFace 把旧的二元分区参数（isResidential）折成产品面：true → residential，false → basic。
// ★basic 分区显式排除 campaign（IMPL_PROMPT §2）。
func PartitionFace(isResidential bool) string {
	if isResidential {
		return ProductFaceResidential
	}
	return ProductFaceBasic
}

// CampaignGrant 活动账号入账台账一行（迁移 010 fulfillment.campaign_grants）。
type CampaignGrant struct {
	SubscriptionID string
	UserID         string
	ChannelSubID   string
	Days           int
	TrafficBytes   int64
	Status         string // active | revoked | expired
	AppliedAt      time.Time
	RevokedAt      *time.Time
}

// CampaignGrantAggregate 当前周期计入的 grant 聚合（/vpn/all campaign{} 子对象 + 内部读口）。
type CampaignGrantAggregate struct {
	ClaimsActive        int
	GrantedDaysTotal    int
	GrantedTrafficTotal int64
	LastClaimAt         *time.Time
}
