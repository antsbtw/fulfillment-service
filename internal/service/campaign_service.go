package service

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/wenwu/saas-platform/fulfillment-service/internal/client"
	"github.com/wenwu/saas-platform/fulfillment-service/internal/models"
	"github.com/wenwu/saas-platform/fulfillment-service/internal/repository"
)

// ============================================================================
// 第三产品面 campaign（营销活动 profile）——document/marketing-campaign/CAMPAIGN_PROFILE_DESIGN_OUTLINE.md §5.1
//
// 与 basic/residential 零交集：
//   - 独立 vpn_provisions 行（product_face='campaign'，每 user 至多一条 current）+ 独立 otun 账号
//     （otun users (auth_user_id,'campaign')，uuid 独立，节点面 standard）；
//   - 不进形态 B 记账（entitlement_*），不参与 active_class / profiles[]；
//   - 叠加：同 user 再次领取 → expire_at = max(now, expire_at) + days，traffic_limit += bytes；
//     到期后再领 → 从 now 起算、流量基线重置（otun 走 POST /api/users 复位 traffic_used）；
//   - 撤销：从活动账号扣减（下限 0/now），只动活动账号；
//   - 下发：仅 /vpn/all 且请求带 X-Client-Capabilities: campaign-profile 时追加 campaign 元素（门控）；
//   - cleanup：到期 + 保留期（默认 7 天）→ is_current=false + otun 账号 disable。
// ============================================================================

// ClientCapabilities 是请求头 X-Client-Capabilities（逗号分隔）解析后的能力集。
type ClientCapabilities map[string]bool

// CapabilityCampaignProfile 契约 §2 能力名（冻结）。
const CapabilityCampaignProfile = "campaign-profile"

// ParseClientCapabilities 解析 "a, b,c" → {a,b,c}；空 → 空集（老客户端）。
func ParseClientCapabilities(header string) ClientCapabilities {
	caps := ClientCapabilities{}
	for _, part := range strings.Split(header, ",") {
		if p := strings.ToLower(strings.TrimSpace(part)); p != "" {
			caps[p] = true
		}
	}
	return caps
}

// Has 报告能力是否声明；nil 集恒 false。
func (c ClientCapabilities) Has(name string) bool {
	return c != nil && c[name]
}

// campaignGrantStore 是 VPNService 依赖的 campaign_grants 读写子集（*repository.CampaignGrantRepository 满足）。
type campaignGrantStore interface {
	InsertIfAbsent(ctx context.Context, g *models.CampaignGrant) (bool, error)
	GetBySubscriptionID(ctx context.Context, subscriptionID string) (*models.CampaignGrant, error)
	MarkRevoked(ctx context.Context, subscriptionID string) (bool, error)
	ExpireActiveByUser(ctx context.Context, userID string) error
	AggregateActiveByUser(ctx context.Context, userID string) (*models.CampaignGrantAggregate, error)
}

// ErrNoCampaignProfile：该用户没有活动账号（撤销/读口 404 语义）。
var ErrNoCampaignProfile = errors.New("no campaign profile")

// ErrCampaignGrantsNotWired：campaign_grants 仓未注入（main.go 未接线）。
var ErrCampaignGrantsNotWired = errors.New("campaign grants store not wired")

// CampaignRevokeRequest 内部撤销请求（POST /api/internal/vpn/campaign/revoke）。
type CampaignRevokeRequest struct {
	UserID         string `json:"user_id" binding:"required"`
	SubscriptionID string `json:"subscription_id"` // 可选：给了则按 grant 幂等（同一 grant 只扣一次）
	Days           int    `json:"days"`
	TrafficBytes   int64  `json:"traffic_bytes"`
}

// CampaignRevokeResult 撤销结果。
type CampaignRevokeResult struct {
	UserID              string `json:"user_id"`
	ExpireAt            string `json:"expire_at"`
	TrafficLimit        int64  `json:"traffic_limit"`
	RevokedDays         int    `json:"revoked_days"`
	RevokedTrafficBytes int64  `json:"revoked_traffic_bytes"`
	Noop                bool   `json:"noop"`
}

// CampaignProfileView 内部读口（GET /api/internal/vpn/campaign/user/:user_id）——campaign-service 叠加闸 / me 用。
type CampaignProfileView struct {
	HasProfile            bool   `json:"has_profile"`
	Status                string `json:"status,omitempty"` // active | expired
	ExpireAt              string `json:"expire_at,omitempty"`
	TrafficLimit          int64  `json:"traffic_limit,omitempty"`
	TrafficUsed           int64  `json:"traffic_used,omitempty"`
	RemainingDays         int    `json:"remaining_days"`
	RemainingTrafficBytes int64  `json:"remaining_traffic_bytes"`
	ClaimsActive          int    `json:"claims_active"`
	GrantedDaysTotal      int    `json:"granted_days_total"`
	GrantedTrafficTotal   int64  `json:"granted_traffic_total"`
	LastClaimAt           string `json:"last_claim_at,omitempty"`
}

// isCampaignRequest：plan_tier=campaign（channel 同为 campaign 只是加固判据）。
func isCampaignRequest(req *models.ProvisionRequest) bool {
	return req != nil && (req.PlanTier == models.PlanTierCampaign || req.Channel == models.PlanTierCampaign)
}

// provisionCampaign 第三产品面开通/叠加（ProvisionVPNUser 对 plan_tier=campaign 的唯一入口；不走
// legacy / entitlement 两条既有路径）。幂等键 = subscription_id（campaign_grants 主键）。
func (s *VPNService) provisionCampaign(ctx context.Context, req *models.ProvisionRequest) (*models.ProvisionResponse, error) {
	if s.campaignGrants == nil {
		return nil, ErrCampaignGrantsNotWired
	}
	if req.SubscriptionID == "" {
		return nil, fmt.Errorf("campaign provision requires subscription_id")
	}
	days := req.ExpireDays
	if days <= 0 {
		days = 30 // 与 calculateExpireDays 缺省一致；campaign-service 恒传 days_valid
	}
	trafficBytes := req.TrafficLimit
	if trafficBytes <= 0 {
		return nil, fmt.Errorf("campaign provision requires traffic_limit")
	}
	log.Printf("[VPNService] Provisioning campaign face user=%s sub=%s days=%d traffic=%d",
		req.UserID, req.SubscriptionID, days, trafficBytes)

	// 0. 幂等：同一 subscription_id 已入账 → 直接返回该 user 的 campaign 行。
	if g, err := s.campaignGrants.GetBySubscriptionID(ctx, req.SubscriptionID); err == nil && g != nil {
		existing, _ := s.vpnRepo.GetCurrentByUserAndFace(ctx, req.UserID, models.ProductFaceCampaign)
		if existing != nil && existing.OtunUUID != nil {
			return &models.ProvisionResponse{
				ResourceID: existing.ID, Status: models.StatusActive, VPNUserID: *existing.OtunUUID,
				Message: "Already provisioned (idempotent, campaign grant)",
			}, nil
		}
	} else if err != nil && !errors.Is(err, repository.ErrNotFound) {
		return nil, fmt.Errorf("lookup campaign grant: %w", err)
	}

	now := time.Now()
	existing, _ := s.vpnRepo.GetCurrentByUserAndFace(ctx, req.UserID, models.ProductFaceCampaign)

	var (
		vp          *models.VPNProvision
		otunUUID    string
		expireAt    time.Time
		limitBytes  int64
		freshPeriod bool
		created     bool
	)
	if existing != nil && existing.OtunUUID != nil && *existing.OtunUUID != "" {
		// 叠加 / 到期后再领
		otunUUID = *existing.OtunUUID
		if existing.ExpireAt != nil && existing.ExpireAt.After(now) {
			expireAt = existing.ExpireAt.AddDate(0, 0, days)
			limitBytes = existing.TrafficLimit + trafficBytes
		} else {
			// 到期后再领：从 now 起算、流量重置基线（旧 grant 标 expired，otun 账号复位 traffic_used）
			freshPeriod = true
			expireAt = now.AddDate(0, 0, days)
			limitBytes = trafficBytes
		}
		if freshPeriod {
			if _, err := s.otunClient.CreateUser(ctx, &client.CreateVPNUserRequest{
				UUID:         otunUUID, // 兼容携带；otun 按 (auth_user_id, product_face) 定位并复位周期
				AuthUserID:   req.UserID,
				Email:        req.UserEmail,
				Protocols:    []string{"vless", "shadowsocks"},
				SSPassword:   generateRandomPassword(16),
				TrafficLimit: limitBytes,
				ExpireAt:     expireAt.Format(time.RFC3339),
				ServiceTier:  models.ServiceTierStandard,
				ProductFace:  models.ProductFaceCampaign,
			}); err != nil {
				return nil, fmt.Errorf("failed to reset campaign VPN user in otun-manager: %w", err)
			}
			if err := s.campaignGrants.ExpireActiveByUser(ctx, req.UserID); err != nil {
				log.Printf("[VPNService] Warning: expire old campaign grants failed user=%s: %v", req.UserID, err)
			}
		} else {
			enabled := true
			if err := s.otunClient.UpdateUser(ctx, otunUUID, &client.UpdateVPNUserRequest{
				TrafficLimit: limitBytes,
				ExpireAt:     expireAt.Format(time.RFC3339),
				Enabled:      &enabled,
			}); err != nil {
				return nil, fmt.Errorf("failed to stack campaign VPN user in otun-manager: %w", err)
			}
		}
		existing.SubscriptionID = req.SubscriptionID
		existing.Channel = models.PlanTierCampaign
		existing.PlanTier = models.PlanTierCampaign
		existing.ServiceTier = models.ServiceTierStandard
		existing.ProductFace = models.ProductFaceCampaign
		existing.Status = models.VPNProvisionStatusActive
		existing.IsCurrent = true
		existing.TrafficLimit = limitBytes
		existing.ExpireAt = &expireAt
		if req.UserEmail != "" {
			existing.Email = req.UserEmail
		}
		if err := s.vpnRepo.Update(ctx, existing); err != nil {
			return nil, fmt.Errorf("failed to update campaign provision: %w", err)
		}
		vp = existing
	} else {
		// 首次：新 uuid、新 otun 账号（product_face=campaign）、新行
		expireAt = now.AddDate(0, 0, days)
		limitBytes = trafficBytes
		vpnUserID := uuid.New().String()
		otunResp, err := s.otunClient.CreateUser(ctx, &client.CreateVPNUserRequest{
			UUID:         vpnUserID,
			Email:        req.UserEmail,
			AuthUserID:   req.UserID,
			Protocols:    []string{"vless", "shadowsocks"},
			SSPassword:   generateRandomPassword(16),
			TrafficLimit: limitBytes,
			ExpireAt:     expireAt.Format(time.RFC3339),
			ServiceTier:  models.ServiceTierStandard,
			ProductFace:  models.ProductFaceCampaign,
		})
		if err != nil {
			if s.logRepo != nil {
				s.logRepo.LogAction(ctx, "", "vpn", "campaign_user_create_failed", "failed", err.Error())
			}
			return nil, fmt.Errorf("failed to create campaign VPN user in otun-manager: %w", err)
		}
		otunUUID = otunResp.UUID
		if otunUUID == "" {
			otunUUID = vpnUserID
		}
		if existing != nil {
			// 历史残留行（无 uuid）：让位
			_ = s.vpnRepo.MarkNotCurrent(ctx, existing.ID)
		}
		vp = &models.VPNProvision{
			ID:             uuid.New().String(),
			UserID:         req.UserID,
			SubscriptionID: req.SubscriptionID,
			Channel:        models.PlanTierCampaign,
			BusinessType:   models.BusinessTypeGift,
			ServiceTier:    models.ServiceTierStandard,
			ProductFace:    models.ProductFaceCampaign,
			OtunUUID:       &otunUUID,
			PlanTier:       models.PlanTierCampaign,
			Status:         models.VPNProvisionStatusActive,
			TrafficLimit:   limitBytes,
			TrafficUsed:    0,
			ExpireAt:       &expireAt,
			Email:          req.UserEmail,
			DeviceID:       req.DeviceID,
			IsCurrent:      true,
		}
		if err := s.vpnRepo.Create(ctx, vp); err != nil {
			_ = s.otunClient.DeleteUser(ctx, otunUUID)
			return nil, fmt.Errorf("failed to save campaign provision: %w", err)
		}
		created = true
	}

	// 入账（幂等键 subscription_id）
	if _, err := s.campaignGrants.InsertIfAbsent(ctx, &models.CampaignGrant{
		SubscriptionID: req.SubscriptionID, UserID: req.UserID, ChannelSubID: req.ChannelSubID,
		Days: days, TrafficBytes: trafficBytes,
	}); err != nil {
		log.Printf("[VPNService] Warning: record campaign grant failed sub=%s: %v", req.SubscriptionID, err)
	}

	action, msg := "campaign_user_stacked", "Campaign VPN user updated (stacked)"
	if created {
		action, msg = "campaign_user_created", "Campaign VPN user created successfully"
	} else if freshPeriod {
		action, msg = "campaign_user_renewed", "Campaign VPN user renewed (fresh period)"
	}
	if s.logRepo != nil {
		s.logRepo.LogActionWithMetadata(ctx, vp.ID, "vpn", action, "active", msg, map[string]interface{}{
			"vpn_user_id": otunUUID, "plan_tier": models.PlanTierCampaign, "product_face": models.ProductFaceCampaign,
			"days": days, "traffic_bytes": trafficBytes, "expire_at": expireAt.Format(time.RFC3339), "traffic_limit": limitBytes,
			"channel_sub_id": req.ChannelSubID,
		})
	}
	s.notifyVPNActive(ctx, req.SubscriptionID, vp.ID, otunUUID)
	return &models.ProvisionResponse{ResourceID: vp.ID, Status: models.StatusActive, VPNUserID: otunUUID, Message: msg}, nil
}

// RevokeCampaign 从活动账号扣减 days / traffic（下限 now / 0），同步 otun；只动活动账号。
// subscription_id 给了则按 grant 幂等（已 revoked / 不存在 → noop 不扣）。
func (s *VPNService) RevokeCampaign(ctx context.Context, req *CampaignRevokeRequest) (*CampaignRevokeResult, error) {
	vp, err := s.vpnRepo.GetCurrentByUserAndFace(ctx, req.UserID, models.ProductFaceCampaign)
	if err != nil && !errors.Is(err, repository.ErrNotFound) {
		return nil, fmt.Errorf("lookup campaign provision: %w", err)
	}
	if vp == nil {
		return nil, ErrNoCampaignProfile
	}
	days, bytes := req.Days, req.TrafficBytes
	if req.SubscriptionID != "" && s.campaignGrants != nil {
		g, gerr := s.campaignGrants.GetBySubscriptionID(ctx, req.SubscriptionID)
		if gerr != nil && !errors.Is(gerr, repository.ErrNotFound) {
			return nil, fmt.Errorf("lookup campaign grant: %w", gerr)
		}
		if g == nil || g.Status != "active" {
			// 未入账 / 已撤销 / 已随周期作废：无可扣
			return &CampaignRevokeResult{UserID: req.UserID, ExpireAt: fmtTime(vp.ExpireAt), TrafficLimit: vp.TrafficLimit, Noop: true}, nil
		}
		if days <= 0 {
			days = g.Days
		}
		if bytes <= 0 {
			bytes = g.TrafficBytes
		}
		// ★验收 F4：MarkRevoked 放到 otun 同步成功之后（见下）——否则 otun PUT 失败时 grant 已标 revoked，
		// subscription-service 重试进来变 Noop，otun 仍全额。
	}
	now := time.Now()
	newExpire := now
	if vp.ExpireAt != nil {
		newExpire = vp.ExpireAt.AddDate(0, 0, -days)
		if newExpire.Before(now) {
			newExpire = now
		}
	}
	newLimit := vp.TrafficLimit - bytes
	if newLimit < 0 {
		newLimit = 0
	}
	// otun：traffic_limit=0 在 otun-manager 语义是"不限"（isTrafficExhausted 只在 >0 时判），且 client
	// 结构 omitempty 会丢 0 → 下发 1 字节 = 事实上已耗尽。行内仍记真值 0。
	otunLimit := newLimit
	if otunLimit <= 0 {
		otunLimit = 1
	}
	if vp.OtunUUID != nil && *vp.OtunUUID != "" {
		if err := s.otunClient.UpdateUser(ctx, *vp.OtunUUID, &client.UpdateVPNUserRequest{
			TrafficLimit: otunLimit, ExpireAt: newExpire.Format(time.RFC3339),
		}); err != nil {
			// otun 未扣成 → 整个撤销失败（不标 grant、不改行），让 subscription-service 回滚状态并由
			// campaign-service 重试（验收 F4）。
			return nil, fmt.Errorf("sync revoked campaign quota to otun failed: %w", err)
		}
	}
	// otun 已扣成，再按 grant 幂等标记（并发重复撤销：第二个到这里发现已 revoked → 只回 Noop，不再改行）。
	if req.SubscriptionID != "" && s.campaignGrants != nil {
		if ok, merr := s.campaignGrants.MarkRevoked(ctx, req.SubscriptionID); merr != nil {
			return nil, fmt.Errorf("mark campaign grant revoked: %w", merr)
		} else if !ok {
			return &CampaignRevokeResult{UserID: req.UserID, ExpireAt: fmtTime(vp.ExpireAt), TrafficLimit: vp.TrafficLimit, Noop: true}, nil
		}
	}
	vp.ExpireAt = &newExpire
	vp.TrafficLimit = newLimit
	if err := s.vpnRepo.Update(ctx, vp); err != nil {
		return nil, fmt.Errorf("update campaign provision: %w", err)
	}
	if s.logRepo != nil {
		s.logRepo.LogActionWithMetadata(ctx, vp.ID, "vpn", "campaign_user_revoked", "active", "Campaign grant revoked",
			map[string]interface{}{"days": days, "traffic_bytes": bytes, "subscription_id": req.SubscriptionID,
				"expire_at": newExpire.Format(time.RFC3339), "traffic_limit": newLimit})
	}
	return &CampaignRevokeResult{UserID: req.UserID, ExpireAt: newExpire.Format(time.RFC3339), TrafficLimit: newLimit,
		RevokedDays: days, RevokedTrafficBytes: bytes}, nil
}

// GetCampaignProfile 内部读口：活动账号现状 + grant 聚合（campaign-service 叠加闸 / me / preview 用）。
func (s *VPNService) GetCampaignProfile(ctx context.Context, userID string) (*CampaignProfileView, error) {
	vp, err := s.vpnRepo.GetCurrentByUserAndFace(ctx, userID, models.ProductFaceCampaign)
	if err != nil && !errors.Is(err, repository.ErrNotFound) {
		return nil, fmt.Errorf("lookup campaign provision: %w", err)
	}
	if vp == nil {
		return &CampaignProfileView{HasProfile: false}, nil
	}
	view := &CampaignProfileView{HasProfile: true, Status: "active", TrafficLimit: vp.TrafficLimit, TrafficUsed: vp.TrafficUsed}
	now := time.Now()
	if vp.ExpireAt != nil {
		view.ExpireAt = vp.ExpireAt.UTC().Format(time.RFC3339)
		if vp.ExpireAt.After(now) {
			view.RemainingDays = int(math.Ceil(vp.ExpireAt.Sub(now).Hours() / 24))
		} else {
			view.Status = models.VPNProvisionStatusExpired
		}
	}
	// 用量真源 otun（users.traffic_used）；失败退回行值
	if vp.OtunUUID != nil && *vp.OtunUUID != "" {
		if sync, serr := s.otunClient.SyncUser(ctx, *vp.OtunUUID); serr == nil && sync != nil {
			if sync.TrafficUsed > 0 {
				view.TrafficUsed = sync.TrafficUsed
			}
			if sync.TrafficLimit > 0 {
				view.TrafficLimit = sync.TrafficLimit
			}
		}
	}
	if view.Status == models.VPNProvisionStatusExpired {
		view.RemainingTrafficBytes = 0
	} else if rem := view.TrafficLimit - view.TrafficUsed; rem > 0 {
		view.RemainingTrafficBytes = rem
	}
	if s.campaignGrants != nil {
		if agg, aerr := s.campaignGrants.AggregateActiveByUser(ctx, userID); aerr == nil && agg != nil {
			view.ClaimsActive = agg.ClaimsActive
			view.GrantedDaysTotal = agg.GrantedDaysTotal
			view.GrantedTrafficTotal = agg.GrantedTrafficTotal
			if agg.LastClaimAt != nil {
				view.LastClaimAt = agg.LastClaimAt.UTC().Format(time.RFC3339)
			}
		}
	}
	return view, nil
}

// buildCampaignElement 构造 /vpn/all 的 campaign 元素（契约 §3）。无活动账号 / 已清理 → nil（C5）。
// 到期但仍在保留期（行仍 is_current）→ status="expired"。protocols 取 otun GET /api/users/:uuid/sync
// （按 uuid，天然是活动账号自己的凭证，H2）；取不到（账号 disabled / 抖动）→ protocols 空、不失败。
func (s *VPNService) buildCampaignElement(ctx context.Context, userID string) *models.VPNSubscribeResponse {
	vp, err := s.vpnRepo.GetCurrentByUserAndFace(ctx, userID, models.ProductFaceCampaign)
	if err != nil || vp == nil {
		return nil
	}
	if vp.Status != models.VPNProvisionStatusActive && vp.Status != models.VPNProvisionStatusExpired {
		return nil // disabled/revoked/converted：不下发
	}
	now := time.Now()
	status := "active"
	if vp.ExpireAt != nil && !vp.ExpireAt.After(now) {
		status = models.VPNProvisionStatusExpired
	}
	trafficUsed, trafficLimit := vp.TrafficUsed, vp.TrafficLimit
	expireAt := fmtTime(vp.ExpireAt)
	var protocols []models.VPNProtocol
	var exitCountry string
	var smart = []byte(nil)
	if vp.OtunUUID != nil && *vp.OtunUUID != "" {
		if sync, serr := s.otunClient.SyncUser(ctx, *vp.OtunUUID); serr == nil && sync != nil {
			for _, p := range sync.Protocols {
				protocols = append(protocols, models.VPNProtocol{Protocol: p.Protocol, URL: p.URL, Node: p.Node})
			}
			if sync.TrafficUsed > 0 {
				trafficUsed = sync.TrafficUsed
			}
			if sync.TrafficLimit > 0 {
				trafficLimit = sync.TrafficLimit
			}
			if sync.ExpireAt != "" {
				expireAt = sync.ExpireAt
			}
			exitCountry = sync.ExitCountry
			smart = sync.SmartStrategy
		} else if serr != nil {
			log.Printf("[VPNService] campaign element: otun sync failed user=%s uuid=%s: %v", userID, *vp.OtunUUID, serr)
		}
	}
	resp := &models.VPNSubscribeResponse{
		Status:        status,
		Channel:       models.PlanTierCampaign,
		PlanTier:      models.PlanTierCampaign,
		ServiceTier:   models.ServiceTierStandard, // 契约 C3：一期固定 standard；端上按 plan_tier/profile_class 分键
		SubscribeURL:  fmt.Sprintf("%s/api/v1/my/vpn/subscribe", s.cfg.Services.PublicBaseURL),
		DeviceID:      userID,
		Protocols:     protocols,
		TrafficLimit:  trafficLimit,
		TrafficUsed:   trafficUsed,
		ExpireAt:      expireAt,
		Message:       "Campaign VPN configuration retrieved successfully",
		ExitCountry:   exitCountry,
		SmartStrategy: smart,
		ConfigVersion: computeConfigVersion(protocols, smart),
		ProfileClass:  models.ProductFaceCampaign,
		Campaign:      &models.CampaignInfo{},
	}
	if s.campaignGrants != nil {
		if agg, aerr := s.campaignGrants.AggregateActiveByUser(ctx, userID); aerr == nil && agg != nil {
			resp.Campaign.ClaimsActive = agg.ClaimsActive
			resp.Campaign.GrantedDaysTotal = agg.GrantedDaysTotal
			resp.Campaign.GrantedTrafficTotal = agg.GrantedTrafficTotal
			if agg.LastClaimAt != nil {
				resp.Campaign.LastClaimAt = agg.LastClaimAt.UTC().Format(time.RFC3339)
			}
		}
	}
	if s.cfg != nil && (s.cfg.Campaign.StackHardMaxDays > 0 || s.cfg.Campaign.StackHardMaxTrafficGB > 0) {
		resp.Campaign.StackLimit = &models.CampaignStackLimit{
			MaxDays:    s.cfg.Campaign.StackHardMaxDays,
			MaxTraffic: int64(s.cfg.Campaign.StackHardMaxTrafficGB) * 1024 * 1024 * 1024,
		}
	}
	return resp
}

// CleanupExpiredCampaignFaces 到期 + 保留期后的活动账号：is_current=false + status=expired + otun disable。
// 供调度器（CampaignCleanupScheduler）周期调用；返回处理条数。
func (s *VPNService) CleanupExpiredCampaignFaces(ctx context.Context, retention time.Duration, limit int) int {
	lister, ok := s.vpnRepo.(interface {
		ListExpiredCampaignRows(ctx context.Context, before time.Time, limit int) ([]*models.VPNProvision, error)
	})
	if !ok {
		return 0
	}
	rows, err := lister.ListExpiredCampaignRows(ctx, time.Now().Add(-retention), limit)
	if err != nil {
		log.Printf("[VPNService] campaign cleanup: list failed: %v", err)
		return 0
	}
	n := 0
	for _, vp := range rows {
		if vp.OtunUUID != nil && *vp.OtunUUID != "" {
			if err := s.otunClient.DisableUser(ctx, *vp.OtunUUID); err != nil {
				log.Printf("[VPNService] campaign cleanup: disable otun user %s failed: %v", *vp.OtunUUID, err)
			}
		}
		vp.Status = models.VPNProvisionStatusExpired
		vp.IsCurrent = false
		if err := s.vpnRepo.Update(ctx, vp); err != nil {
			log.Printf("[VPNService] campaign cleanup: update row %s failed: %v", vp.ID, err)
			continue
		}
		if s.campaignGrants != nil {
			_ = s.campaignGrants.ExpireActiveByUser(ctx, vp.UserID)
		}
		if s.logRepo != nil {
			s.logRepo.LogAction(ctx, vp.ID, "vpn", "campaign_user_cleaned", "expired", "campaign face expired + retention elapsed")
		}
		n++
	}
	if n > 0 {
		log.Printf("[VPNService] campaign cleanup: %d face(s) cleaned", n)
	}
	return n
}

func fmtTime(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// CampaignCleanupScheduler 仿 EntitlementScheduler：定时清理到期+保留期的活动账号。
type CampaignCleanupScheduler struct {
	svc       *VPNService
	interval  time.Duration
	retention time.Duration
}

// NewCampaignCleanupScheduler 创建调度器。
func NewCampaignCleanupScheduler(svc *VPNService, interval, retention time.Duration) *CampaignCleanupScheduler {
	return &CampaignCleanupScheduler{svc: svc, interval: interval, retention: retention}
}

// Start 阻塞运行；ctx 取消即停。
func (c *CampaignCleanupScheduler) Start(ctx context.Context) {
	log.Printf("[CampaignCleanupScheduler] Started (interval=%v, retention=%v)", c.interval, c.retention)
	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Println("[CampaignCleanupScheduler] Stopped")
			return
		case <-ticker.C:
			if c.svc != nil {
				c.svc.CleanupExpiredCampaignFaces(ctx, c.retention, 200)
			}
		}
	}
}
