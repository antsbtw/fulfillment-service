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

type VPNProvisionRepository struct {
	pool *pgxpool.Pool
}

func NewVPNProvisionRepository(pool *pgxpool.Pool) *VPNProvisionRepository {
	return &VPNProvisionRepository{pool: pool}
}

const vpnColumns = `id, user_id, subscription_id, channel,
	business_type, service_tier, otun_uuid, plan_tier, status,
	traffic_limit, traffic_used, expire_at,
	email, device_id, granted_by, note, is_current,
	created_at, updated_at`

func (r *VPNProvisionRepository) Create(ctx context.Context, vp *models.VPNProvision) error {
	query := `
		INSERT INTO fulfillment.vpn_provisions (
			id, user_id, subscription_id, channel,
			business_type, service_tier, otun_uuid, plan_tier, status,
			traffic_limit, traffic_used, expire_at,
			email, device_id, granted_by, note, is_current
		) VALUES (
			$1, $2, $3, $4,
			$5, $6, $7, $8, $9,
			$10, $11, $12,
			$13, $14, $15, $16, $17
		)
	`
	_, err := r.pool.Exec(ctx, query,
		vp.ID, vp.UserID, vp.SubscriptionID, vp.Channel,
		vp.BusinessType, vp.ServiceTier, vp.OtunUUID, vp.PlanTier, vp.Status,
		vp.TrafficLimit, vp.TrafficUsed, vp.ExpireAt,
		vp.Email, vp.DeviceID, vp.GrantedBy, vp.Note, vp.IsCurrent,
	)
	if err != nil {
		return fmt.Errorf("insert vpn_provision: %w", err)
	}
	return nil
}

func (r *VPNProvisionRepository) GetByID(ctx context.Context, id string) (*models.VPNProvision, error) {
	query := fmt.Sprintf(`SELECT %s FROM fulfillment.vpn_provisions WHERE id = $1`, vpnColumns)
	return r.scanOne(r.pool.QueryRow(ctx, query, id))
}

func (r *VPNProvisionRepository) GetCurrentByUser(ctx context.Context, userID string) (*models.VPNProvision, error) {
	query := fmt.Sprintf(`
		SELECT %s FROM fulfillment.vpn_provisions
		WHERE user_id = $1 AND is_current = TRUE AND status = 'active'
		ORDER BY created_at DESC
		LIMIT 1
	`, vpnColumns)
	return r.scanOne(r.pool.QueryRow(ctx, query, userID))
}

func (r *VPNProvisionRepository) GetCurrentByUserAnyStatus(ctx context.Context, userID string) (*models.VPNProvision, error) {
	query := fmt.Sprintf(`
		SELECT %s FROM fulfillment.vpn_provisions
		WHERE user_id = $1 AND is_current = TRUE
		ORDER BY created_at DESC
		LIMIT 1
	`, vpnColumns)
	return r.scanOne(r.pool.QueryRow(ctx, query, userID))
}

func (r *VPNProvisionRepository) GetBySubscriptionID(ctx context.Context, subscriptionID string) (*models.VPNProvision, error) {
	query := fmt.Sprintf(`
		SELECT %s FROM fulfillment.vpn_provisions
		WHERE subscription_id = $1 AND is_current = TRUE
		ORDER BY created_at DESC
		LIMIT 1
	`, vpnColumns)
	return r.scanOne(r.pool.QueryRow(ctx, query, subscriptionID))
}

func (r *VPNProvisionRepository) GetByUserAndBusinessType(ctx context.Context, userID, businessType string) (*models.VPNProvision, error) {
	query := fmt.Sprintf(`
		SELECT %s FROM fulfillment.vpn_provisions
		WHERE user_id = $1 AND business_type = $2
		ORDER BY created_at DESC
		LIMIT 1
	`, vpnColumns)
	return r.scanOne(r.pool.QueryRow(ctx, query, userID, businessType))
}

func (r *VPNProvisionRepository) GetOtunUUIDByUser(ctx context.Context, userID string) (*string, error) {
	query := `
		SELECT otun_uuid FROM fulfillment.vpn_provisions
		WHERE user_id = $1 AND otun_uuid IS NOT NULL AND otun_uuid != ''
		ORDER BY created_at DESC
		LIMIT 1
	`
	var otunUUID *string
	err := r.pool.QueryRow(ctx, query, userID).Scan(&otunUUID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("get otun_uuid: %w", err)
	}
	return otunUUID, nil
}

// Update operations

func (r *VPNProvisionRepository) Update(ctx context.Context, vp *models.VPNProvision) error {
	query := `
		UPDATE fulfillment.vpn_provisions SET
			subscription_id = $1, channel = $2,
			business_type = $3, service_tier = $4,
			otun_uuid = $5, plan_tier = $6, status = $7,
			traffic_limit = $8, traffic_used = $9, expire_at = $10,
			email = $11, device_id = $12, granted_by = $13, note = $14,
			is_current = $15, updated_at = NOW()
		WHERE id = $16
	`
	_, err := r.pool.Exec(ctx, query,
		vp.SubscriptionID, vp.Channel,
		vp.BusinessType, vp.ServiceTier,
		vp.OtunUUID, vp.PlanTier, vp.Status,
		vp.TrafficLimit, vp.TrafficUsed, vp.ExpireAt,
		vp.Email, vp.DeviceID, vp.GrantedBy, vp.Note,
		vp.IsCurrent, vp.ID,
	)
	if err != nil {
		return fmt.Errorf("update vpn_provision: %w", err)
	}
	return nil
}

func (r *VPNProvisionRepository) UpdateStatus(ctx context.Context, id, status string) error {
	query := `UPDATE fulfillment.vpn_provisions SET status = $1, updated_at = NOW() WHERE id = $2`
	_, err := r.pool.Exec(ctx, query, status, id)
	if err != nil {
		return fmt.Errorf("update vpn_provision status: %w", err)
	}
	return nil
}

func (r *VPNProvisionRepository) MarkNotCurrent(ctx context.Context, id string) error {
	query := `UPDATE fulfillment.vpn_provisions SET is_current = FALSE, status = 'converted', updated_at = NOW() WHERE id = $1`
	_, err := r.pool.Exec(ctx, query, id)
	if err != nil {
		return fmt.Errorf("mark vpn_provision not current: %w", err)
	}
	return nil
}

func (r *VPNProvisionRepository) UpdateTrafficUsed(ctx context.Context, id string, trafficUsed int64) error {
	query := `UPDATE fulfillment.vpn_provisions SET traffic_used = $1, updated_at = NOW() WHERE id = $2`
	_, err := r.pool.Exec(ctx, query, trafficUsed, id)
	if err != nil {
		return fmt.Errorf("update traffic_used: %w", err)
	}
	return nil
}

// ListByFilters queries vpn_provisions with optional filters
func (r *VPNProvisionRepository) ListByFilters(ctx context.Context, userID, businessType, status string) ([]*models.VPNProvision, error) {
	query := fmt.Sprintf(`
		SELECT %s FROM fulfillment.vpn_provisions
		WHERE ($1 = '' OR user_id::text = $1)
		  AND ($2 = '' OR business_type = $2)
		  AND ($3 = '' OR status = $3)
		ORDER BY created_at DESC
		LIMIT 100
	`, vpnColumns)
	rows, err := r.pool.Query(ctx, query, userID, businessType, status)
	if err != nil {
		return nil, fmt.Errorf("list vpn_provisions: %w", err)
	}
	defer rows.Close()
	return r.scanMany(rows)
}

// IsExpired checks if a vpn provision has expired by time or traffic
func IsVPNExpired(vp *models.VPNProvision) bool {
	if vp.ExpireAt != nil && time.Now().After(*vp.ExpireAt) {
		return true
	}
	if vp.TrafficLimit > 0 && vp.TrafficUsed >= vp.TrafficLimit {
		return true
	}
	return false
}

func (r *VPNProvisionRepository) scanOne(row pgx.Row) (*models.VPNProvision, error) {
	vp := &models.VPNProvision{}
	err := row.Scan(
		&vp.ID, &vp.UserID, &vp.SubscriptionID, &vp.Channel,
		&vp.BusinessType, &vp.ServiceTier, &vp.OtunUUID, &vp.PlanTier, &vp.Status,
		&vp.TrafficLimit, &vp.TrafficUsed, &vp.ExpireAt,
		&vp.Email, &vp.DeviceID, &vp.GrantedBy, &vp.Note, &vp.IsCurrent,
		&vp.CreatedAt, &vp.UpdatedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("scan vpn_provision: %w", err)
	}
	return vp, nil
}

func (r *VPNProvisionRepository) scanMany(rows pgx.Rows) ([]*models.VPNProvision, error) {
	var results []*models.VPNProvision
	for rows.Next() {
		vp := &models.VPNProvision{}
		err := rows.Scan(
			&vp.ID, &vp.UserID, &vp.SubscriptionID, &vp.Channel,
			&vp.BusinessType, &vp.ServiceTier, &vp.OtunUUID, &vp.PlanTier, &vp.Status,
			&vp.TrafficLimit, &vp.TrafficUsed, &vp.ExpireAt,
			&vp.Email, &vp.DeviceID, &vp.GrantedBy, &vp.Note, &vp.IsCurrent,
			&vp.CreatedAt, &vp.UpdatedAt,
		)
		if err != nil {
			return nil, fmt.Errorf("scan vpn_provision row: %w", err)
		}
		results = append(results, vp)
	}
	return results, rows.Err()
}
