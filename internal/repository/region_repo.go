package repository

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/wenwu/saas-platform/fulfillment-service/internal/models"
)

type RegionRepository struct {
	pool *pgxpool.Pool
}

func NewRegionRepository(pool *pgxpool.Pool) *RegionRepository {
	return &RegionRepository{pool: pool}
}

// GetAll retrieves all regions
func (r *RegionRepository) GetAll(ctx context.Context) ([]*models.Region, error) {
	query := `
		SELECT code, name, provider, available, created_at, updated_at
		FROM fulfillment.regions
		ORDER BY name
	`

	rows, err := r.pool.Query(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("query regions: %w", err)
	}
	defer rows.Close()

	var regions []*models.Region
	for rows.Next() {
		region := &models.Region{}
		err := rows.Scan(
			&region.Code, &region.Name, &region.Provider,
			&region.Available, &region.CreatedAt, &region.UpdatedAt,
		)
		if err != nil {
			return nil, fmt.Errorf("scan region: %w", err)
		}
		regions = append(regions, region)
	}

	return regions, rows.Err()
}

// GetAvailable retrieves available regions
func (r *RegionRepository) GetAvailable(ctx context.Context) ([]*models.Region, error) {
	query := `
		SELECT code, name, provider, available, created_at, updated_at
		FROM fulfillment.regions
		WHERE available = true
		ORDER BY name
	`

	rows, err := r.pool.Query(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("query available regions: %w", err)
	}
	defer rows.Close()

	var regions []*models.Region
	for rows.Next() {
		region := &models.Region{}
		err := rows.Scan(
			&region.Code, &region.Name, &region.Provider,
			&region.Available, &region.CreatedAt, &region.UpdatedAt,
		)
		if err != nil {
			return nil, fmt.Errorf("scan region: %w", err)
		}
		regions = append(regions, region)
	}

	return regions, rows.Err()
}

// GetByCode retrieves a region by code
func (r *RegionRepository) GetByCode(ctx context.Context, code string) (*models.Region, error) {
	query := `
		SELECT code, name, provider, available, created_at, updated_at
		FROM fulfillment.regions
		WHERE code = $1
	`

	region := &models.Region{}
	err := r.pool.QueryRow(ctx, query, code).Scan(
		&region.Code, &region.Name, &region.Provider,
		&region.Available, &region.CreatedAt, &region.UpdatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("get region by code: %w", err)
	}

	return region, nil
}

// Upsert creates or updates a region
func (r *RegionRepository) Upsert(ctx context.Context, region *models.Region) error {
	query := `
		INSERT INTO fulfillment.regions (code, name, provider, available)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (code) DO UPDATE SET
			name = EXCLUDED.name,
			provider = EXCLUDED.provider,
			available = EXCLUDED.available
	`

	_, err := r.pool.Exec(ctx, query, region.Code, region.Name, region.Provider, region.Available)
	if err != nil {
		return fmt.Errorf("upsert region: %w", err)
	}

	return nil
}
