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

type ResourceRepository struct {
	pool *pgxpool.Pool
}

func NewResourceRepository(pool *pgxpool.Pool) *ResourceRepository {
	return &ResourceRepository{pool: pool}
}

// Create creates a new resource
func (r *ResourceRepository) Create(ctx context.Context, res *models.Resource) error {
	query := `
		INSERT INTO fulfillment.resources (
			id, subscription_id, user_id, resource_type, provider, region,
			instance_id, public_ip, private_ip, api_port, api_key, vless_port,
			ss_port, public_key, short_id, ssh_private_key, ssh_key_name,
			status, error_message, plan_tier, traffic_limit, traffic_used
		) VALUES (
			$1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12,
			$13, $14, $15, $16, $17, $18, $19, $20, $21, $22
		)
	`

	_, err := r.pool.Exec(ctx, query,
		res.ID, res.SubscriptionID, res.UserID, res.ResourceType, res.Provider, res.Region,
		res.InstanceID, res.PublicIP, res.PrivateIP, res.APIPort, res.APIKey, res.VlessPort,
		res.SSPort, res.PublicKey, res.ShortID, res.SSHPrivateKey, res.SSHKeyName,
		res.Status, res.ErrorMessage, res.PlanTier, res.TrafficLimit, res.TrafficUsed,
	)
	if err != nil {
		return fmt.Errorf("insert resource: %w", err)
	}

	return nil
}

// GetByID retrieves a resource by ID
func (r *ResourceRepository) GetByID(ctx context.Context, id string) (*models.Resource, error) {
	query := `
		SELECT id, subscription_id, user_id, resource_type, provider, region,
			   instance_id, public_ip, private_ip, api_port, api_key, vless_port,
			   ss_port, public_key, short_id, ssh_private_key, ssh_key_name,
			   status, error_message, plan_tier, traffic_limit, traffic_used,
			   created_at, updated_at, ready_at, deleted_at
		FROM fulfillment.resources
		WHERE id = $1
	`

	return r.scanResource(r.pool.QueryRow(ctx, query, id))
}

// GetBySubscriptionID retrieves resources for a subscription
func (r *ResourceRepository) GetBySubscriptionID(ctx context.Context, subscriptionID string) ([]*models.Resource, error) {
	query := `
		SELECT id, subscription_id, user_id, resource_type, provider, region,
			   instance_id, public_ip, private_ip, api_port, api_key, vless_port,
			   ss_port, public_key, short_id, ssh_private_key, ssh_key_name,
			   status, error_message, plan_tier, traffic_limit, traffic_used,
			   created_at, updated_at, ready_at, deleted_at
		FROM fulfillment.resources
		WHERE subscription_id = $1 AND deleted_at IS NULL
		ORDER BY created_at DESC
	`

	rows, err := r.pool.Query(ctx, query, subscriptionID)
	if err != nil {
		return nil, fmt.Errorf("query resources: %w", err)
	}
	defer rows.Close()

	return r.scanResources(rows)
}

// GetByUserID retrieves resources for a user
func (r *ResourceRepository) GetByUserID(ctx context.Context, userID string) ([]*models.Resource, error) {
	query := `
		SELECT id, subscription_id, user_id, resource_type, provider, region,
			   instance_id, public_ip, private_ip, api_port, api_key, vless_port,
			   ss_port, public_key, short_id, ssh_private_key, ssh_key_name,
			   status, error_message, plan_tier, traffic_limit, traffic_used,
			   created_at, updated_at, ready_at, deleted_at
		FROM fulfillment.resources
		WHERE user_id = $1 AND deleted_at IS NULL
		ORDER BY created_at DESC
	`

	rows, err := r.pool.Query(ctx, query, userID)
	if err != nil {
		return nil, fmt.Errorf("query resources: %w", err)
	}
	defer rows.Close()

	return r.scanResources(rows)
}

// GetActiveByUserAndType retrieves active resource for user and type (excludes deleted and failed)
func (r *ResourceRepository) GetActiveByUserAndType(ctx context.Context, userID, resourceType string) (*models.Resource, error) {
	query := `
		SELECT id, subscription_id, user_id, resource_type, provider, region,
			   instance_id, public_ip, private_ip, api_port, api_key, vless_port,
			   ss_port, public_key, short_id, ssh_private_key, ssh_key_name,
			   status, error_message, plan_tier, traffic_limit, traffic_used,
			   created_at, updated_at, ready_at, deleted_at
		FROM fulfillment.resources
		WHERE user_id = $1 AND resource_type = $2
		  AND status NOT IN ('deleted', 'failed')
		  AND deleted_at IS NULL
		ORDER BY created_at DESC
		LIMIT 1
	`

	return r.scanResource(r.pool.QueryRow(ctx, query, userID, resourceType))
}

// GetLatestByUserAndType retrieves the latest resource for user and type (includes failed, excludes deleted)
func (r *ResourceRepository) GetLatestByUserAndType(ctx context.Context, userID, resourceType string) (*models.Resource, error) {
	query := `
		SELECT id, subscription_id, user_id, resource_type, provider, region,
			   instance_id, public_ip, private_ip, api_port, api_key, vless_port,
			   ss_port, public_key, short_id, ssh_private_key, ssh_key_name,
			   status, error_message, plan_tier, traffic_limit, traffic_used,
			   created_at, updated_at, ready_at, deleted_at
		FROM fulfillment.resources
		WHERE user_id = $1 AND resource_type = $2
		  AND status != 'deleted'
		  AND deleted_at IS NULL
		ORDER BY created_at DESC
		LIMIT 1
	`

	return r.scanResource(r.pool.QueryRow(ctx, query, userID, resourceType))
}

// Update updates a resource
func (r *ResourceRepository) Update(ctx context.Context, res *models.Resource) error {
	query := `
		UPDATE fulfillment.resources SET
			instance_id = $1,
			public_ip = $2,
			private_ip = $3,
			api_port = $4,
			api_key = $5,
			vless_port = $6,
			ss_port = $7,
			public_key = $8,
			short_id = $9,
			ssh_private_key = $10,
			ssh_key_name = $11,
			status = $12,
			error_message = $13,
			traffic_used = $14,
			ready_at = $15,
			deleted_at = $16
		WHERE id = $17
	`

	_, err := r.pool.Exec(ctx, query,
		res.InstanceID, res.PublicIP, res.PrivateIP,
		res.APIPort, res.APIKey, res.VlessPort,
		res.SSPort, res.PublicKey, res.ShortID,
		res.SSHPrivateKey, res.SSHKeyName,
		res.Status, res.ErrorMessage, res.TrafficUsed,
		res.ReadyAt, res.DeletedAt, res.ID,
	)
	if err != nil {
		return fmt.Errorf("update resource: %w", err)
	}

	return nil
}

// UpdateStatus updates only the status
func (r *ResourceRepository) UpdateStatus(ctx context.Context, id, status string, errorMsg *string) error {
	query := `UPDATE fulfillment.resources SET status = $1, error_message = $2 WHERE id = $3`
	_, err := r.pool.Exec(ctx, query, status, errorMsg, id)
	if err != nil {
		return fmt.Errorf("update status: %w", err)
	}
	return nil
}

func (r *ResourceRepository) scanResource(row pgx.Row) (*models.Resource, error) {
	res := &models.Resource{}
	err := row.Scan(
		&res.ID, &res.SubscriptionID, &res.UserID, &res.ResourceType, &res.Provider, &res.Region,
		&res.InstanceID, &res.PublicIP, &res.PrivateIP, &res.APIPort, &res.APIKey, &res.VlessPort,
		&res.SSPort, &res.PublicKey, &res.ShortID, &res.SSHPrivateKey, &res.SSHKeyName,
		&res.Status, &res.ErrorMessage, &res.PlanTier, &res.TrafficLimit, &res.TrafficUsed,
		&res.CreatedAt, &res.UpdatedAt, &res.ReadyAt, &res.DeletedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("scan resource: %w", err)
	}
	return res, nil
}

func (r *ResourceRepository) scanResources(rows pgx.Rows) ([]*models.Resource, error) {
	var resources []*models.Resource
	for rows.Next() {
		res := &models.Resource{}
		err := rows.Scan(
			&res.ID, &res.SubscriptionID, &res.UserID, &res.ResourceType, &res.Provider, &res.Region,
			&res.InstanceID, &res.PublicIP, &res.PrivateIP, &res.APIPort, &res.APIKey, &res.VlessPort,
			&res.SSPort, &res.PublicKey, &res.ShortID, &res.SSHPrivateKey, &res.SSHKeyName,
			&res.Status, &res.ErrorMessage, &res.PlanTier, &res.TrafficLimit, &res.TrafficUsed,
			&res.CreatedAt, &res.UpdatedAt, &res.ReadyAt, &res.DeletedAt,
		)
		if err != nil {
			return nil, fmt.Errorf("scan resource row: %w", err)
		}
		resources = append(resources, res)
	}
	return resources, rows.Err()
}
