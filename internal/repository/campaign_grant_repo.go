package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/wenwu/saas-platform/fulfillment-service/internal/models"
)

// CampaignGrantRepository 活动账号入账台账（迁移 010 fulfillment.campaign_grants）。
// 每次领取（一条 subscription_id）一条 grant；撤销标 revoked；到期后再领开新周期时旧 grant 标 expired。
// 作用：(1) 开通幂等（同一 subscription_id 重放不重复叠加）；(2) 撤销幂等（同一 grant 只扣一次）；
// (3) /vpn/all campaign 元素的 campaign{} 子对象聚合（claims_active / granted_* / last_claim_at）。
type CampaignGrantRepository struct {
	pool *pgxpool.Pool
}

func NewCampaignGrantRepository(pool *pgxpool.Pool) *CampaignGrantRepository {
	return &CampaignGrantRepository{pool: pool}
}

// InsertIfAbsent 幂等入账：已存在（同 subscription_id）返回 inserted=false。
func (r *CampaignGrantRepository) InsertIfAbsent(ctx context.Context, g *models.CampaignGrant) (bool, error) {
	tag, err := r.pool.Exec(ctx, `
		INSERT INTO fulfillment.campaign_grants (subscription_id, user_id, channel_sub_id, days, traffic_bytes, status, applied_at)
		VALUES ($1, $2, $3, $4, $5, 'active', NOW())
		ON CONFLICT (subscription_id) DO NOTHING
	`, g.SubscriptionID, g.UserID, g.ChannelSubID, g.Days, g.TrafficBytes)
	if err != nil {
		return false, fmt.Errorf("insert campaign_grant: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// GetBySubscriptionID 取一条 grant；无 → ErrNotFound。
func (r *CampaignGrantRepository) GetBySubscriptionID(ctx context.Context, subscriptionID string) (*models.CampaignGrant, error) {
	g := &models.CampaignGrant{}
	err := r.pool.QueryRow(ctx, `
		SELECT subscription_id, user_id, COALESCE(channel_sub_id,''), days, traffic_bytes, status, applied_at, revoked_at
		FROM fulfillment.campaign_grants WHERE subscription_id = $1
	`, subscriptionID).Scan(&g.SubscriptionID, &g.UserID, &g.ChannelSubID, &g.Days, &g.TrafficBytes, &g.Status, &g.AppliedAt, &g.RevokedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("get campaign_grant: %w", err)
	}
	return g, nil
}

// MarkRevoked 只对 status=active 的 grant 生效；返回是否真的翻转（幂等：已 revoked → false）。
func (r *CampaignGrantRepository) MarkRevoked(ctx context.Context, subscriptionID string) (bool, error) {
	tag, err := r.pool.Exec(ctx, `
		UPDATE fulfillment.campaign_grants
		SET status = 'revoked', revoked_at = NOW(), updated_at = NOW()
		WHERE subscription_id = $1 AND status = 'active'
	`, subscriptionID)
	if err != nil {
		return false, fmt.Errorf("revoke campaign_grant: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// ExpireActiveByUser 开新周期（到期后再领）：把该 user 仍 active 的旧 grant 标 expired。
func (r *CampaignGrantRepository) ExpireActiveByUser(ctx context.Context, userID string) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE fulfillment.campaign_grants
		SET status = 'expired', updated_at = NOW()
		WHERE user_id = $1 AND status = 'active'
	`, userID)
	if err != nil {
		return fmt.Errorf("expire campaign_grants: %w", err)
	}
	return nil
}

// AggregateActiveByUser 聚合当前周期计入的 grant（status=active）。
func (r *CampaignGrantRepository) AggregateActiveByUser(ctx context.Context, userID string) (*models.CampaignGrantAggregate, error) {
	agg := &models.CampaignGrantAggregate{}
	var last *time.Time
	err := r.pool.QueryRow(ctx, `
		SELECT COUNT(*), COALESCE(SUM(days),0), COALESCE(SUM(traffic_bytes),0), MAX(applied_at)
		FROM fulfillment.campaign_grants WHERE user_id = $1 AND status = 'active'
	`, userID).Scan(&agg.ClaimsActive, &agg.GrantedDaysTotal, &agg.GrantedTrafficTotal, &last)
	if err != nil {
		return nil, fmt.Errorf("aggregate campaign_grants: %w", err)
	}
	agg.LastClaimAt = last
	return agg, nil
}

// ListExpiredCampaignRows 取到期超过 retention 仍 is_current 的 campaign 面行（cleanup 调度用）。
func (r *VPNProvisionRepository) ListExpiredCampaignRows(ctx context.Context, before time.Time, limit int) ([]*models.VPNProvision, error) {
	query := fmt.Sprintf(`
		SELECT %s FROM fulfillment.vpn_provisions
		WHERE product_face = 'campaign' AND is_current = TRUE
		  AND expire_at IS NOT NULL AND expire_at < $1
		ORDER BY expire_at ASC
		LIMIT $2
	`, vpnColumns)
	rows, err := r.pool.Query(ctx, query, before, limit)
	if err != nil {
		return nil, fmt.Errorf("list expired campaign rows: %w", err)
	}
	defer rows.Close()
	return r.scanMany(rows)
}
