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

type EntitlementRepository struct {
	pool *pgxpool.Pool
}

func NewEntitlementRepository(pool *pgxpool.Pool) *EntitlementRepository {
	return &EntitlementRepository{pool: pool}
}

// Create inserts a new entitlement record
func (r *EntitlementRepository) Create(ctx context.Context, e *models.Entitlement) error {
	query := `
		INSERT INTO fulfillment.entitlements (
			id, user_id, email, otun_uuid, source, status,
			traffic_limit, traffic_used, expire_at, service_tier,
			granted_by, note, device_id
		) VALUES (
			$1, $2, $3, $4, $5, $6,
			$7, $8, $9, $10,
			$11, $12, $13
		)
	`
	_, err := r.pool.Exec(ctx, query,
		e.ID, e.UserID, e.Email, e.OtunUUID, e.Source, e.Status,
		e.TrafficLimit, e.TrafficUsed, e.ExpireAt, e.ServiceTier,
		e.GrantedBy, e.Note, e.DeviceID,
	)
	if err != nil {
		return fmt.Errorf("insert entitlement: %w", err)
	}
	return nil
}

// GetByID retrieves an entitlement by ID
func (r *EntitlementRepository) GetByID(ctx context.Context, id string) (*models.Entitlement, error) {
	query := `
		SELECT id, user_id, email, otun_uuid, source, status,
		       traffic_limit, traffic_used, expire_at, service_tier,
		       granted_by, note, device_id, created_at, updated_at
		FROM fulfillment.entitlements
		WHERE id = $1
	`
	return r.scanEntitlement(r.pool.QueryRow(ctx, query, id))
}

// GetByUserIDAndSource retrieves an entitlement by user_id and source
func (r *EntitlementRepository) GetByUserIDAndSource(ctx context.Context, userID, source string) (*models.Entitlement, error) {
	query := `
		SELECT id, user_id, email, otun_uuid, source, status,
		       traffic_limit, traffic_used, expire_at, service_tier,
		       granted_by, note, device_id, created_at, updated_at
		FROM fulfillment.entitlements
		WHERE user_id = $1 AND source = $2
		ORDER BY created_at DESC
		LIMIT 1
	`
	return r.scanEntitlement(r.pool.QueryRow(ctx, query, userID, source))
}

// GetActiveByUserIDAndSource retrieves an active entitlement by user_id and source
func (r *EntitlementRepository) GetActiveByUserIDAndSource(ctx context.Context, userID, source string) (*models.Entitlement, error) {
	query := `
		SELECT id, user_id, email, otun_uuid, source, status,
		       traffic_limit, traffic_used, expire_at, service_tier,
		       granted_by, note, device_id, created_at, updated_at
		FROM fulfillment.entitlements
		WHERE user_id = $1 AND source = $2 AND status = 'active'
		ORDER BY created_at DESC
		LIMIT 1
	`
	return r.scanEntitlement(r.pool.QueryRow(ctx, query, userID, source))
}

// GetOtunUUIDByUserID finds any existing otun_uuid for a user across all entitlements
func (r *EntitlementRepository) GetOtunUUIDByUserID(ctx context.Context, userID string) (*string, error) {
	query := `
		SELECT otun_uuid
		FROM fulfillment.entitlements
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

// ExistsTrialByDeviceID checks if a trial entitlement already exists for the given device_id
func (r *EntitlementRepository) ExistsTrialByDeviceID(ctx context.Context, deviceID string) (bool, error) {
	query := `
		SELECT EXISTS(
			SELECT 1 FROM fulfillment.entitlements
			WHERE device_id = $1 AND source = 'trial'
		)
	`
	var exists bool
	err := r.pool.QueryRow(ctx, query, deviceID).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("check trial by device_id: %w", err)
	}
	return exists, nil
}

// ExistsTrialByEmail checks if a trial entitlement already exists for the given email
func (r *EntitlementRepository) ExistsTrialByEmail(ctx context.Context, email string) (bool, error) {
	query := `
		SELECT EXISTS(
			SELECT 1 FROM fulfillment.entitlements
			WHERE email = $1 AND source = 'trial'
		)
	`
	var exists bool
	err := r.pool.QueryRow(ctx, query, email).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("check trial by email: %w", err)
	}
	return exists, nil
}

// UpdateStatus updates the status of an entitlement
func (r *EntitlementRepository) UpdateStatus(ctx context.Context, id, status string) error {
	query := `UPDATE fulfillment.entitlements SET status = $1 WHERE id = $2`
	_, err := r.pool.Exec(ctx, query, status, id)
	if err != nil {
		return fmt.Errorf("update entitlement status: %w", err)
	}
	return nil
}

// UpdateOtunUUID sets the otun_uuid after VPN user provisioning
func (r *EntitlementRepository) UpdateOtunUUID(ctx context.Context, id, otunUUID string) error {
	query := `UPDATE fulfillment.entitlements SET otun_uuid = $1 WHERE id = $2`
	_, err := r.pool.Exec(ctx, query, otunUUID, id)
	if err != nil {
		return fmt.Errorf("update otun_uuid: %w", err)
	}
	return nil
}

// UpdateTrafficUsed syncs traffic_used from otun-manager
func (r *EntitlementRepository) UpdateTrafficUsed(ctx context.Context, id string, trafficUsed int64) error {
	query := `UPDATE fulfillment.entitlements SET traffic_used = $1 WHERE id = $2`
	_, err := r.pool.Exec(ctx, query, trafficUsed, id)
	if err != nil {
		return fmt.Errorf("update traffic_used: %w", err)
	}
	return nil
}

// ListByFilters queries entitlements with optional filters
func (r *EntitlementRepository) ListByFilters(ctx context.Context, userID, source, status string) ([]*models.Entitlement, error) {
	query := `
		SELECT id, user_id, email, otun_uuid, source, status,
		       traffic_limit, traffic_used, expire_at, service_tier,
		       granted_by, note, device_id, created_at, updated_at
		FROM fulfillment.entitlements
		WHERE ($1 = '' OR user_id::text = $1)
		  AND ($2 = '' OR source = $2)
		  AND ($3 = '' OR status = $3)
		ORDER BY created_at DESC
		LIMIT 100
	`
	rows, err := r.pool.Query(ctx, query, userID, source, status)
	if err != nil {
		return nil, fmt.Errorf("list entitlements: %w", err)
	}
	defer rows.Close()

	return r.scanEntitlements(rows)
}

func (r *EntitlementRepository) scanEntitlement(row pgx.Row) (*models.Entitlement, error) {
	e := &models.Entitlement{}
	err := row.Scan(
		&e.ID, &e.UserID, &e.Email, &e.OtunUUID, &e.Source, &e.Status,
		&e.TrafficLimit, &e.TrafficUsed, &e.ExpireAt, &e.ServiceTier,
		&e.GrantedBy, &e.Note, &e.DeviceID, &e.CreatedAt, &e.UpdatedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("scan entitlement: %w", err)
	}
	return e, nil
}

func (r *EntitlementRepository) scanEntitlements(rows pgx.Rows) ([]*models.Entitlement, error) {
	var entitlements []*models.Entitlement
	for rows.Next() {
		e := &models.Entitlement{}
		err := rows.Scan(
			&e.ID, &e.UserID, &e.Email, &e.OtunUUID, &e.Source, &e.Status,
			&e.TrafficLimit, &e.TrafficUsed, &e.ExpireAt, &e.ServiceTier,
			&e.GrantedBy, &e.Note, &e.DeviceID, &e.CreatedAt, &e.UpdatedAt,
		)
		if err != nil {
			return nil, fmt.Errorf("scan entitlement row: %w", err)
		}
		entitlements = append(entitlements, e)
	}
	return entitlements, rows.Err()
}

// IsExpired checks if an entitlement has expired by time or traffic
func IsExpired(e *models.Entitlement) bool {
	if e.ExpireAt != nil && time.Now().After(*e.ExpireAt) {
		return true
	}
	if e.TrafficLimit > 0 && e.TrafficUsed >= e.TrafficLimit {
		return true
	}
	return false
}
