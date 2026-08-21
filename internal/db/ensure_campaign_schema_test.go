package db

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// 真 PG 回归：EnsureCampaignSchema 的 residential 回填【不得】把活动面行串成 residential。
//
// ★P1（2026-08-21）：回填原本只按 service_tier 判定，promo × residential 这个合法组合会被匹配上，
// 每次服务重启（autodeploy 每轮都重启）都把 product_face='promo' 改写成 'residential'，
// 与付费住宅套餐挤同一面 → LIMIT 1 让活动券 10GB 顶替付费 100GB。
// 该缺陷在【启动路径】，provision 侧的内存 fake 测试（TestCampaignFace_NoCrossoverWithPaidResidential）
// 结构上挡不住，故必须在真 SQL 上钉死。
// 未配 TEST_DATABASE_URL 则 skip（沿本仓惯例）。
func TestEnsureCampaignSchema_BackfillPreservesCampaignFace(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping EnsureCampaignSchema DB test")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()

	if _, err := pool.Exec(ctx, `CREATE SCHEMA IF NOT EXISTS fulfillment`); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	// 最小可复现表：只保留本用例涉及的列。
	if _, err := pool.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS fulfillment.vpn_provisions (
			id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			user_id VARCHAR(256) NOT NULL,
			service_tier VARCHAR(32) NOT NULL DEFAULT 'standard',
			plan_tier VARCHAR(32),
			is_current BOOLEAN NOT NULL DEFAULT TRUE,
			product_face VARCHAR(16) NOT NULL DEFAULT 'basic'
		)`); err != nil {
		t.Fatalf("create table: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM fulfillment.vpn_provisions WHERE user_id = 'u-p1-backfill'`)
	})
	if _, err := pool.Exec(ctx, `DELETE FROM fulfillment.vpn_provisions WHERE user_id = 'u-p1-backfill'`); err != nil {
		t.Fatalf("pre-clean: %v", err)
	}

	// 生产四类行的最小复刻。
	rows := []struct{ id, plan, tier, face string }{
		{"11111111-1111-1111-1111-111111111111", "residential", "residential", "residential"}, // 付费住宅
		{"22222222-2222-2222-2222-222222222222", "basic", "standard", "basic"},                // 付费基础
		{"33333333-3333-3333-3333-333333333333", "promo", "standard", "promo"},                // 活动·标准
		{"44444444-4444-4444-4444-444444444444", "promo", "residential", "promo"},             // ★活动·住宅
		{"55555555-5555-5555-5555-555555555555", "promo", "residential", "campaign"},          // 改名前旧值
	}
	for _, r := range rows {
		if _, err := pool.Exec(ctx,
			`INSERT INTO fulfillment.vpn_provisions (id, user_id, plan_tier, service_tier, product_face)
			 VALUES ($1, 'u-p1-backfill', $2, $3, $4)`, r.id, r.plan, r.tier, r.face); err != nil {
			t.Fatalf("insert %s: %v", r.id, err)
		}
	}

	// 模拟一次服务启动。
	if err := EnsureCampaignSchema(ctx, pool); err != nil {
		t.Fatalf("EnsureCampaignSchema: %v", err)
	}

	assertFace := func(id, want string) {
		t.Helper()
		var got string
		if err := pool.QueryRow(ctx,
			`SELECT product_face FROM fulfillment.vpn_provisions WHERE id = $1`, id).Scan(&got); err != nil {
			t.Fatalf("query %s: %v", id, err)
		}
		if got != want {
			t.Fatalf("★串面：id=%s product_face = %q, want %q", id, got, want)
		}
	}
	// 活动面两行必须原封不动——这是本用例的核心断言。
	assertFace("44444444-4444-4444-4444-444444444444", "promo")
	assertFace("55555555-5555-5555-5555-555555555555", "campaign")
	// 付费面行不受影响（回填对它们的既有语义必须保持）。
	assertFace("11111111-1111-1111-1111-111111111111", "residential")
	assertFace("22222222-2222-2222-2222-222222222222", "basic")
	assertFace("33333333-3333-3333-3333-333333333333", "promo")
}

// 回填的正向能力不能被误伤：service_tier=residential 的【付费】行 face 不对时仍须被纠正。
func TestEnsureCampaignSchema_BackfillStillFixesPaidResidential(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping EnsureCampaignSchema DB test")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()

	if _, err := pool.Exec(ctx, `CREATE SCHEMA IF NOT EXISTS fulfillment`); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS fulfillment.vpn_provisions (
			id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			user_id VARCHAR(256) NOT NULL,
			service_tier VARCHAR(32) NOT NULL DEFAULT 'standard',
			plan_tier VARCHAR(32),
			is_current BOOLEAN NOT NULL DEFAULT TRUE,
			product_face VARCHAR(16) NOT NULL DEFAULT 'basic'
		)`); err != nil {
		t.Fatalf("create table: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM fulfillment.vpn_provisions WHERE user_id = 'u-p1-paidfix'`)
	})
	if _, err := pool.Exec(ctx, `DELETE FROM fulfillment.vpn_provisions WHERE user_id = 'u-p1-paidfix'`); err != nil {
		t.Fatalf("pre-clean: %v", err)
	}
	// 老码窗口期新建的付费住宅行落成了 basic，回填应把它纠正为 residential。
	const id = "66666666-6666-6666-6666-666666666666"
	if _, err := pool.Exec(ctx,
		`INSERT INTO fulfillment.vpn_provisions (id, user_id, plan_tier, service_tier, product_face)
		 VALUES ($1, 'u-p1-paidfix', 'residential', 'residential', 'basic')`, id); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := EnsureCampaignSchema(ctx, pool); err != nil {
		t.Fatalf("EnsureCampaignSchema: %v", err)
	}
	var got string
	if err := pool.QueryRow(ctx,
		`SELECT product_face FROM fulfillment.vpn_provisions WHERE id = $1`, id).Scan(&got); err != nil {
		t.Fatalf("query: %v", err)
	}
	if got != "residential" {
		t.Fatalf("回填正向能力被误伤：product_face = %q, want residential", got)
	}
}
