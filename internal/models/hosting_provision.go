package models

import "time"

// HostingProvision represents a provisioned hosting node (obox)
type HostingProvision struct {
	ID             string
	SubscriptionID string
	UserID         string
	Channel        string

	// hosting-service reference
	HostingNodeID string
	Provider      string
	Region        string

	// Node connection info (cached from hosting-service callback)
	PublicIP  *string
	APIPort   int
	APIKey    *string
	VlessPort int
	SSPort    int
	PublicKey *string
	ShortID   *string

	// Status and plan
	Status       string
	ErrorMessage *string
	PlanTier     string
	TrafficLimit int64
	TrafficUsed  int64

	// Cleanup tracking
	NeedsCleanup bool // 标记是否需要后台清理（VPS 创建失败但删除也失败时设置）

	// NodeKind hosted_v2(新式:owner key + 应用平台)/ hosted_legacy(老式一体化)。
	NodeKind string
	// NeedsRebuild 换套餐后新式机不自动重建,等用户在 App 里删除重建。
	NeedsRebuild bool

	CreatedAt time.Time
	UpdatedAt time.Time
	ReadyAt   *time.Time
	DeletedAt *time.Time
}

const (
	NodeKindHostedV2     = "hosted_v2"
	NodeKindHostedLegacy = "hosted_legacy"
)

// NodeKindOrLegacy 空值按老式处理(存量行)。
func (hp *HostingProvision) NodeKindOrLegacy() string {
	if hp.NodeKind == "" {
		return NodeKindHostedLegacy
	}
	return hp.NodeKind
}

// IsHostedV2 新式托管机。
func (hp *HostingProvision) IsHostedV2() bool { return hp.NodeKind == NodeKindHostedV2 }
