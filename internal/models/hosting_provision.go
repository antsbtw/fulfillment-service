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

	CreatedAt time.Time
	UpdatedAt time.Time
	ReadyAt   *time.Time
	DeletedAt *time.Time
}
