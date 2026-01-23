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

// Create creates a new resource log entry
func (r *LogRepository) Create(ctx context.Context, logEntry *models.ResourceLog) error {
	if logEntry.ID == "" {
		logEntry.ID = uuid.New().String()
	}

	query := `
		INSERT INTO fulfillment.resource_logs (id, resource_id, action, status, message, metadata)
		VALUES ($1, $2, $3, $4, $5, $6)
	`

	_, err := r.pool.Exec(ctx, query,
		logEntry.ID, logEntry.ResourceID, logEntry.Action, logEntry.Status, logEntry.Message, logEntry.Metadata,
	)
	if err != nil {
		return fmt.Errorf("insert resource log: %w", err)
	}

	return nil
}

// GetByResourceID retrieves logs for a resource
func (r *LogRepository) GetByResourceID(ctx context.Context, resourceID string, limit int) ([]*models.ResourceLog, error) {
	if limit <= 0 {
		limit = 50
	}

	query := `
		SELECT id, resource_id, action, status, message, metadata, created_at
		FROM fulfillment.resource_logs
		WHERE resource_id = $1
		ORDER BY created_at DESC
		LIMIT $2
	`

	rows, err := r.pool.Query(ctx, query, resourceID, limit)
	if err != nil {
		return nil, fmt.Errorf("query resource logs: %w", err)
	}
	defer rows.Close()

	var logEntries []*models.ResourceLog
	for rows.Next() {
		logEntry := &models.ResourceLog{}
		err := rows.Scan(
			&logEntry.ID, &logEntry.ResourceID, &logEntry.Action, &logEntry.Status,
			&logEntry.Message, &logEntry.Metadata, &logEntry.CreatedAt,
		)
		if err != nil {
			return nil, fmt.Errorf("scan resource log: %w", err)
		}
		logEntries = append(logEntries, logEntry)
	}

	return logEntries, rows.Err()
}

// LogAction is a helper to log an action
func (r *LogRepository) LogAction(ctx context.Context, resourceID, action, status, message string) error {
	logEntry := &models.ResourceLog{
		ResourceID: resourceID,
		Action:     action,
		Status:     status,
		Message:    message,
	}
	return r.Create(ctx, logEntry)
}

// LogActionWithMetadata is a helper to log an action with metadata
func (r *LogRepository) LogActionWithMetadata(ctx context.Context, resourceID, action, status, message string, metadata map[string]interface{}) error {
	logEntry := &models.ResourceLog{
		ResourceID: resourceID,
		Action:     action,
		Status:     status,
		Message:    message,
		Metadata:   metadata,
	}
	return r.Create(ctx, logEntry)
}
