package repository

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/wenwu/saas-platform/fulfillment-service/internal/models"
)

type LogRepository struct {
	pool *pgxpool.Pool
}

func NewLogRepository(pool *pgxpool.Pool) *LogRepository {
	return &LogRepository{pool: pool}
}

// Create creates a new provision log entry
func (r *LogRepository) Create(ctx context.Context, logEntry *models.ProvisionLog) error {
	if logEntry.ID == "" {
		logEntry.ID = uuid.New().String()
	}

	query := `
		INSERT INTO fulfillment.provision_logs (id, provision_id, provision_type, action, status, message, metadata)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
	`

	_, err := r.pool.Exec(ctx, query,
		logEntry.ID, logEntry.ProvisionID, logEntry.ProvisionType,
		logEntry.Action, logEntry.Status, logEntry.Message, logEntry.Metadata,
	)
	if err != nil {
		return fmt.Errorf("insert provision log: %w", err)
	}

	return nil
}

// GetByProvisionID retrieves logs for a provision
func (r *LogRepository) GetByProvisionID(ctx context.Context, provisionID string, limit int) ([]*models.ProvisionLog, error) {
	if limit <= 0 {
		limit = 50
	}

	query := `
		SELECT id, provision_id, provision_type, action, status, message, metadata, created_at
		FROM fulfillment.provision_logs
		WHERE provision_id = $1
		ORDER BY created_at DESC
		LIMIT $2
	`

	rows, err := r.pool.Query(ctx, query, provisionID, limit)
	if err != nil {
		return nil, fmt.Errorf("query provision logs: %w", err)
	}
	defer rows.Close()

	var logEntries []*models.ProvisionLog
	for rows.Next() {
		logEntry := &models.ProvisionLog{}
		err := rows.Scan(
			&logEntry.ID, &logEntry.ProvisionID, &logEntry.ProvisionType,
			&logEntry.Action, &logEntry.Status,
			&logEntry.Message, &logEntry.Metadata, &logEntry.CreatedAt,
		)
		if err != nil {
			return nil, fmt.Errorf("scan provision log: %w", err)
		}
		logEntries = append(logEntries, logEntry)
	}

	return logEntries, rows.Err()
}

// LogAction is a helper to log an action
func (r *LogRepository) LogAction(ctx context.Context, provisionID, provisionType, action, status, message string) error {
	logEntry := &models.ProvisionLog{
		ProvisionID:   provisionID,
		ProvisionType: provisionType,
		Action:        action,
		Status:        status,
		Message:       message,
	}
	return r.Create(ctx, logEntry)
}

// LogActionWithMetadata is a helper to log an action with metadata
func (r *LogRepository) LogActionWithMetadata(ctx context.Context, provisionID, provisionType, action, status, message string, metadata map[string]interface{}) error {
	logEntry := &models.ProvisionLog{
		ProvisionID:   provisionID,
		ProvisionType: provisionType,
		Action:        action,
		Status:        status,
		Message:       message,
		Metadata:      metadata,
	}
	return r.Create(ctx, logEntry)
}
