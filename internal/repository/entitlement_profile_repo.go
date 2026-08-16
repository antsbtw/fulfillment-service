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

// EntitlementProfileRepository 读写 fulfillment.vpn_entitlement_profiles / vpn_entitlement_entries。
type EntitlementProfileRepository struct {
	pool *pgxpool.Pool
}

func NewEntitlementProfileRepository(pool *pgxpool.Pool) *EntitlementProfileRepository {
	return &EntitlementProfileRepository{pool: pool}
}

const profileColumns = `id, user_id, service_face, class, status, expire_at, active_since,
	traffic_limit, traffic_used, traffic_baseline, days_remaining, days_consumed, effective_from,
	created_at, updated_at`

const entryColumns = `id, profile_id, subscription_id, channel, channel_sub_id, kind, purchase_type,
	days, traffic, period_start, period_end, granted_at, revoked_at, source_event_id, created_at`

// ==================== profiles ====================

// GetProfile 按 (user, face, class) 取一条；无则 ErrNotFound。
func (r *EntitlementProfileRepository) GetProfile(ctx context.Context, userID, face, class string) (*models.EntitlementProfile, error) {
	q := fmt.Sprintf(`SELECT %s FROM fulfillment.vpn_entitlement_profiles WHERE user_id=$1 AND service_face=$2 AND class=$3`, profileColumns)
	return scanProfile(r.pool.QueryRow(ctx, q, userID, face, class))
}

// GetProfilesByUserFace 取该 user 该面的全部 profile（≤3 条）。
func (r *EntitlementProfileRepository) GetProfilesByUserFace(ctx context.Context, userID, face string) ([]*models.EntitlementProfile, error) {
	q := fmt.Sprintf(`SELECT %s FROM fulfillment.vpn_entitlement_profiles WHERE user_id=$1 AND service_face=$2 ORDER BY class`, profileColumns)
	rows, err := r.pool.Query(ctx, q, userID, face)
	if err != nil {
		return nil, fmt.Errorf("list profiles: %w", err)
	}
	defer rows.Close()
	var out []*models.EntitlementProfile
	for rows.Next() {
		p, err := scanProfile(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// UpsertProfile 按唯一键 (user, face, class) 插入或整行更新；返回后 p.ID 已填。
func (r *EntitlementProfileRepository) UpsertProfile(ctx context.Context, p *models.EntitlementProfile) error {
	q := `
		INSERT INTO fulfillment.vpn_entitlement_profiles
			(user_id, service_face, class, status, expire_at, active_since,
			 traffic_limit, traffic_used, traffic_baseline, days_remaining, days_consumed, effective_from)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
		ON CONFLICT (user_id, service_face, class) DO UPDATE SET
			status=EXCLUDED.status, expire_at=EXCLUDED.expire_at, active_since=EXCLUDED.active_since,
			traffic_limit=EXCLUDED.traffic_limit, traffic_used=EXCLUDED.traffic_used,
			traffic_baseline=EXCLUDED.traffic_baseline, days_remaining=EXCLUDED.days_remaining,
			days_consumed=EXCLUDED.days_consumed, effective_from=EXCLUDED.effective_from,
			updated_at=NOW()
		RETURNING id, created_at, updated_at`
	err := r.pool.QueryRow(ctx, q,
		p.UserID, p.ServiceFace, p.Class, p.Status, p.ExpireAt, p.ActiveSince,
		p.TrafficLimit, p.TrafficUsed, p.TrafficBaseline, p.DaysRemaining, p.DaysConsumed, p.EffectiveFrom,
	).Scan(&p.ID, &p.CreatedAt, &p.UpdatedAt)
	if err != nil {
		return fmt.Errorf("upsert profile: %w", err)
	}
	return nil
}

// ListFacesDueForResolve 调度器扫描：生效(active) profile 在 horizon 前到期的 (user, face)，去重。
// 含"订阅即将到期且有 waiting 桶"和"订购桶即将耗尽"两类；是否真要切换由 Resolve 判定。
func (r *EntitlementProfileRepository) ListFacesDueForResolve(ctx context.Context, horizon time.Time, limit int) ([][2]string, error) {
	q := `
		SELECT DISTINCT a.user_id, a.service_face
		FROM fulfillment.vpn_entitlement_profiles a
		WHERE a.status = 'active' AND a.expire_at IS NOT NULL AND a.expire_at <= $1
		  AND EXISTS (
		      SELECT 1 FROM fulfillment.vpn_entitlement_profiles w
		      WHERE w.user_id = a.user_id AND w.service_face = a.service_face
		        AND w.class = 'purchase' AND w.days_remaining > 0 AND w.id <> a.id
		  )
		LIMIT $2`
	rows, err := r.pool.Query(ctx, q, horizon, limit)
	if err != nil {
		return nil, fmt.Errorf("list due faces: %w", err)
	}
	defer rows.Close()
	var out [][2]string
	for rows.Next() {
		var u, f string
		if err := rows.Scan(&u, &f); err != nil {
			return nil, err
		}
		out = append(out, [2]string{u, f})
	}
	return out, rows.Err()
}

// ==================== entries ====================

// GetEntryByKey 按幂等键取条目；无则 ErrNotFound。
func (r *EntitlementProfileRepository) GetEntryByKey(ctx context.Context, channel, channelSubID, sourceEventID string) (*models.EntitlementEntry, error) {
	q := fmt.Sprintf(`SELECT %s FROM fulfillment.vpn_entitlement_entries WHERE channel=$1 AND channel_sub_id=$2 AND source_event_id=$3`, entryColumns)
	return scanEntry(r.pool.QueryRow(ctx, q, channel, channelSubID, sourceEventID))
}

// CreateEntry 插入条目；幂等键冲突时返回 (false, nil)（已存在，不重复入账）。
func (r *EntitlementProfileRepository) CreateEntry(ctx context.Context, e *models.EntitlementEntry) (bool, error) {
	q := `
		INSERT INTO fulfillment.vpn_entitlement_entries
			(profile_id, subscription_id, channel, channel_sub_id, kind, purchase_type,
			 days, traffic, period_start, period_end, granted_at, revoked_at, source_event_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)
		ON CONFLICT (channel, channel_sub_id, source_event_id) DO NOTHING
		RETURNING id, created_at`
	err := r.pool.QueryRow(ctx, q,
		e.ProfileID, e.SubscriptionID, e.Channel, e.ChannelSubID, e.Kind, e.PurchaseType,
		e.Days, e.Traffic, e.PeriodStart, e.PeriodEnd, e.GrantedAt, e.RevokedAt, e.SourceEventID,
	).Scan(&e.ID, &e.CreatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("insert entry: %w", err)
	}
	return true, nil
}

// ListEntriesByProfile 取 profile 的全部条目（含已撤销），按 granted_at 升序。
func (r *EntitlementProfileRepository) ListEntriesByProfile(ctx context.Context, profileID string) ([]*models.EntitlementEntry, error) {
	q := fmt.Sprintf(`SELECT %s FROM fulfillment.vpn_entitlement_entries WHERE profile_id=$1 ORDER BY granted_at ASC, created_at ASC`, entryColumns)
	rows, err := r.pool.Query(ctx, q, profileID)
	if err != nil {
		return nil, fmt.Errorf("list entries: %w", err)
	}
	defer rows.Close()
	var out []*models.EntitlementEntry
	for rows.Next() {
		e, err := scanEntry(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// MarkEntryRevoked 标记条目撤销时间（幂等：已撤销不动）。
func (r *EntitlementProfileRepository) MarkEntryRevoked(ctx context.Context, id string, at time.Time) error {
	_, err := r.pool.Exec(ctx, `UPDATE fulfillment.vpn_entitlement_entries SET revoked_at=$2 WHERE id=$1 AND revoked_at IS NULL`, id, at)
	if err != nil {
		return fmt.Errorf("mark entry revoked: %w", err)
	}
	return nil
}

// ==================== scan ====================

func scanProfile(row pgx.Row) (*models.EntitlementProfile, error) {
	p := &models.EntitlementProfile{}
	err := row.Scan(
		&p.ID, &p.UserID, &p.ServiceFace, &p.Class, &p.Status, &p.ExpireAt, &p.ActiveSince,
		&p.TrafficLimit, &p.TrafficUsed, &p.TrafficBaseline, &p.DaysRemaining, &p.DaysConsumed, &p.EffectiveFrom,
		&p.CreatedAt, &p.UpdatedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("scan profile: %w", err)
	}
	return p, nil
}

func scanEntry(row pgx.Row) (*models.EntitlementEntry, error) {
	e := &models.EntitlementEntry{}
	err := row.Scan(
		&e.ID, &e.ProfileID, &e.SubscriptionID, &e.Channel, &e.ChannelSubID, &e.Kind, &e.PurchaseType,
		&e.Days, &e.Traffic, &e.PeriodStart, &e.PeriodEnd, &e.GrantedAt, &e.RevokedAt, &e.SourceEventID, &e.CreatedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("scan entry: %w", err)
	}
	return e, nil
}
