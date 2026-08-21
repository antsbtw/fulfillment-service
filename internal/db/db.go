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
// "起新码"合成一步；手工再跑 010 也无害。
//
// ★2026-08-21 P1 修复：回填 UPDATE 原本只按 service_tier 判定，注释里"campaign 行 service_tier=standard
// 不受影响"的前提，在活动面支持住宅线路（promo × residential）后【已不成立】——
// plan_tier='promo' 且 service_tier='residential' 的活动行会匹配上，被改写成 product_face='residential'，
// 与付费住宅套餐挤同一面；GetCurrentByUserAndServicePartition 的 LIMIT 1 让活动券 10GB 顶替付费 100GB，
// 且该读路径不受能力头门控（老客户端同样中招）。
// 因为发生在【启动路径】而非任何 provision 写路径，静态排查 provisionCampaign/ProductFaceFor/
// UpdateProjection/cleanup 全都扑空，且"改回 promo 后读一次仍是 promo"的对照实验也无法证伪——
// 真正的触发条件是【服务重启】（autodeploy timer 每轮拉新镜像都会重启）。
// 修复：回填显式排除活动面（沿用 repo 层 notCampaign 同款谓词，并排旧值 'campaign' 以免迁移 011 窗口期漏网）。
func EnsureCampaignSchema(ctx context.Context, pool *pgxpool.Pool) error {
	stmts := []string{
		`ALTER TABLE fulfillment.vpn_provisions ADD COLUMN IF NOT EXISTS product_face VARCHAR(16) NOT NULL DEFAULT 'basic'`,
		// ★活动面（promo/campaign）绝不参与本回填：promo × residential 是合法组合，其 face 必须留在 promo。
		`UPDATE fulfillment.vpn_provisions SET product_face = 'residential'
		   WHERE service_tier = 'residential' AND product_face <> 'residential'
		     AND COALESCE(product_face, 'basic') NOT IN ('promo', 'campaign')`,
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
