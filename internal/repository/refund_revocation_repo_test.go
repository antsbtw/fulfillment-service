package repository

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/wenwu/saas-platform/fulfillment-service/internal/db"
	"github.com/wenwu/saas-platform/fulfillment-service/internal/models"
)

// 真 PG:EnsureRefundRevocationSchema 建表 + 按 ref 幂等写入/读回。未配 TEST_DATABASE_URL 则 skip。
func TestRefundRevocationRepo_RoundTrip(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if _, err := pool.Exec(ctx, `CREATE SCHEMA IF NOT EXISTS fulfillment`); err != nil {
		t.Fatal(err)
	}
	if err := db.EnsureRefundRevocationSchema(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if err := db.EnsureRefundRevocationSchema(ctx, pool); err != nil { // 幂等
		t.Fatal(err)
	}
	r := NewRefundRevocationRepository(pool)
	ref := "pay-" + time.Now().Format("150405.000000")
	if _, err := r.Get(ctx, ref); err != ErrNotFound {
		t.Fatalf("missing ref must be ErrNotFound, got %v", err)
	}
	old, nw := time.Now().Add(48*time.Hour).UTC().Truncate(time.Second), time.Now().UTC().Truncate(time.Second)
	v := &models.RefundRevocation{Ref: ref, UserID: "u1", PlanTier: "basic", ProvisionID: "p1", Days: 30, TrafficBytes: 5,
		OldExpireAt: &old, NewExpireAt: &nw, NewTrafficLimit: 7, Disabled: true, Reason: "test"}
	if ok, err := r.InsertIfAbsent(ctx, v); err != nil || !ok {
		t.Fatalf("insert: ok=%v err=%v", ok, err)
	}
	if ok, err := r.InsertIfAbsent(ctx, v); err != nil || ok {
		t.Fatalf("duplicate ref must not insert: ok=%v err=%v", ok, err)
	}
	got, err := r.Get(ctx, ref)
	if err != nil || got.Days != 30 || got.ProvisionID != "p1" || !got.Disabled || !got.OldExpireAt.Equal(old) || got.NewTrafficLimit != 7 {
		t.Fatalf("got %+v err=%v", got, err)
	}
	// 无开通的 noop 记录:provision_id 为空
	noop := &models.RefundRevocation{Ref: ref + "-n", UserID: "u2", PlanTier: "residential", Noop: true}
	if ok, err := r.InsertIfAbsent(ctx, noop); err != nil || !ok {
		t.Fatal(err)
	}
	if g, _ := r.Get(ctx, ref+"-n"); g == nil || !g.Noop || g.ProvisionID != "" || g.OldExpireAt != nil {
		t.Fatalf("noop round trip: %+v", g)
	}
}
