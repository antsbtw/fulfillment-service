package service

import (
	"context"
	"fmt"
	"time"

	"github.com/wenwu/saas-platform/fulfillment-service/internal/client"
	"github.com/wenwu/saas-platform/fulfillment-service/internal/models"
)

// ConsumptionService 回答"某用户的某一笔 Apple 购买交付了没有、用了多少",
// 供 payment-service 回报 Apple 退款申请(payment-service docs/APPLE_CONSUMPTION_RESPONSE_DESIGN.md §4.2)。
// 只读:不改任何开通状态。

type appleVPNLister interface {
	ListByUserChannelFace(ctx context.Context, userID, channel, face string) ([]*models.VPNProvision, error)
}

type otunUserGetter interface {
	GetUser(ctx context.Context, uuid string) (*client.VPNUserInfo, error)
}

type hostingAliveLister interface {
	ListAliveSince(ctx context.Context, userID string, since time.Time) ([]*models.HostingProvision, error)
}

type hostingTrafficReader interface {
	GetNodeTraffic(ctx context.Context, nodeID string) (*client.NodeTraffic, error)
}

type ConsumptionService struct {
	vpn     appleVPNLister
	otun    otunUserGetter
	hosting hostingAliveLister
	traffic hostingTrafficReader
	now     func() time.Time
}

func NewConsumptionService(vpn appleVPNLister, otun otunUserGetter, hosting hostingAliveLister, traffic hostingTrafficReader) *ConsumptionService {
	return &ConsumptionService{vpn: vpn, otun: otun, hosting: hosting, traffic: traffic, now: time.Now}
}

// ConsumptionQuery 一笔 Apple 交易。PurchaseAt / PeriodEnd 取自这笔交易本身(不是首购日)。
type ConsumptionQuery struct {
	UserID      string
	App         string // otun | obox
	ServiceType string // vpn | hosting | license
	PlanTier    string
	PurchaseAt  time.Time
	PeriodEnd   time.Time // 自动续期订阅的本期到期;非订阅可为零值
	TrafficGB   int
}

const (
	// 开通写库可能比 Apple 的 purchaseDate 早一点点(verify 先到、时钟差),留 10 分钟余量。
	consumptionWriteSlack = 10 * time.Minute
	// 本笔开通后的到期至少要到 Apple 本期到期前 7 天,才算"开的是这一期"
	// (与 ca28738 平台同期重放判据同一口径)。
	consumptionPeriodSlack = 7 * 24 * time.Hour
	consumptionGiB         = int64(1) << 30
)

func (s *ConsumptionService) Get(ctx context.Context, q ConsumptionQuery) (*models.ConsumptionResponse, error) {
	if q.UserID == "" || q.PurchaseAt.IsZero() {
		return nil, fmt.Errorf("user_id and purchase_at are required")
	}
	switch {
	case q.App == "otun" && q.ServiceType == "vpn":
		return s.otunVPN(ctx, q)
	case q.App == "obox" && q.ServiceType == "hosting":
		return s.oboxHosting(ctx, q)
	case q.App == "obox" && q.ServiceType == "license":
		return &models.ConsumptionResponse{ResourceType: "obox_pro", Supported: false,
			Detail: "OBox Pro 权益不在 fulfillment 开通,暂不回报"}, nil
	default:
		return &models.ConsumptionResponse{ResourceType: q.App + "_" + q.ServiceType, Supported: false,
			Detail: "unknown product"}, nil
	}
}

// otunVPN:按这一笔交易判交付(设计 v0.2)——账号上别的渠道的流量不算本笔交付。
func (s *ConsumptionService) otunVPN(ctx context.Context, q ConsumptionQuery) (*models.ConsumptionResponse, error) {
	face := models.MapPlanToProductFace(q.PlanTier)
	resp := &models.ConsumptionResponse{ResourceType: "otun_" + face, Supported: true}
	limit := int64(q.TrafficGB) * consumptionGiB
	resp.LimitBytes = limit

	rows, err := s.vpn.ListByUserChannelFace(ctx, q.UserID, "apple", face)
	if err != nil {
		return nil, fmt.Errorf("list vpn provisions: %w", err)
	}
	hit := matchAppleDelivery(rows, q.PurchaseAt, q.PeriodEnd)
	if hit == nil {
		resp.UndeliveredReason = "not_provisioned"
		resp.Detail = fmt.Sprintf("%d apple %s rows, none written for this purchase", len(rows), face)
		return resp, nil
	}
	resp.Delivered = true
	at := hit.UpdatedAt
	if at.Before(q.PurchaseAt) {
		at = q.PurchaseAt
	}
	resp.DeliveredAt = &at
	resp.ActiveDays = daysBetween(at, s.now())
	if limit == 0 {
		limit = hit.TrafficLimit
		resp.LimitBytes = limit
	}
	if hit.OtunUUID == nil || *hit.OtunUUID == "" {
		return nil, fmt.Errorf("delivered row %s has no otun uuid", hit.ID)
	}
	live, err := s.otun.GetUser(ctx, *hit.OtunUUID)
	if err != nil {
		// 取不到用量就不回报(设计 §4.6),不能拿 0 冒充
		return nil, fmt.Errorf("otun GetUser: %w", err)
	}
	// Apple 开通是从头开一期(已用清零),本笔开通后的用量全算本笔;封顶为本笔额度
	used := live.TrafficUsed
	if limit > 0 && used > limit {
		used = limit
	}
	resp.UsedBytes = used
	resp.UsagePercent = percent(used, limit)
	resp.Detail = fmt.Sprintf("row %s status=%s current=%v", hit.ID, hit.Status, hit.IsCurrent)
	return resp, nil
}

// matchAppleDelivery 找"为这一笔购买写入"的 apple 行:写入时间不早于本笔购买、到期覆盖本期。
func matchAppleDelivery(rows []*models.VPNProvision, purchaseAt, periodEnd time.Time) *models.VPNProvision {
	for _, r := range rows {
		if r.UpdatedAt.Before(purchaseAt.Add(-consumptionWriteSlack)) {
			continue
		}
		if !periodEnd.IsZero() {
			if r.ExpireAt == nil || r.ExpireAt.Before(periodEnd.Add(-consumptionPeriodSlack)) {
				continue
			}
		}
		return r
	}
	return nil
}

// oboxHosting:本笔购买之后有任何一台托管机就绪并存活过 = 已交付;
// 用量取这些机器读数的最大值(换区新机继承本周期已用,最大值即本周期累计)。
func (s *ConsumptionService) oboxHosting(ctx context.Context, q ConsumptionQuery) (*models.ConsumptionResponse, error) {
	resp := &models.ConsumptionResponse{ResourceType: "obox_hosting", Supported: true}
	nodes, err := s.hosting.ListAliveSince(ctx, q.UserID, q.PurchaseAt)
	if err != nil {
		return nil, err
	}
	var firstReady *time.Time
	var ready []*models.HostingProvision
	for _, n := range nodes {
		if n.ReadyAt == nil {
			continue
		}
		ready = append(ready, n)
		if firstReady == nil || n.ReadyAt.Before(*firstReady) {
			t := *n.ReadyAt
			firstReady = &t
		}
	}
	resp.LimitBytes = int64(q.TrafficGB) * consumptionGiB
	if len(ready) == 0 {
		if len(nodes) == 0 {
			resp.UndeliveredReason = "no_node"
		} else {
			resp.UndeliveredReason = "node_never_ready"
		}
		resp.Detail = fmt.Sprintf("%d nodes since purchase, none ready", len(nodes))
		return resp, nil
	}
	resp.Delivered = true
	at := *firstReady
	if at.Before(q.PurchaseAt) {
		at = q.PurchaseAt
	}
	resp.DeliveredAt = &at
	resp.ActiveDays = daysBetween(at, s.now())

	var used, nodeLimit int64
	readOK := 0
	for _, n := range ready {
		if n.HostingNodeID == "" || s.traffic == nil {
			continue
		}
		t, terr := s.traffic.GetNodeTraffic(ctx, n.HostingNodeID)
		if terr != nil {
			continue // 已删机器 hosting 可能读不到,只要有一台读到就行
		}
		readOK++
		if t.TrafficUsed > used {
			used = t.TrafficUsed
		}
		if t.TrafficLimit > nodeLimit {
			nodeLimit = t.TrafficLimit
		}
	}
	if readOK == 0 {
		return nil, fmt.Errorf("hosting traffic unavailable for %d ready nodes", len(ready))
	}
	if resp.LimitBytes == 0 {
		resp.LimitBytes = nodeLimit
	}
	resp.UsedBytes = used
	resp.UsagePercent = percent(used, resp.LimitBytes)
	resp.Detail = fmt.Sprintf("%d ready nodes since purchase, traffic read from %d", len(ready), readOK)
	return resp, nil
}

func percent(used, limit int64) float64 {
	if limit <= 0 {
		return 0
	}
	p := float64(used) / float64(limit) * 100
	if p > 100 {
		p = 100
	}
	return p
}

func daysBetween(from, to time.Time) int {
	if !to.After(from) {
		return 0
	}
	return int(to.Sub(from).Hours() / 24)
}
