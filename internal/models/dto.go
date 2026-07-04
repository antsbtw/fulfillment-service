package models

import (
	"encoding/json"
	"time"
)

// ==================== Internal API DTOs ====================

// ProvisionRequest is sent by subscription-service to create a resource
type ProvisionRequest struct {
	// Three-level classification (new fields, preferred)
	AppSource    string `json:"app_source"`    // otun / obox
	BusinessType string `json:"business_type"` // purchase / subscription / trial / gift
	Channel      string `json:"channel"`       // apple, google, stripe, credit

	// Association
	SubscriptionID string `json:"subscription_id"`
	UserID         string `json:"user_id" binding:"required"`
	UserEmail      string `json:"user_email"`

	// Resource parameters
	ResourceType string `json:"resource_type"` // Legacy: hosting_node, otun_node, vpn_user
	PlanTier     string `json:"plan_tier"`     // basic, standard, premium, unlimited
	Region       string `json:"region"`
	TrafficLimit int64  `json:"traffic_limit"`
	ExpireDays   int    `json:"expire_days"`

	// v3.1 新增
	ProductID    string `json:"product_id,omitempty"`
	PurchaseType string `json:"purchase_type,omitempty"` // subscription, one_time

	ChannelSubID string     `json:"channel_sub_id,omitempty"`
	ChannelTxID  string     `json:"channel_tx_id,omitempty"`
	PeriodStart  *time.Time `json:"period_start,omitempty"`
	PeriodEnd    *time.Time `json:"period_end,omitempty"`
	Amount       int64      `json:"amount,omitempty"`
	Currency     string     `json:"currency,omitempty"`
	Quantity     int        `json:"quantity,omitempty"`
	Months       int        `json:"months,omitempty"`

	// Trial-specific
	DeviceID string `json:"device_id,omitempty"`
}

// ProvisionResponse is returned after starting provisioning
type ProvisionResponse struct {
	ResourceID            string `json:"resource_id"`
	Status                string `json:"status"`
	EstimatedReadySeconds int    `json:"estimated_ready_seconds,omitempty"`
	VPNUserID             string `json:"vpn_user_id,omitempty"` // For VPN user, the UUID in otun-manager
	Message               string `json:"message"`
}

// DeprovisionRequest is sent to delete a resource
type DeprovisionRequest struct {
	SubscriptionID string `json:"subscription_id" binding:"required"`
	ResourceID     string `json:"resource_id"`
	Reason         string `json:"reason"`
}

type OBoxDeprovisionRequest struct {
	UserID string `json:"user_id" binding:"required"`
	VPSID  string `json:"vps_id" binding:"required"`
	Reason string `json:"reason"`
}

type OBoxSuspendRequest struct {
	UserID       string `json:"user_id" binding:"required"`
	VPSID        string `json:"vps_id" binding:"required"`
	Reason       string `json:"reason"`
	TrafficUsed  int64  `json:"traffic_used"`
	TrafficLimit int64  `json:"traffic_limit"`
}

// DeprovisionResponse is returned after starting deprovisioning
type DeprovisionResponse struct {
	ResourceID string `json:"resource_id"`
	Status     string `json:"status"`
	Message    string `json:"message"`
}

// ResourceStatusResponse is the detailed resource status
type ResourceStatusResponse struct {
	ResourceID     string  `json:"resource_id"`
	SubscriptionID string  `json:"subscription_id"`
	UserID         string  `json:"user_id"`
	ResourceType   string  `json:"resource_type"`
	Provider       string  `json:"provider"`
	Region         string  `json:"region"`
	Status         string  `json:"status"`
	PublicIP       *string `json:"public_ip,omitempty"`
	APIPort        int     `json:"api_port,omitempty"`
	APIKey         *string `json:"api_key,omitempty"`
	VlessPort      int     `json:"vless_port,omitempty"`
	SSPort         int     `json:"ss_port,omitempty"`
	PublicKey      *string `json:"public_key,omitempty"`
	ShortID        *string `json:"short_id,omitempty"`
	PlanTier       string  `json:"plan_tier"`
	TrafficLimitGB float64 `json:"traffic_limit_gb"`
	TrafficUsedGB  float64 `json:"traffic_used_gb"`
	TrafficPercent float64 `json:"traffic_percent"`
	ReadyAt        *string `json:"ready_at,omitempty"`
	CreatedAt      string  `json:"created_at"`
	ErrorMessage   *string `json:"error_message,omitempty"`
}

// ==================== User API DTOs ====================

// HostingStatus represents different hosting states for frontend
type HostingStatus string

const (
	HostingStatusNoSubscription      HostingStatus = "no_subscription"      // 无订阅
	HostingStatusSubscribedNoNode    HostingStatus = "subscribed_no_node"   // 有订阅无节点
	HostingStatusNodeCreating        HostingStatus = "node_creating"        // 节点创建中
	HostingStatusNodeActive          HostingStatus = "node_active"          // 节点正常运行
	HostingStatusNodeFailed          HostingStatus = "node_failed"          // 节点创建失败
	HostingStatusSubscriptionExpired HostingStatus = "subscription_expired" // 订阅已过期
)

// UserNodeStatusResponse is returned to users querying their node
type UserNodeStatusResponse struct {
	// 主状态字段 - 前端根据此字段决定显示逻辑
	HostingStatus HostingStatus `json:"hosting_status"`

	// 订阅信息 (如果有)
	HasSubscription bool              `json:"has_subscription"`
	Subscription    *SubscriptionInfo `json:"subscription,omitempty"`

	// 节点信息 (如果有)
	HasNode bool          `json:"has_node"`
	Node    *UserNodeInfo `json:"node,omitempty"`

	// 节点创建进度 (创建中时使用)
	CreationProgress *NodeCreationProgress `json:"creation_progress,omitempty"`

	// 友好提示
	Message string `json:"message,omitempty"`
}

// SubscriptionInfo contains subscription details
type SubscriptionInfo struct {
	SubscriptionID string `json:"subscription_id"`
	Status         string `json:"status"`
	PlanTier       string `json:"plan_tier"`
	ExpiresAt      string `json:"expires_at,omitempty"`
	AutoRenew      bool   `json:"auto_renew"`
}

// NodeCreationProgress tracks node creation steps
type NodeCreationProgress struct {
	CurrentStep int    `json:"current_step"` // 1-4
	TotalSteps  int    `json:"total_steps"`  // 4
	StepName    string `json:"step_name"`    // 当前步骤名称
	Steps       []NodeCreationStep `json:"steps"`
}

// NodeCreationStep represents a single creation step
type NodeCreationStep struct {
	Step      int    `json:"step"`
	Name      string `json:"name"`
	Status    string `json:"status"` // pending, in_progress, completed, failed
	StartedAt string `json:"started_at,omitempty"`
}

// UserNodeInfo contains node info visible to users
type UserNodeInfo struct {
	ResourceID     string  `json:"resource_id"`
	Region         string  `json:"region"`
	RegionName     string  `json:"region_name"`
	Status         string  `json:"status"`
	PublicIP       *string `json:"public_ip,omitempty"`
	APIPort        int     `json:"api_port,omitempty"`
	APIKey         *string `json:"api_key,omitempty"`
	VlessPort      int     `json:"vless_port,omitempty"`
	SSPort         int     `json:"ss_port,omitempty"`
	PublicKey      *string `json:"public_key,omitempty"`
	ShortID        *string `json:"short_id,omitempty"`
	PlanTier       string  `json:"plan_tier"`
	TrafficLimitGB float64 `json:"traffic_limit_gb"`
	TrafficUsedGB  float64 `json:"traffic_used_gb"`
	TrafficPercent float64 `json:"traffic_percent"`
	CreatedAt      string  `json:"created_at"`
}

// RegionListResponse is the list of available regions
type RegionListResponse struct {
	Regions []RegionInfo `json:"regions"`
}

// RegionInfo is a single region entry
type RegionInfo struct {
	Code      string `json:"code"`
	Name      string `json:"name"`
	Provider  string `json:"provider"`
	Available bool   `json:"available"`
}

// CreateNodeRequest is for user-initiated node creation
type CreateNodeRequest struct {
	Region string `json:"region" binding:"required"`
}

// RecreateNodeRequest is for user-initiated node recreation
type RecreateNodeRequest struct {
	Region string `json:"region" binding:"required"`
}

// DeleteNodeResponse is returned after node deletion
type DeleteNodeResponse struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
}

// CreateNodeResponse is returned after starting node creation
type CreateNodeResponse struct {
	Success          bool                  `json:"success"`
	ResourceID       string                `json:"resource_id,omitempty"`
	Status           string                `json:"status"` // creating, failed
	CreationProgress *NodeCreationProgress `json:"creation_progress,omitempty"`
	Message          string                `json:"message"`
}

// ==================== Callback DTOs ====================

// NodeReadyCallback is sent by node agent when ready
type NodeReadyCallback struct {
	ResourceID string `json:"resource_id" binding:"required"`
	PublicIP   string `json:"public_ip" binding:"required"`
	APIPort    int    `json:"api_port"`
	APIKey     string `json:"api_key"`
	VlessPort  int    `json:"vless_port"`
	SSPort     int    `json:"ss_port"`
	PublicKey  string `json:"public_key"`
	ShortID    string `json:"short_id"`
}

// NodeFailedCallback is sent when node creation fails
type NodeFailedCallback struct {
	ResourceID   string `json:"resource_id" binding:"required"`
	ErrorMessage string `json:"error_message"`
}

// ==================== Subscription Service Callback ====================

// SubscriptionCallback is sent to subscription-service on status changes (v3.1 简化版)
type SubscriptionCallback struct {
	SubscriptionID string `json:"subscription_id" binding:"required"`
	App            string `json:"app" binding:"required"`    // otun, obox
	Status         string `json:"status" binding:"required"` // active, failed, deleted
	Reason         string `json:"reason,omitempty"`          // 删除原因：user_initiated（用户主动删除VPS）, subscription_cancelled（订阅取消）
	Error          string `json:"error,omitempty"`
	Message        string `json:"message,omitempty"`
}

// ==================== VPN User DTOs ====================

// VPNStatus represents different VPN states for frontend
type VPNStatus string

const (
	VPNStatusNoSubscription      VPNStatus = "no_subscription"      // 无订阅
	VPNStatusActive              VPNStatus = "active"               // VPN 正常可用
	VPNStatusExpired             VPNStatus = "expired"              // 已过期
	VPNStatusDisabled            VPNStatus = "disabled"             // 已禁用
)

// VPNStatusResponse is returned to users querying their VPN status
type VPNStatusResponse struct {
	// 主状态字段 - 前端根据此字段决定显示逻辑
	VPNStatus VPNStatus `json:"vpn_status"`

	// 订阅信息 (如果有)
	HasSubscription bool              `json:"has_subscription"`
	Subscription    *SubscriptionInfo `json:"subscription,omitempty"`

	// VPN 用户信息 (如果有)
	HasVPNUser bool         `json:"has_vpn_user"`
	VPNUser    *VPNUserInfo `json:"vpn_user,omitempty"`

	// 友好提示
	Message string `json:"message,omitempty"`
}

// VPNUserInfo contains VPN user info visible to users
type VPNUserInfo struct {
	ResourceID     string  `json:"resource_id"`
	VPNUserID      string  `json:"vpn_user_id"`      // UUID in otun-manager
	Status         string  `json:"status"`
	PlanTier       string  `json:"plan_tier"`
	TrafficLimitGB float64 `json:"traffic_limit_gb"`
	TrafficUsedGB  float64 `json:"traffic_used_gb"`
	TrafficPercent float64 `json:"traffic_percent"`
	ExpireAt       string  `json:"expire_at,omitempty"`
	CreatedAt      string  `json:"created_at"`
}

// VPNSubscribeResponse is returned when getting VPN subscription config
type VPNSubscribeResponse struct {
	Status       string        `json:"status"`
	Channel      string        `json:"channel"`
	PlanTier     string        `json:"plan_tier"`
	ServiceTier  string        `json:"service_tier"`
	SubscribeURL string        `json:"subscribe_url"`
	DeviceID     string        `json:"device_id"`
	Protocols    []VPNProtocol `json:"protocols,omitempty"`
	TrafficLimit int64         `json:"traffic_limit"`
	TrafficUsed  int64         `json:"traffic_used"`
	ExpireAt     string        `json:"expire_at,omitempty"`
	Message      string        `json:"message,omitempty"`
	// P0：出口国家 + 客户端分流策略。【仅 residential(realm) 分支填充】，标准套餐留空
	//（omitempty 不输出），前端对标准分支按"无此字段"降级。SmartStrategy 用 RawMessage
	// 原样透传 otun-manager 下发的结构，fulfillment 不解析其内部。
	ExitCountry   string          `json:"exit_country,omitempty"`
	SmartStrategy json.RawMessage `json:"smart_strategy,omitempty"`
	// ★2c：N=2 出口摘要（primary/backup 各一，region 挂出口级）。仅 realm 分支填，标准 omitempty 不输出。
	// 前端按 protocol.node == nodes[].role 匹配取 region。单出口只 1 个 node。
	Nodes []RealmNodeSummary `json:"nodes,omitempty"`
	// ★Batch 3（§8.3）：config_version = protocols + smart_strategy 的稳定 hash。
	// 前端轻量拉此值（前台/建连前/30min），变了才拉全量 + 静默重连。
	// 遵防 churn 铁律（§5.2）：只纳变了才需刷新的字段 + 集合排序，traffic_used 等实时值不纳入。
	ConfigVersion string `json:"config_version,omitempty"`
}

// VPNQuickStatus is a lightweight status response without protocols
type VPNQuickStatus struct {
	Status       string `json:"status"`
	Channel      string `json:"channel"`
	PlanTier     string `json:"plan_tier"`
	TrafficLimit int64  `json:"traffic_limit"`
	TrafficUsed  int64  `json:"traffic_used"`
	ExpireAt     string `json:"expire_at,omitempty"`
}

// VPNProtocol represents a single VPN protocol configuration.
//
// 序列化时【同时】输出 "protocol" 与 "protocol_name" 两个 key（值相同）。
// 已发布客户端里存在两套解码结构体，分别认不同字段名：
//   - ProtocolConfig.CodingKeys      认 "protocol"
//   - VPNResourceProtocol.CodingKeys 认 "protocol_name"
// 历史上在 protocol ↔ protocol_name 之间来回改，必然破坏其中一套。双 key 输出对两套都兼容，
// 是唯一不会再回归的形态。不要再删任何一个 key。改动只在序列化层，不影响任何后端消费者。
type VPNProtocol struct {
	Protocol string `json:"-"` // vless, shadowsocks（MarshalJSON 同时写出 protocol + protocol_name）
	URL      string `json:"url"`
	Node     string `json:"node"` // primary, backup
}

// MarshalJSON 同时输出 protocol 与 protocol_name（值相同），兼容前端两套解码结构体。
// ★2c：region 不挂 protocol（出口级归属，出现 N 次冗余）——改由 response 顶层 nodes[] 摘要
// 表达（每出口 1 次），前端按 protocol.node 匹配 nodes[].role 取 region。protocols[] 回归纯协议列表。
func (p VPNProtocol) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Protocol     string `json:"protocol"`
		ProtocolName string `json:"protocol_name"`
		URL          string `json:"url"`
		Node         string `json:"node"`
	}{
		Protocol:     p.Protocol,
		ProtocolName: p.Protocol,
		URL:          p.URL,
		Node:         p.Node,
	})
}

// RealmNodeSummary 是 N=2 出口摘要块（response 顶层 nodes[]，§2c region 结构）。
// region 挂在出口级（每出口 1 次），前端按 protocol.node == nodes[].role 匹配取 region。
// 单出口(N=1)时只 1 个 node，前端不读 nodes 也不坏（零回归）。
type RealmNodeSummary struct {
	Role     string `json:"role"` // primary / backup
	Region   string `json:"region,omitempty"`
	EgressID string `json:"egress_id"`
}

// UpdateVPNUserRequest is for updating VPN user (extend/upgrade)
type UpdateVPNUserRequest struct {
	TrafficLimit int64  `json:"traffic_limit,omitempty"` // New traffic limit in bytes
	ExtendDays   int    `json:"extend_days,omitempty"`   // Days to extend
	PlanTier     string `json:"plan_tier,omitempty"`     // New plan tier
}
