package models

import "time"

// RefundRevocation 一笔服务类订单退款后的停服扣减记录(fulfillment.vpn_refund_revocations)。
// ref 为幂等键(payment 侧的付款 id);同一 ref 只扣一次,重复调用返回首次结果。
type RefundRevocation struct {
	Ref             string     `json:"ref"`
	UserID          string     `json:"user_id"`
	PlanTier        string     `json:"plan_tier"`
	ProvisionID     string     `json:"provision_id,omitempty"`
	Days            int        `json:"days"`
	TrafficBytes    int64      `json:"traffic_bytes"`
	OldExpireAt     *time.Time `json:"old_expire_at,omitempty"`
	NewExpireAt     *time.Time `json:"new_expire_at,omitempty"`
	NewTrafficLimit int64      `json:"new_traffic_limit"`
	Disabled        bool       `json:"disabled"`
	Noop            bool       `json:"noop"`
	Reason          string     `json:"reason,omitempty"`
	CreatedAt       time.Time  `json:"created_at"`
}
