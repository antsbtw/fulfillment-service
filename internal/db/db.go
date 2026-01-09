package db

import (
	"context"
	"fmt"
	"log"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/wenwu/saas-platform/fulfillment-service/internal/config"
)

type Database struct {
	Pool   *pgxpool.Pool
	Schema string
}

func New(cfg *config.Config) (*Database, error) {
	ctx := context.Background()

	poolConfig, err := pgxpool.ParseConfig(cfg.Database.DSN())
	if err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	poolConfig.MaxConns = 25
	poolConfig.MinConns = 5

	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
	}

	if err := pool.Ping(ctx); err != nil {
		return nil, fmt.Errorf("ping: %w", err)
	}

	// Set search path to schema
	schema := cfg.Database.Schema
	_, err = pool.Exec(ctx, fmt.Sprintf("SET search_path TO %s, public", schema))
	if err != nil {
		return nil, fmt.Errorf("set search_path: %w", err)
	}

	log.Printf("[db] Connected to PostgreSQL: %s/%s (schema: %s)",
		cfg.Database.Host, cfg.Database.DBName, schema)

	return &Database{
		Pool:   pool,
		Schema: schema,
	}, nil
}

func (d *Database) Close() {
	if d.Pool != nil {
		d.Pool.Close()
	}
}

// NewPool creates a simple connection pool from DSN
func NewPool(dsn string) (*pgxpool.Pool, error) {
	ctx := context.Background()

	poolConfig, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	poolConfig.MaxConns = 25
	poolConfig.MinConns = 5

	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
	}

	if err := pool.Ping(ctx); err != nil {
		return nil, fmt.Errorf("ping: %w", err)
	}

	log.Printf("[db] Connected to PostgreSQL")

	return pool, nil
}
