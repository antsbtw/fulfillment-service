package service

import (
	"context"
	"fmt"
	"log"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/wenwu/saas-platform/fulfillment-service/internal/client"
	"github.com/wenwu/saas-platform/fulfillment-service/internal/models"
	"github.com/wenwu/saas-platform/fulfillment-service/internal/repository"
)

// ==================== 订阅 / 订购 profile：折叠 / 裁决 / 同步 / 投影 ====================
//
// 依据：document/subscription-entitlement/SUBSCRIPTION_PURCHASE_PROFILE_RULES.md（规则 v2）、
//       VPN_PROFILES_CONTRACT_DRAFT.md（契约 v0.2）、BACKEND_CHANGE_LIST.md §2.2。
// 文件名用 entitlement_profile_service.go：本仓已有 entitlement_service.go（gift/trial 配置的
// EntitlementService），不能重名。
//
// 四个函数全部幂等；Resolve+Sync 可被事件、调度器、读请求重复调用。
// 裁决【只看时间不看流量】（规则 §2 非目标）：流量耗尽不切换、不借桶、不冻结天数。

// entitlementStore 是记账层依赖的 repo 子集（*repository.EntitlementProfileRepository 满足）。
type entitlementStore interface {
	GetProfilesByUserFace(ctx context.Context, userID, face string) ([]*models.EntitlementProfile, error)
	UpsertProfile(ctx context.Context, p *models.EntitlementProfile) error
	CreateEntry(ctx context.Context, e *models.EntitlementEntry) (bool, error)
	ListEntriesByProfile(ctx context.Context, profileID string) ([]*models.EntitlementEntry, error)
	MarkEntryRevoked(ctx context.Context, id string, at time.Time) error
	ListFacesDueForResolve(ctx context.Context, horizon time.Time, limit int) ([][2]string, error)
}

// projectionStore 是 vpn_provisions 投影行的读写子集（*repository.VPNProvisionRepository 满足）。
type projectionStore interface {
	GetCurrentByUserAndServicePartition(ctx context.Context, userID string, isResidential bool) (*models.VPNProvision, error)
	UpdateProjection(ctx context.Context, id string, expireAt *time.Time, trafficLimit int64, channel, activeClass string) error
}

// otunAccountGateway 是该面 otun 账号的读/写口（生产 = otunAccountAdapter 包 *client.OTunClient）。
type otunAccountGateway interface {
	// ReadUsage 读该面账号当前 traffic_used 真源（standard=users，residential=realm_users）。
	ReadUsage(ctx context.Context, otunUUID, face string) (int64, error)
	// Push 写 expire_at + traffic_limit 并显式 enabled=true；serviceTier 随投影行（套餐升降级时更新 tier，
	// 与 syncOtunUserQuota 同口径）。
	Push(ctx context.Context, otunUUID, face, serviceTier, authUserID, email string, trafficLimit int64, expireAt time.Time) error
}

// EntitlementProfileService 记账层服务。
type EntitlementProfileService struct {
	store      entitlementStore
	provisions projectionStore
	otun       otunAccountGateway
	// enabled = ENTITLEMENT_PROFILES_ENABLED：false 只影子写（ApplyEntry），Sync 不驱动 otun/投影。
	enabled bool
	// switchLead：订阅到期前多久开始"桥接推送"（≥ otun-manager cleanup 间隔 1h）。
	switchLead time.Duration
	now        func() time.Time

	// lastPush 记住每个 (user|face) 最近一次推给 otun 的 (expire, limit)：桥接推送的目标值与投影行
	// 故意不同（投影行仍是订阅到期），靠它避免桥接窗口内每分钟/每次读都重复 PUT。进程重启后
	// 最多多推一次（幂等）。
	pushMu   sync.Mutex
	lastPush map[string]pushState
}

type pushState struct {
	expire time.Time
	limit  int64
}

// NewEntitlementProfileService 生产构造。
func NewEntitlementProfileService(
	store *repository.EntitlementProfileRepository,
	provisions *repository.VPNProvisionRepository,
	otunClient *client.OTunClient,
	enabled bool,
	switchLead time.Duration,
) *EntitlementProfileService {
	return &EntitlementProfileService{
		store:      store,
		provisions: provisions,
		otun:       &otunAccountAdapter{c: otunClient},
		enabled:    enabled,
		switchLead: switchLead,
		now:        time.Now,
	}
}

// Enabled 开关是否打开（VPNService 据此选路）。
func (s *EntitlementProfileService) Enabled() bool { return s != nil && s.enabled }

// ==================== otun 账号适配 ====================

// otunAccountAdapter 把 *client.OTunClient 适配成 otunAccountGateway。
// ★residential 的 PUT /api/users/{uuid} 恒 404（用户已迁出 users 表），必须走 POST /api/users
// UPSERT（按 auth_user_id 定位、uuid 不变、enabled=true）——与 vpn_service.go syncOtunUserQuota 同口径。
type otunAccountAdapter struct{ c *client.OTunClient }

func (a *otunAccountAdapter) ReadUsage(ctx context.Context, otunUUID, face string) (int64, error) {
	if a == nil || a.c == nil {
		return 0, fmt.Errorf("otun client not configured")
	}
	if face == models.ServiceFaceResidential {
		// 与 buildSubscribeResponse residential 分支同一真源（realm_users，manager connect-url 下发）。
		resp, err := a.c.GetRealmConnectURL(ctx, otunUUID)
		if err != nil {
			return 0, err
		}
		if resp == nil {
			return 0, nil // 未分配出口：用量视为 0
		}
		return resp.TrafficUsed, nil
	}
	u, err := a.c.GetUser(ctx, otunUUID)
	if err != nil {
		return 0, err
	}
	return u.TrafficUsed, nil
}

func (a *otunAccountAdapter) Push(ctx context.Context, otunUUID, face, serviceTier, authUserID, email string, trafficLimit int64, expireAt time.Time) error {
	if a == nil || a.c == nil {
		return fmt.Errorf("otun client not configured")
	}
	if face == models.ServiceFaceResidential {
		_, err := a.c.CreateUser(ctx, &client.CreateVPNUserRequest{
			UUID:         otunUUID,
			AuthUserID:   authUserID,
			Email:        email,
			TrafficLimit: trafficLimit,
			ExpireAt:     expireAt.Format(time.RFC3339),
			ServiceTier:  models.ServiceTierResidential,
		})
		return err
	}
	enabled := true
	return a.c.UpdateUser(ctx, otunUUID, &client.UpdateVPNUserRequest{
		TrafficLimit: trafficLimit,
		ExpireAt:     expireAt.Format(time.RFC3339),
		Enabled:      &enabled,
		ServiceTier:  serviceTier, // 空则 manager 保留原值
	})
}

// ==================== 条目输入与分类 ====================

// EntryInput 是一笔权益条目的输入（由 ProvisionRequest 转换而来）。
type EntryInput struct {
	UserID      string
	ServiceFace string // standard | residential

	SubscriptionID string
	Channel        string
	ChannelSubID   string
	SourceEventID  string // 可空：空时按 subscription_id/period 派生幂等键
	PurchaseType   string // subscription | one_time | gift | trial（空则按 channel 推断）
	BusinessType   string

	Days        int   // 天数（一次性入桶；订阅/试用信息量）
	Traffic     int64 // 字节
	PeriodStart *time.Time
	PeriodEnd   *time.Time // 订阅/试用的到期；一次性可空
}

// classifyEntry 由 channel / purchase_type / channel_sub_id 得到 (class, kind, 归一化 purchase_type)。
// kind 映射见契约 §4.3；class 见规则 §2。
func classifyEntry(channel, purchaseType, businessType, channelSubID string) (class, kind, normalizedPT string) {
	ch := strings.ToLower(strings.TrimSpace(channel))
	pt := strings.ToLower(strings.TrimSpace(purchaseType))
	bt := strings.ToLower(strings.TrimSpace(businessType))

	if ch == "trial" || pt == models.PurchaseTypeTrial || bt == models.BusinessTypeTrial {
		return models.EntitlementClassTrial, models.EntryKindTrial, models.PurchaseTypeTrial
	}
	if pt == "" {
		// 老上游没带 purchase_type：按 channel 推断（apple/google/stripe 视为 recurring）
		switch ch {
		case "apple", "google", "stripe":
			pt = models.PurchaseTypeSubscription
		case "gift":
			pt = models.PurchaseTypeGift
		default:
			pt = models.PurchaseTypeOneTime
		}
		if bt == models.BusinessTypeGift {
			pt = models.PurchaseTypeGift
		}
	}

	switch ch {
	case "apple":
		kind = models.EntryKindApple
	case "google":
		kind = models.EntryKindGoogle
	case "stripe":
		if pt == models.PurchaseTypeSubscription {
			kind = models.EntryKindStripe
		} else {
			kind = models.EntryKindStripeOnetime
		}
	case "credit":
		if strings.HasPrefix(channelSubID, "campaign-claim-") {
			kind = models.EntryKindCampaign
		} else {
			kind = models.EntryKindCredit
		}
	case "gift":
		kind = models.EntryKindGift
	case "gift_card":
		kind = models.EntryKindGiftCard
	default:
		kind = models.EntryKindManual
	}

	if pt == models.PurchaseTypeSubscription {
		return models.EntitlementClassSubscription, kind, pt
	}
	// one_time / gift / 其它一律入订购桶（规则 §2：新来源不开新 profile）
	return models.EntitlementClassPurchase, kind, pt
}

// entryIdempotencyKey 派生 source_event_id：ProvisionRequest 不带事件 ID，用
//
//	订阅：sub:<subscription_id>|pe:<period_end>   —— 同一周期重放去重，续期（新 period_end）新条目
//	一次性/赠送：sub:<subscription_id>            —— 同一笔订购只入桶一次（重放/重触发不重复加天）
//	试用：sub:<subscription_id>|trial
func entryIdempotencyKey(class, subscriptionID string, periodEnd *time.Time, granted time.Time) string {
	switch class {
	case models.EntitlementClassSubscription:
		if periodEnd != nil {
			return "sub:" + subscriptionID + "|pe:" + periodEnd.UTC().Format(time.RFC3339)
		}
		return "sub:" + subscriptionID + "|day:" + granted.UTC().Format("2006-01-02")
	case models.EntitlementClassTrial:
		return "sub:" + subscriptionID + "|trial"
	default:
		return "sub:" + subscriptionID
	}
}

// ==================== ApplyEntry ====================

// ApplyEntry 把一笔条目折进对应 profile（幂等：条目唯一键冲突则不重复入账，返回 applied=false）。
//
//	subscription → 订阅 profile：expire = max(period_end)、流量按本期重置
//	one_time/gift → 订购桶：days_remaining += days、traffic_limit += traffic（生效中则 expire 顺延）
//	trial → trial profile
//	任何非 trial 条目到来 → 作废该面 trial profile（规则：任何付费到来即作废 trial）
func (s *EntitlementProfileService) ApplyEntry(ctx context.Context, in *EntryInput) (*models.EntitlementProfile, bool, error) {
	if s == nil {
		return nil, false, nil
	}
	if in.UserID == "" || in.ServiceFace == "" {
		return nil, false, fmt.Errorf("ApplyEntry: user_id and service_face required")
	}
	now := s.now()
	class, kind, pt := classifyEntry(in.Channel, in.PurchaseType, in.BusinessType, in.ChannelSubID)

	profiles, err := s.loadProfiles(ctx, in.UserID, in.ServiceFace)
	if err != nil {
		return nil, false, err
	}
	p := profiles[class]
	if p == nil {
		p = &models.EntitlementProfile{
			UserID: in.UserID, ServiceFace: in.ServiceFace, Class: class, Status: models.ProfileStatusNone,
		}
		// 先落 profile 拿到 id（entry 外键）
		if err := s.store.UpsertProfile(ctx, p); err != nil {
			return nil, false, err
		}
		profiles[class] = p
	}

	sourceEventID := in.SourceEventID
	if sourceEventID == "" {
		sourceEventID = entryIdempotencyKey(class, in.SubscriptionID, in.PeriodEnd, now)
	}
	periodEnd := in.PeriodEnd
	if class != models.EntitlementClassPurchase && periodEnd == nil {
		days := in.Days
		if days <= 0 {
			days = 30
		}
		t := now.AddDate(0, 0, days)
		periodEnd = &t
	}
	days := in.Days
	if class == models.EntitlementClassSubscription && periodEnd != nil {
		start := now
		if in.PeriodStart != nil && !in.PeriodStart.IsZero() {
			start = *in.PeriodStart
		}
		if d := int(math.Ceil(periodEnd.Sub(start).Hours() / 24)); d > 0 {
			days = d
		}
	}

	entry := &models.EntitlementEntry{
		ProfileID:      p.ID,
		SubscriptionID: in.SubscriptionID,
		Channel:        strings.ToLower(strings.TrimSpace(in.Channel)),
		ChannelSubID:   in.ChannelSubID,
		Kind:           kind,
		PurchaseType:   pt,
		Days:           days,
		Traffic:        in.Traffic,
		PeriodStart:    in.PeriodStart,
		PeriodEnd:      periodEnd,
		GrantedAt:      now,
		SourceEventID:  sourceEventID,
	}
	inserted, err := s.store.CreateEntry(ctx, entry)
	if err != nil {
		return nil, false, err
	}
	if !inserted {
		log.Printf("[Entitlement] entry already applied (channel=%s sub=%s key=%s), skip", entry.Channel, entry.ChannelSubID, sourceEventID)
		return p, false, nil
	}

	switch class {
	case models.EntitlementClassSubscription:
		if p.ExpireAt == nil || (periodEnd != nil && periodEnd.After(*p.ExpireAt)) {
			p.ExpireAt = periodEnd
		}
		p.TrafficLimit = in.Traffic // 本期额度（流量按期重置）
		if in.PeriodStart != nil && !in.PeriodStart.IsZero() {
			ps := *in.PeriodStart
			p.EffectiveFrom = &ps
		} else if p.EffectiveFrom == nil {
			p.EffectiveFrom = &now
		}
		if p.ExpireAt != nil && p.ExpireAt.After(now) {
			p.Status = models.ProfileStatusActive
		}
	case models.EntitlementClassPurchase:
		p.DaysRemaining += in.Days
		p.TrafficLimit += in.Traffic
		if p.Status == models.ProfileStatusActive && p.ExpireAt != nil {
			// 生效中追加：到期顺延（expire = active_since + 剩余天数 的等价增量形式）
			t := p.ExpireAt.AddDate(0, 0, in.Days)
			p.ExpireAt = &t
		} else if p.DaysRemaining > 0 && p.Status != models.ProfileStatusActive {
			p.Status = models.ProfileStatusWaiting // 是否立即生效由 Resolve 裁决
		}
	case models.EntitlementClassTrial:
		p.ExpireAt = periodEnd
		p.TrafficLimit = in.Traffic
		if p.EffectiveFrom == nil {
			p.EffectiveFrom = &now
		}
		if p.ExpireAt != nil && p.ExpireAt.After(now) {
			p.Status = models.ProfileStatusActive
		}
	}
	if err := s.store.UpsertProfile(ctx, p); err != nil {
		return nil, false, err
	}

	// 付费到来即作废 trial（规则 §2 约束；subscription-service 侧 SupersedeActiveTrial 同义）
	if class != models.EntitlementClassTrial {
		if tri := profiles[models.EntitlementClassTrial]; tri != nil && tri.Status == models.ProfileStatusActive {
			tri.Status = models.ProfileStatusExpired
			if tri.ExpireAt == nil || tri.ExpireAt.After(now) {
				tri.ExpireAt = &now
			}
			if err := s.store.UpsertProfile(ctx, tri); err != nil {
				log.Printf("[Entitlement] supersede trial profile failed user=%s face=%s: %v", in.UserID, in.ServiceFace, err)
			}
		}
	}
	return p, true, nil
}

// RevokeEntry 撤销一笔订购条目：标记 revoked_at，桶按条目天数/流量扣减（下限 0）。
// 生效中的桶按扣减后剩余重算到期（不早于 now）。本轮无发出方（撤销事件今天不到达 fulfillment），预留。
func (s *EntitlementProfileService) RevokeEntry(ctx context.Context, userID, face, channel, channelSubID string) (bool, error) {
	if s == nil {
		return false, nil
	}
	now := s.now()
	profiles, err := s.loadProfiles(ctx, userID, face)
	if err != nil {
		return false, err
	}
	p := profiles[models.EntitlementClassPurchase]
	if p == nil {
		return false, nil
	}
	entries, err := s.store.ListEntriesByProfile(ctx, p.ID)
	if err != nil {
		return false, err
	}
	var target *models.EntitlementEntry
	for _, e := range entries {
		if e.Channel == strings.ToLower(channel) && e.ChannelSubID == channelSubID && e.RevokedAt == nil {
			target = e
			break
		}
	}
	if target == nil {
		return false, nil // 幂等：找不到 / 已撤销
	}
	if err := s.store.MarkEntryRevoked(ctx, target.ID, now); err != nil {
		return false, err
	}
	p.DaysRemaining -= target.Days
	if p.DaysRemaining < 0 {
		p.DaysRemaining = 0
	}
	p.TrafficLimit -= target.Traffic
	if p.TrafficLimit < 0 {
		p.TrafficLimit = 0
	}
	if p.Status == models.ProfileStatusActive && p.ActiveSince != nil {
		t := p.ActiveSince.AddDate(0, 0, p.DaysRemaining+p.DaysConsumed)
		if t.Before(now) {
			t = now
		}
		p.ExpireAt = &t
	}
	if p.DaysRemaining == 0 && p.Status == models.ProfileStatusWaiting {
		p.Status = models.ProfileStatusNone
	}
	return true, s.store.UpsertProfile(ctx, p)
}

// ==================== Resolve ====================

// resolveResult 是一次裁决的结果。
type resolveResult struct {
	ActiveClass string
	Profiles    map[string]*models.EntitlementProfile
	// Changed：生效 class 变化，或生效 profile 的 expire/traffic 有变化（Sync 据此决定是否 PUT）
	Changed bool
}

// active 取当前生效 profile（可能 nil）。
func (r *resolveResult) active() *models.EntitlementProfile {
	if r == nil || r.ActiveClass == models.ActiveClassNone {
		return nil
	}
	return r.Profiles[r.ActiveClass]
}

// Resolve 生效裁决（契约 §3，服务端唯一实现，只看时间不看流量）：
//
//	订阅有效 → subscription；否则桶 days_remaining>0 → purchase（进入时 active_since=now、记流量基线）；
//	否则 trial 有效 → trial；否则 none。
//
// 回切（purchase→subscription）结算：days_remaining 取瞬间值、traffic_used += otun.used-baseline、days_consumed 累加。
func (s *EntitlementProfileService) Resolve(ctx context.Context, userID, face string) (*resolveResult, error) {
	if s == nil {
		return nil, fmt.Errorf("entitlement service not configured")
	}
	now := s.now()
	profiles, err := s.loadProfiles(ctx, userID, face)
	if err != nil {
		return nil, err
	}
	sub := profiles[models.EntitlementClassSubscription]
	pur := profiles[models.EntitlementClassPurchase]
	tri := profiles[models.EntitlementClassTrial]

	prevActive := models.ActiveClassNone
	for c, p := range profiles {
		if p.Status == models.ProfileStatusActive {
			prevActive = c
		}
	}
	// 记录裁决前生效 profile 的 expire/traffic 以判 Changed
	var prevExpire *time.Time
	var prevLimit int64
	if pp := profiles[prevActive]; pp != nil {
		prevExpire = pp.ExpireAt
		prevLimit = pp.TrafficLimit
	}

	subValid := sub != nil && sub.ExpireAt != nil && sub.ExpireAt.After(now)
	triValid := tri != nil && tri.Status != models.ProfileStatusExpired && tri.ExpireAt != nil && tri.ExpireAt.After(now)

	active := models.ActiveClassNone
	switch {
	case subValid:
		active = models.EntitlementClassSubscription
	case pur != nil && pur.DaysRemaining > 0 &&
		(pur.Status != models.ProfileStatusActive || (pur.ExpireAt != nil && pur.ExpireAt.After(now))):
		// 生效中的桶以 expire_at 为准（days_remaining 是上次 tick 的快照）：过了 expire 就不再判 purchase，
		// 首次 Resolve 即结算，避免"一拍陈旧"（验收 F1）。
		active = models.EntitlementClassPurchase
	case triValid:
		active = models.EntitlementClassTrial
	}

	dirty := map[string]bool{}

	// --- purchase 桶状态机 ---
	if pur != nil {
		switch {
		case active == models.EntitlementClassPurchase && pur.Status != models.ProfileStatusActive:
			// 进入生效：active_since=now、基线=otun 当前 used、expire=now+剩余天数
			pur.ActiveSince = &now
			pur.EffectiveFrom = &now
			pur.TrafficBaseline = s.readUsageSafe(ctx, userID, face)
			t := now.AddDate(0, 0, pur.DaysRemaining)
			pur.ExpireAt = &t
			pur.Status = models.ProfileStatusActive
			dirty[models.EntitlementClassPurchase] = true
		case active == models.EntitlementClassPurchase && pur.Status == models.ProfileStatusActive:
			// 生效中：days_remaining 持续递减（与 expire_at 同源）、流量按基线折算
			s.tickActivePurchase(ctx, userID, face, pur, now)
			dirty[models.EntitlementClassPurchase] = true
		case active != models.EntitlementClassPurchase && pur.Status == models.ProfileStatusActive:
			// 回切（新订阅到来）或桶耗尽：结算
			s.tickActivePurchase(ctx, userID, face, pur, now)
			pur.ActiveSince = nil
			if active == models.EntitlementClassSubscription {
				pur.Status = models.ProfileStatusWaiting
				pur.ExpireAt = nil
				pur.EffectiveFrom = sub.ExpireAt
			} else {
				pur.Status = models.ProfileStatusExpired // 桶耗尽；expire_at 保留末值（none 分支下发用）
				pur.DaysRemaining = 0
			}
			dirty[models.EntitlementClassPurchase] = true
		default:
			// 非生效：waiting（桶>0 且订阅有效）/ expired（曾用完）/ none
			var want string
			switch {
			case pur.DaysRemaining > 0:
				want = models.ProfileStatusWaiting
			case pur.DaysConsumed > 0 || pur.TrafficUsed > 0:
				want = models.ProfileStatusExpired
			default:
				want = models.ProfileStatusNone
			}
			var wantEff *time.Time
			if want == models.ProfileStatusWaiting && sub != nil {
				wantEff = sub.ExpireAt
			}
			if pur.Status != want || !sameTime(pur.EffectiveFrom, wantEff) {
				pur.Status = want
				pur.EffectiveFrom = wantEff
				dirty[models.EntitlementClassPurchase] = true
			}
		}
	}
	// --- subscription 状态 ---
	if sub != nil {
		want := models.ProfileStatusExpired
		if subValid {
			want = models.ProfileStatusActive
		}
		if sub.Status != want {
			sub.Status = want
			dirty[models.EntitlementClassSubscription] = true
		}
	}
	// --- trial 状态 ---
	if tri != nil {
		want := models.ProfileStatusExpired
		if active == models.EntitlementClassTrial {
			want = models.ProfileStatusActive
		}
		if tri.Status != want {
			tri.Status = want
			dirty[models.EntitlementClassTrial] = true
		}
	}

	for c := range dirty {
		if err := s.store.UpsertProfile(ctx, profiles[c]); err != nil {
			return nil, fmt.Errorf("persist profile %s: %w", c, err)
		}
	}

	res := &resolveResult{ActiveClass: active, Profiles: profiles}
	changed := active != prevActive
	if !changed {
		if ap := res.active(); ap != nil {
			changed = !sameTime(ap.ExpireAt, prevExpire) || ap.TrafficLimit != prevLimit
		}
	}
	res.Changed = changed
	return res, nil
}

// tickActivePurchase 生效中的桶：days_remaining = ceil((expire-now)/1d)、days_consumed 累加、
// traffic_used += otun.used - baseline（otun 侧重置 used → 差为负 → 以当前 used 为增量并重置基线）。
func (s *EntitlementProfileService) tickActivePurchase(ctx context.Context, userID, face string, pur *models.EntitlementProfile, now time.Time) {
	if pur.ExpireAt != nil {
		remain := int(math.Ceil(pur.ExpireAt.Sub(now).Hours() / 24))
		if remain < 0 {
			remain = 0
		}
		if remain < pur.DaysRemaining {
			pur.DaysConsumed += pur.DaysRemaining - remain
			pur.DaysRemaining = remain
		}
	}
	used, ok := s.readUsage(ctx, userID, face)
	if !ok {
		return
	}
	delta := used - pur.TrafficBaseline
	if delta < 0 {
		delta = used // otun 侧计数被重置
	}
	if delta > 0 {
		pur.TrafficUsed += delta
	}
	pur.TrafficBaseline = used
}

// ==================== Sync ====================

// Sync = Resolve + 把生效 profile 同步到该面 otun 账号 + 更新 vpn_provisions 投影行。
// 开关 false 时不动 otun/投影（影子写只到 profile/entries）。
// 仅当生效 profile 或其 expire/traffic 与投影行不一致时 PUT；同步 expire_at、
// traffic_limit（订购 = baseline + 桶剩余，基线法；不 reset used）、enabled=true。
// ★桥接推送：订阅仍有效但将在 switchLead 内到期且有 waiting 桶 → 提前把 otun expire 推到
// 订阅到期 + 桶天数（active_class 仍是 subscription，投影行/响应仍取订阅到期），确保先推后到期，
// 不给 otun-manager cleanup（1h 一轮）留下 disable 窗口。
func (s *EntitlementProfileService) Sync(ctx context.Context, userID, face string) (*resolveResult, error) {
	res, err := s.Resolve(ctx, userID, face)
	if err != nil {
		return nil, err
	}
	if !s.enabled {
		return res, nil
	}
	vp, err := s.provisions.GetCurrentByUserAndServicePartition(ctx, userID, face == models.ServiceFaceResidential)
	if err != nil || vp == nil {
		return res, nil // 该面尚无账号/投影行（首开中）：无处可同步
	}
	now := s.now()
	ap := res.active()

	var projExpire *time.Time
	var projLimit int64
	pushExpire := time.Time{}
	pushLimit := int64(0)
	doPush := false
	channel := vp.Channel

	switch res.ActiveClass {
	case models.EntitlementClassSubscription:
		projExpire, projLimit = ap.ExpireAt, ap.TrafficLimit
		pushExpire, pushLimit, doPush = derefTime(ap.ExpireAt), ap.TrafficLimit, ap.ExpireAt != nil
		if pur := res.Profiles[models.EntitlementClassPurchase]; pur != nil && pur.DaysRemaining > 0 &&
			ap.ExpireAt != nil && !ap.ExpireAt.After(now.Add(s.switchLead)) {
			pushExpire = ap.ExpireAt.AddDate(0, 0, pur.DaysRemaining) // 桥接
		}
	case models.EntitlementClassPurchase:
		projExpire, projLimit = ap.ExpireAt, ap.TrafficLimit
		remaining := ap.TrafficLimit - ap.TrafficUsed
		if remaining < 0 {
			remaining = 0
		}
		pushExpire, pushLimit, doPush = derefTime(ap.ExpireAt), ap.TrafficBaseline+remaining, ap.ExpireAt != nil
	case models.EntitlementClassTrial:
		projExpire, projLimit = ap.ExpireAt, ap.TrafficLimit
		pushExpire, pushLimit, doPush = derefTime(ap.ExpireAt), ap.TrafficLimit, ap.ExpireAt != nil
	default:
		// none：投影取最后生效 profile 的到期（不为 null）；不推 otun（到期由其自然处置）
		if last := lastProfile(res.Profiles); last != nil {
			projExpire, projLimit = last.ExpireAt, last.TrafficLimit
		}
	}
	if ap != nil {
		if ch := s.latestEntryChannel(ctx, ap); ch != "" {
			channel = ch
		}
	}

	needProjection := !sameTime(vp.ExpireAt, projExpire) || vp.TrafficLimit != projLimit || vp.Channel != channel
	bridging := doPush && projExpire != nil && !pushExpire.Equal(*projExpire)
	key := userID + "|" + face
	desired := pushState{expire: pushExpire.UTC().Truncate(time.Second), limit: pushLimit}
	s.pushMu.Lock()
	last, seen := s.lastPush[key]
	s.pushMu.Unlock()
	if doPush && (res.Changed || needProjection || bridging) && !(seen && last == desired) &&
		vp.OtunUUID != nil && *vp.OtunUUID != "" {
		if err := s.otun.Push(ctx, *vp.OtunUUID, face, vp.ServiceTier, userID, vp.Email, pushLimit, pushExpire); err != nil {
			log.Printf("[Entitlement] Sync push failed user=%s face=%s uuid=%s: %v", userID, face, *vp.OtunUUID, err)
			return res, fmt.Errorf("sync otun account: %w", err)
		}
		s.pushMu.Lock()
		if s.lastPush == nil {
			s.lastPush = map[string]pushState{}
		}
		s.lastPush[key] = desired
		s.pushMu.Unlock()
		log.Printf("[Entitlement] Sync pushed user=%s face=%s class=%s expire=%s limit=%d bridging=%v",
			userID, face, res.ActiveClass, pushExpire.Format(time.RFC3339), pushLimit, bridging)
	}
	if needProjection || res.Changed {
		if err := s.provisions.UpdateProjection(ctx, vp.ID, projExpire, projLimit, channel, res.ActiveClass); err != nil {
			log.Printf("[Entitlement] update projection failed provision=%s: %v", vp.ID, err)
		}
	}
	return res, nil
}

// ==================== Project ====================

// Project 产出契约 §2 结构（active_class + profiles[] + 生效值）。不做裁决——调用方先 Resolve/Sync。
// otunUsed：该面 otun 账号当前 traffic_used（调用方读响应时已取到；nil = 未知）。
func (s *EntitlementProfileService) Project(ctx context.Context, userID, face string, otunUsed *int64) (*models.EntitlementProjection, error) {
	if s == nil {
		return nil, fmt.Errorf("entitlement service not configured")
	}
	now := s.now()
	profiles, err := s.loadProfiles(ctx, userID, face)
	if err != nil {
		return nil, err
	}
	out := &models.EntitlementProjection{ActiveClass: models.ActiveClassNone}
	if len(profiles) == 0 {
		return out, nil
	}
	sub := profiles[models.EntitlementClassSubscription]
	for c, p := range profiles {
		if p.Status == models.ProfileStatusActive {
			out.ActiveClass = c
		}
	}
	order := []string{models.EntitlementClassSubscription, models.EntitlementClassPurchase, models.EntitlementClassTrial}
	for _, c := range order {
		p := profiles[c]
		if p == nil {
			continue
		}
		entries, err := s.store.ListEntriesByProfile(ctx, p.ID)
		if err != nil {
			return nil, err
		}
		v := models.VPNProfileView{Class: c, Status: p.Status, TrafficLimit: p.TrafficLimit, TrafficUsed: p.TrafficUsed}
		if p.ExpireAt != nil && !(c == models.EntitlementClassPurchase && p.Status == models.ProfileStatusWaiting) {
			v.ExpireAt = strPtrRFC3339(*p.ExpireAt)
		}
		if p.EffectiveFrom != nil {
			v.EffectiveFrom = strPtrRFC3339(*p.EffectiveFrom)
		}
		switch c {
		case models.EntitlementClassSubscription, models.EntitlementClassTrial:
			if p.Status == models.ProfileStatusActive && otunUsed != nil {
				v.TrafficUsed = *otunUsed // 本期已用 = 账号真源
			}
		case models.EntitlementClassPurchase:
			days := p.DaysRemaining
			used := p.TrafficUsed
			if p.Status == models.ProfileStatusActive && p.ExpireAt != nil {
				days = int(math.Ceil(p.ExpireAt.Sub(now).Hours() / 24))
				if days < 0 {
					days = 0
				}
				if otunUsed != nil {
					if d := *otunUsed - p.TrafficBaseline; d > 0 {
						used += d
					}
				}
			}
			if p.Status == models.ProfileStatusWaiting && sub != nil && sub.ExpireAt != nil {
				v.EffectiveFrom = strPtrRFC3339(*sub.ExpireAt)
			}
			consumed := p.DaysConsumed
			if p.Status == models.ProfileStatusActive && days < p.DaysRemaining {
				consumed += p.DaysRemaining - days
			}
			remaining := p.TrafficLimit - used
			if remaining < 0 {
				remaining = 0
			}
			v.TrafficUsed = used
			v.TrafficRemaining = &remaining
			v.DaysRemaining = &days
			v.DaysConsumed = &consumed
		}
		v.Kinds = distinctKinds(entries, c, now)
		out.Profiles = append(out.Profiles, v)

		if c == out.ActiveClass {
			out.ExpireAt = p.ExpireAt
			out.TrafficLimit = v.TrafficLimit
			out.TrafficUsed = v.TrafficUsed
			out.Channel = latestChannel(entries)
		}
	}
	if out.ActiveClass == models.ActiveClassNone {
		if last := lastProfile(profiles); last != nil {
			out.ExpireAt = last.ExpireAt
			out.TrafficLimit = last.TrafficLimit
			out.TrafficUsed = last.TrafficUsed
			if entries, err := s.store.ListEntriesByProfile(ctx, last.ID); err == nil {
				out.Channel = latestChannel(entries)
			}
		}
	}
	return out, nil
}

// AdminView 后台读口：两面 × profiles × entries。
func (s *EntitlementProfileService) AdminView(ctx context.Context, userID string) (*models.AdminUserVPNProfilesResponse, error) {
	resp := &models.AdminUserVPNProfilesResponse{UserID: userID, Faces: map[string][]models.AdminEntitlementProfileView{}}
	for _, face := range []string{models.ServiceFaceStandard, models.ServiceFaceResidential} {
		profiles, err := s.store.GetProfilesByUserFace(ctx, userID, face)
		if err != nil {
			return nil, err
		}
		views := make([]models.AdminEntitlementProfileView, 0, len(profiles))
		for _, p := range profiles {
			pv := models.AdminEntitlementProfileView{
				ID: p.ID, ServiceFace: p.ServiceFace, Class: p.Class, Status: p.Status, ExpireAt: p.ExpireAt,
				ActiveSince: p.ActiveSince, TrafficLimit: p.TrafficLimit, TrafficUsed: p.TrafficUsed,
				TrafficBaseline: p.TrafficBaseline, DaysRemaining: p.DaysRemaining, DaysConsumed: p.DaysConsumed,
				EffectiveFrom: p.EffectiveFrom, UpdatedAt: p.UpdatedAt, Entries: []models.AdminEntitlementEntryView{},
			}
			entries, err := s.store.ListEntriesByProfile(ctx, p.ID)
			if err != nil {
				return nil, err
			}
			for _, e := range entries {
				pv.Entries = append(pv.Entries, models.AdminEntitlementEntryView{
					ID: e.ID, SubscriptionID: e.SubscriptionID, Channel: e.Channel, ChannelSubID: e.ChannelSubID,
					Kind: e.Kind, PurchaseType: e.PurchaseType, Days: e.Days, Traffic: e.Traffic,
					PeriodStart: e.PeriodStart, PeriodEnd: e.PeriodEnd, GrantedAt: e.GrantedAt,
					RevokedAt: e.RevokedAt, SourceEventID: e.SourceEventID,
				})
			}
			views = append(views, pv)
		}
		resp.Faces[face] = views
	}
	return resp, nil
}

// ==================== helpers ====================

func (s *EntitlementProfileService) loadProfiles(ctx context.Context, userID, face string) (map[string]*models.EntitlementProfile, error) {
	list, err := s.store.GetProfilesByUserFace(ctx, userID, face)
	if err != nil {
		return nil, fmt.Errorf("load profiles: %w", err)
	}
	m := make(map[string]*models.EntitlementProfile, len(list))
	for _, p := range list {
		m[p.Class] = p
	}
	return m, nil
}

// readUsage 读该面 otun 账号 used；无账号/失败 → (0,false)。
func (s *EntitlementProfileService) readUsage(ctx context.Context, userID, face string) (int64, bool) {
	if s.provisions == nil || s.otun == nil {
		return 0, false
	}
	vp, err := s.provisions.GetCurrentByUserAndServicePartition(ctx, userID, face == models.ServiceFaceResidential)
	if err != nil || vp == nil || vp.OtunUUID == nil || *vp.OtunUUID == "" {
		return 0, false
	}
	used, err := s.otun.ReadUsage(ctx, *vp.OtunUUID, face)
	if err != nil {
		log.Printf("[Entitlement] read otun usage failed user=%s face=%s: %v", userID, face, err)
		return 0, false
	}
	return used, true
}

func (s *EntitlementProfileService) readUsageSafe(ctx context.Context, userID, face string) int64 {
	u, _ := s.readUsage(ctx, userID, face)
	return u
}

func (s *EntitlementProfileService) latestEntryChannel(ctx context.Context, p *models.EntitlementProfile) string {
	entries, err := s.store.ListEntriesByProfile(ctx, p.ID)
	if err != nil {
		return ""
	}
	return latestChannel(entries)
}

func latestChannel(entries []*models.EntitlementEntry) string {
	for i := len(entries) - 1; i >= 0; i-- {
		if entries[i].RevokedAt == nil {
			return entries[i].Channel
		}
	}
	return ""
}

// distinctKinds 当前有效条目的 kind 去重集合（订阅：本期及以后的条目；订购/试用：未撤销条目）。
func distinctKinds(entries []*models.EntitlementEntry, class string, now time.Time) []string {
	seen := map[string]bool{}
	var out []string
	for _, e := range entries {
		if e.RevokedAt != nil {
			continue
		}
		if class == models.EntitlementClassSubscription && e.PeriodEnd != nil && !e.PeriodEnd.After(now) {
			continue
		}
		if !seen[e.Kind] {
			seen[e.Kind] = true
			out = append(out, e.Kind)
		}
	}
	if len(out) == 0 && len(entries) > 0 {
		// 全部过期/撤销：仍给最后一条的 kind（none 分支也有可读的来源）
		out = append(out, entries[len(entries)-1].Kind)
	}
	sort.Strings(out)
	return out
}

// lastProfile：none 分支用——最后一个生效过的 profile（按 expire_at 最晚，其次 updated_at）。
func lastProfile(profiles map[string]*models.EntitlementProfile) *models.EntitlementProfile {
	var best *models.EntitlementProfile
	for _, p := range profiles {
		if p.ExpireAt == nil {
			continue
		}
		if best == nil || p.ExpireAt.After(*best.ExpireAt) {
			best = p
		}
	}
	return best
}

func sameTime(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	d := a.Sub(*b)
	if d < 0 {
		d = -d
	}
	return d < time.Second
}

func derefTime(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return *t
}

func strPtrRFC3339(t time.Time) *string {
	s := t.UTC().Format(time.RFC3339)
	return &s
}
