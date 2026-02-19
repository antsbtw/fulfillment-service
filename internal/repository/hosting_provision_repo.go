package repository

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/wenwu/saas-platform/fulfillment-service/internal/models"
)

var ErrNotFound = errors.New("not found")

type HostingProvisionRepository struct {
	pool *pgxpool.Pool
}

func NewHostingProvisionRepository(pool *pgxpool.Pool) *HostingProvisionRepository {
	return &HostingProvisionRepository{pool: pool}
}

func (r *HostingProvisionRepository) Create(ctx context.Context, hp *models.HostingProvision) error {
	query := `
		INSERT INTO fulfillment.hosting_provisions (
			id, subscription_id, user_id, channel,
			hosting_node_id, provider, region,
			public_ip, api_port, api_key, vless_port, ss_port, public_key, short_id,
			status, error_message, plan_tier, traffic_limit, traffic_used
		) VALUES (
			$1, $2, $3, $4,
			$5, $6, $7,
			$8, $9, $10, $11, $12, $13, $14,
			$15, $16, $17, $18, $19
		)
	`
	_, err := r.pool.Exec(ctx, query,
		hp.ID, hp.SubscriptionID, hp.UserID, hp.Channel,
		hp.HostingNodeID, hp.Provider, hp.Region,
		hp.PublicIP, hp.APIPort, hp.APIKey, hp.VlessPort, hp.SSPort, hp.PublicKey, hp.ShortID,
		hp.Status, hp.ErrorMessage, hp.PlanTier, hp.TrafficLimit, hp.TrafficUsed,
	)
	if err != nil {
		return fmt.Errorf("insert hosting_provision: %w", err)
	}
	return nil
}

func (r *HostingProvisionRepository) GetByID(ctx context.Context, id string) (*models.HostingProvision, error) {
	query := `
		SELECT id, subscription_id, user_id, channel,
			   hosting_node_id, provider, region,
			   public_ip, api_port, api_key, vless_port, ss_port, public_key, short_id,
			   status, error_message, plan_tier, traffic_limit, traffic_used,
			   created_at, updated_at, ready_at, deleted_at
		FROM fulfillment.hosting_provisions
		WHERE id = $1
	`
	return r.scanOne(r.pool.QueryRow(ctx, query, id))
}

func (r *HostingProvisionRepository) GetBySubscriptionID(ctx context.Context, subscriptionID string) ([]*models.HostingProvision, error) {
	query := `
		SELECT id, subscription_id, user_id, channel,
			   hosting_node_id, provider, region,
			   public_ip, api_port, api_key, vless_port, ss_port, public_key, short_id,
			   status, error_message, plan_tier, traffic_limit, traffic_used,
			   created_at, updated_at, ready_at, deleted_at
		FROM fulfillment.hosting_provisions
		WHERE subscription_id = $1 AND deleted_at IS NULL
		ORDER BY created_at DESC
	`
	rows, err := r.pool.Query(ctx, query, subscriptionID)
	if err != nil {
		return nil, fmt.Errorf("query hosting_provisions: %w", err)
	}
	defer rows.Close()
	return r.scanMany(rows)
}

func (r *HostingProvisionRepository) GetActiveByUser(ctx context.Context, userID string) (*models.HostingProvision, error) {
	query := `
		SELECT id, subscription_id, user_id, channel,
			   hosting_node_id, provider, region,
			   public_ip, api_port, api_key, vless_port, ss_port, public_key, short_id,
			   status, error_message, plan_tier, traffic_limit, traffic_used,
			   created_at, updated_at, ready_at, deleted_at
		FROM fulfillment.hosting_provisions
		WHERE user_id = $1
		  AND status NOT IN ('deleted', 'failed')
		  AND deleted_at IS NULL
		ORDER BY created_at DESC
		LIMIT 1
	`
	return r.scanOne(r.pool.QueryRow(ctx, query, userID))
}

func (r *HostingProvisionRepository) GetLatestByUser(ctx context.Context, userID string) (*models.HostingProvision, error) {
	query := `
		SELECT id, subscription_id, user_id, channel,
			   hosting_node_id, provider, region,
			   public_ip, api_port, api_key, vless_port, ss_port, public_key, short_id,
			   status, error_message, plan_tier, traffic_limit, traffic_used,
			   created_at, updated_at, ready_at, deleted_at
		FROM fulfillment.hosting_provisions
		WHERE user_id = $1
		  AND status != 'deleted'
		  AND deleted_at IS NULL
		ORDER BY created_at DESC
		LIMIT 1
	`
	return r.scanOne(r.pool.QueryRow(ctx, query, userID))
}

func (r *HostingProvisionRepository) Update(ctx context.Context, hp *models.HostingProvision) error {
	query := `
		UPDATE fulfillment.hosting_provisions SET
			hosting_node_id = $1,
			public_ip = $2,
			api_port = $3,
			api_key = $4,
			vless_port = $5,
			ss_port = $6,
			public_key = $7,
			short_id = $8,
			status = $9,
			error_message = $10,
			traffic_used = $11,
			ready_at = $12,
			deleted_at = $13,
			updated_at = NOW()
		WHERE id = $14
	`
	_, err := r.pool.Exec(ctx, query,
		hp.HostingNodeID,
		hp.PublicIP, hp.APIPort, hp.APIKey,
		hp.VlessPort, hp.SSPort, hp.PublicKey, hp.ShortID,
		hp.Status, hp.ErrorMessage, hp.TrafficUsed,
		hp.ReadyAt, hp.DeletedAt, hp.ID,
	)
	if err != nil {
		return fmt.Errorf("update hosting_provision: %w", err)
	}
	return nil
}

func (r *HostingProvisionRepository) UpdateStatus(ctx context.Context, id, status string, errorMsg *string) error {
	query := `UPDATE fulfillment.hosting_provisions SET status = $1, error_message = $2, updated_at = NOW() WHERE id = $3`
	_, err := r.pool.Exec(ctx, query, status, errorMsg, id)
	if err != nil {
		return fmt.Errorf("update hosting_provision status: %w", err)
	}
	return nil
}

func (r *HostingProvisionRepository) scanOne(row pgx.Row) (*models.HostingProvision, error) {
	hp := &models.HostingProvision{}
	err := row.Scan(
		&hp.ID, &hp.SubscriptionID, &hp.UserID, &hp.Channel,
		&hp.HostingNodeID, &hp.Provider, &hp.Region,
		&hp.PublicIP, &hp.APIPort, &hp.APIKey, &hp.VlessPort, &hp.SSPort, &hp.PublicKey, &hp.ShortID,
		&hp.Status, &hp.ErrorMessage, &hp.PlanTier, &hp.TrafficLimit, &hp.TrafficUsed,
		&hp.CreatedAt, &hp.UpdatedAt, &hp.ReadyAt, &hp.DeletedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("scan hosting_provision: %w", err)
	}
	return hp, nil
}

func (r *HostingProvisionRepository) scanMany(rows pgx.Rows) ([]*models.HostingProvision, error) {
	var results []*models.HostingProvision
	for rows.Next() {
		hp := &models.HostingProvision{}
		err := rows.Scan(
			&hp.ID, &hp.SubscriptionID, &hp.UserID, &hp.Channel,
			&hp.HostingNodeID, &hp.Provider, &hp.Region,
			&hp.PublicIP, &hp.APIPort, &hp.APIKey, &hp.VlessPort, &hp.SSPort, &hp.PublicKey, &hp.ShortID,
			&hp.Status, &hp.ErrorMessage, &hp.PlanTier, &hp.TrafficLimit, &hp.TrafficUsed,
			&hp.CreatedAt, &hp.UpdatedAt, &hp.ReadyAt, &hp.DeletedAt,
		)
		if err != nil {
			return nil, fmt.Errorf("scan hosting_provision row: %w", err)
		}
		results = append(results, hp)
	}
	return results, rows.Err()
}
