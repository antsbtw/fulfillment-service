package repository

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/wenwu/saas-platform/fulfillment-service/internal/models"
)

// RefundRevocationRepository 服务类退款停服扣减记录(fulfillment.vpn_refund_revocations,按 ref 幂等)。
type RefundRevocationRepository struct {
	pool *pgxpool.Pool
}

func NewRefundRevocationRepository(pool *pgxpool.Pool) *RefundRevocationRepository {
	return &RefundRevocationRepository{pool: pool}
}

// Get 按 ref 取记录;无 → ErrNotFound。
func (r *RefundRevocationRepository) Get(ctx context.Context, ref string) (*models.RefundRevocation, error) {
	var v models.RefundRevocation
	err := r.pool.QueryRow(ctx, `
		SELECT ref, user_id, plan_tier, COALESCE(provision_id, ''), days, traffic_bytes, old_expire_at, new_expire_at,
		       new_traffic_limit, disabled, noop, COALESCE(reason, ''), created_at
		FROM fulfillment.vpn_refund_revocations WHERE ref = $1`, ref).
		Scan(&v.Ref, &v.UserID, &v.PlanTier, &v.ProvisionID, &v.Days, &v.TrafficBytes, &v.OldExpireAt, &v.NewExpireAt,
			&v.NewTrafficLimit, &v.Disabled, &v.Noop, &v.Reason, &v.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get refund revocation: %w", err)
	}
	return &v, nil
}

// InsertIfAbsent 记录一次扣减;同 ref 已存在返回 false。
func (r *RefundRevocationRepository) InsertIfAbsent(ctx context.Context, v *models.RefundRevocation) (bool, error) {
	tag, err := r.pool.Exec(ctx, `
		INSERT INTO fulfillment.vpn_refund_revocations
			(ref, user_id, plan_tier, provision_id, days, traffic_bytes, old_expire_at, new_expire_at, new_traffic_limit, disabled, noop, reason)
		VALUES ($1, $2, $3, NULLIF($4, ''), $5, $6, $7, $8, $9, $10, $11, $12)
		ON CONFLICT (ref) DO NOTHING`,
		v.Ref, v.UserID, v.PlanTier, v.ProvisionID, v.Days, v.TrafficBytes, v.OldExpireAt, v.NewExpireAt,
		v.NewTrafficLimit, v.Disabled, v.Noop, v.Reason)
	if err != nil {
		return false, fmt.Errorf("insert refund revocation: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}
