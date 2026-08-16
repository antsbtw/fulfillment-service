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

// EnsureCampaignSchema 幂等地保证迁移 010（vpn_provisions.product_face + 回填 + campaign_grants）已生效。
// ★验收 F2/F5 同型：新码读路径 vpnColumns 含 product_face，010 前起不来；老码在 010 后不写 product_face，
// 窗口期新建的 residential 行会落成 basic。新镜像启动即跑这组 IF NOT EXISTS 语句 + 幂等回填，把"跑迁移"和
// "起新码"合成一步；手工再跑 010 也无害。回填 UPDATE 每次启动都跑（只动 service_tier='residential' 且
// product_face 不对的行，campaign 行 service_tier=standard 不受影响）。
func EnsureCampaignSchema(ctx context.Context, pool *pgxpool.Pool) error {
	stmts := []string{
		`ALTER TABLE fulfillment.vpn_provisions ADD COLUMN IF NOT EXISTS product_face VARCHAR(16) NOT NULL DEFAULT 'basic'`,
		`UPDATE fulfillment.vpn_provisions SET product_face = 'residential' WHERE service_tier = 'residential' AND product_face <> 'residential'`,
		`CREATE INDEX IF NOT EXISTS idx_vpn_provisions_user_face_current ON fulfillment.vpn_provisions(user_id, product_face) WHERE is_current = TRUE`,
		`CREATE TABLE IF NOT EXISTS fulfillment.campaign_grants (
			subscription_id VARCHAR(256) PRIMARY KEY,
			user_id VARCHAR(256) NOT NULL,
			channel_sub_id VARCHAR(256),
			days INT NOT NULL DEFAULT 0,
			traffic_bytes BIGINT NOT NULL DEFAULT 0,
			status VARCHAR(16) NOT NULL DEFAULT 'active',
			applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			revoked_at TIMESTAMPTZ,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		`CREATE INDEX IF NOT EXISTS idx_campaign_grants_user_status ON fulfillment.campaign_grants(user_id, status)`,
	}
	for _, q := range stmts {
		if _, err := pool.Exec(ctx, q); err != nil {
			return fmt.Errorf("ensure campaign schema (migration 010): %w (stmt: %.70s)", err, q)
		}
	}
	return nil
}
