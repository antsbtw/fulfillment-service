package models

import "time"

// ==================== 订阅 / 订购 profile 记账层（形态 B，规则 v2 + 契约 v0.2）====================
//
// 每个产品面（standard / residential）在 fulfillment 记账层拆成最多三个 profile：
//   subscription：apple/google/stripe recurring —— expire = max(period_end)，流量按本期
//   purchase    ：所有一次性（stripe 一次性 / credit / gift / gift_card / campaign / manual）—— 一桶
//                 天数 + 流量，只在成为生效 profile 时开始消耗
//   trial       ：试用（沿用现有 trial 逻辑，只额外记一条）
// 下游 otun-manager 账号仍每面一个、uuid 不变；vpn_provisions 是该面账号的投影行。

// service_face（记账层的"产品面"，与 vpn_provisions.service_tier 的二元分区一致）
const (
	ServiceFaceStandard    = "standard"
	ServiceFaceResidential = "residential"
)

// class / active_class（契约 §4.1，冻结）
const (
	EntitlementClassSubscription = "subscription"
	EntitlementClassPurchase     = "purchase"
	EntitlementClassTrial        = "trial"
	ActiveClassNone              = "none" // 仅 active_class 用
)

// profiles[].status（契约 §4.2，冻结；权益语义，与顶层 provisioning status 不同枚举）
const (
	ProfileStatusActive  = "active"
	ProfileStatusWaiting = "waiting"
	ProfileStatusExpired = "expired"
	ProfileStatusNone    = "none"
)

// kind（契约 §4.3，冻结；展示分类，由 channel/purchase_type/channel_sub_id 映射，不是原始 channel）
const (
	EntryKindApple         = "apple"
	EntryKindGoogle        = "google"
	EntryKindStripe        = "stripe"
	EntryKindStripeOnetime = "stripe_onetime"
	EntryKindCredit        = "credit"
	EntryKindCampaign      = "promo"
	EntryKindGift          = "gift"
	EntryKindGiftCard      = "gift_card"
	EntryKindManual        = "manual"
	EntryKindTrial         = "trial"
)

// purchase_type（上游 payment/credit 发出的取值）
const (
	PurchaseTypeSubscription = "subscription"
	PurchaseTypeOneTime      = "one_time"
	PurchaseTypeGift         = "gift"
	PurchaseTypeTrial        = "trial"
)

// ServiceFaceOf 把 service_tier / plan_tier 折成产品面（residential 与其他）。
func ServiceFaceOf(serviceTier string) string {
	if serviceTier == ServiceTierResidential {
		return ServiceFaceResidential
	}
	return ServiceFaceStandard
}

// EntitlementProfile 对应 fulfillment.vpn_entitlement_profiles 一行。
type EntitlementProfile struct {
	ID          string
	UserID      string
	ServiceFace string
	Class       string
	Status      string

	ExpireAt    *time.Time
	ActiveSince *time.Time

	TrafficLimit    int64
	TrafficUsed     int64
	TrafficBaseline int64

	DaysRemaining int
	DaysConsumed  int

	EffectiveFrom *time.Time

	CreatedAt time.Time
	UpdatedAt time.Time
}

// EntitlementEntry 对应 fulfillment.vpn_entitlement_entries 一行（一笔权益条目）。
type EntitlementEntry struct {
	ID             string
	ProfileID      string
	SubscriptionID string
	Channel        string
	ChannelSubID   string
	Kind           string
	PurchaseType   string
	Days           int
	Traffic        int64
	PeriodStart    *time.Time
	PeriodEnd      *time.Time
	GrantedAt      time.Time
	RevokedAt      *time.Time
	SourceEventID  string
	CreatedAt      time.Time
}

// ==================== 对外投影（契约 §2）====================

// VPNProfileView 是 /vpn/all 元素 profiles[] 的一项（字段名冻结，契约 §7）。
// 新增字段全部可空/可缺；端上不得依赖它们决定连接。
type VPNProfileView struct {
	Class  string `json:"class"`  // subscription | purchase | trial
	Status string `json:"status"` // active | waiting | expired | none
	// waiting 无到期（null）；active = active_since + 剩余天数
	ExpireAt     *string `json:"expire_at"`
	TrafficLimit int64   `json:"traffic_limit"`
	TrafficUsed  int64   `json:"traffic_used"`
	// 以下仅 purchase 桶下发（方案 a：与订阅项同形 + 冗余便利字段）
	TrafficRemaining *int64 `json:"traffic_remaining,omitempty"`
	DaysRemaining    *int   `json:"days_remaining,omitempty"`
	DaysConsumed     *int   `json:"days_consumed,omitempty"`
	// waiting = 订阅 profile expire；active = active_since
	EffectiveFrom *string  `json:"effective_from,omitempty"`
	Kinds         []string `json:"kinds,omitempty"`
}

// EntitlementProjection 是 Project() 的产出：该面的 active_class + profiles[] + 投影用的生效值。
type EntitlementProjection struct {
	ActiveClass string
	Profiles    []VPNProfileView
	// 生效 profile 的投影值（供 vpn_provisions 投影行 / 顶层既有字段）
	ExpireAt     *time.Time // none 时 = 最后生效 profile 的到期（不为 nil，若曾有过 profile）
	TrafficLimit int64
	TrafficUsed  int64
	Channel      string
}

// ==================== 后台读口 DTO ====================

// AdminEntitlementProfileView 后台排障读口：一条 profile + 其 entries。
type AdminEntitlementProfileView struct {
	ID              string     `json:"id"`
	ServiceFace     string     `json:"service_face"`
	Class           string     `json:"class"`
	Status          string     `json:"status"`
	ExpireAt        *time.Time `json:"expire_at"`
	ActiveSince     *time.Time `json:"active_since"`
	TrafficLimit    int64      `json:"traffic_limit"`
	TrafficUsed     int64      `json:"traffic_used"`
	TrafficBaseline int64      `json:"traffic_baseline"`
	DaysRemaining   int        `json:"days_remaining"`
	DaysConsumed    int        `json:"days_consumed"`
	EffectiveFrom   *time.Time `json:"effective_from"`
	UpdatedAt       time.Time  `json:"updated_at"`

	Entries []AdminEntitlementEntryView `json:"entries"`
}

// AdminEntitlementEntryView 后台读口的条目视图。
type AdminEntitlementEntryView struct {
	ID             string     `json:"id"`
	SubscriptionID string     `json:"subscription_id"`
	Channel        string     `json:"channel"`
	ChannelSubID   string     `json:"channel_sub_id"`
	Kind           string     `json:"kind"`
	PurchaseType   string     `json:"purchase_type"`
	Days           int        `json:"days"`
	Traffic        int64      `json:"traffic"`
	PeriodStart    *time.Time `json:"period_start"`
	PeriodEnd      *time.Time `json:"period_end"`
	GrantedAt      time.Time  `json:"granted_at"`
	RevokedAt      *time.Time `json:"revoked_at"`
	SourceEventID  string     `json:"source_event_id"`
}

// AdminUserVPNProfilesResponse GET /api/internal/admin/users/:user_id/vpn-profiles
type AdminUserVPNProfilesResponse struct {
	UserID string                                   `json:"user_id"`
	Faces  map[string][]AdminEntitlementProfileView `json:"faces"` // standard / residential
}
