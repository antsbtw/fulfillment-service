package service

// 新式托管机开通逻辑的真库测试(回执 R-12 ③)。需要一个跑过全部迁移的空库:
//   FULFILLMENT_TEST_DSN=postgres://user:pw@127.0.0.1:5432/fulfillment_hv2_test?sslmode=disable
// 未设置则跳过。

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/wenwu/saas-platform/fulfillment-service/internal/client"
	"github.com/wenwu/saas-platform/fulfillment-service/internal/config"
	"github.com/wenwu/saas-platform/fulfillment-service/internal/models"
	"github.com/wenwu/saas-platform/fulfillment-service/internal/repository"
)

func hv2Pool(t *testing.T) *pgxpool.Pool {
	dsn := os.Getenv("FULFILLMENT_TEST_DSN")
	if dsn == "" {
		t.Skip("FULFILLMENT_TEST_DSN not set")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(context.Background(), `DELETE FROM fulfillment.hosting_provisions WHERE user_id LIKE 'hv2-%'`); err != nil {
		t.Fatal(err)
	}
	return pool
}

func hv2Service(pool *pgxpool.Pool, hostingURL string) *ProvisionService {
	cfg := &config.Config{}
	cfg.Hosting.CloudProvider = "aws"
	cfg.Hosting.DefaultRegion = "us-east-1"
	var hc *client.HostingClient
	if hostingURL != "" {
		hc = client.NewHostingClient(hostingURL, "adm")
	}
	return NewProvisionService(cfg, repository.NewHostingProvisionRepository(pool), repository.NewRegionRepository(pool),
		repository.NewLogRepository(pool), hc, client.NewSubscriptionClient("http://127.0.0.1:1", "x"), nil)
}

func TestProvision_DeferCreatesNothing(t *testing.T) {
	pool := hv2Pool(t)
	s := hv2Service(pool, "") // hostingClient=nil:任何建机调用都会 panic
	resp, err := s.Provision(context.Background(), &models.ProvisionRequest{
		UserID: "hv2-u1", SubscriptionID: "sub-hv2-1", PlanTier: "basic", DeferProvision: true,
	})
	if err != nil || resp.Status != "awaiting_setup" {
		t.Fatalf("resp=%+v err=%v", resp, err)
	}
	var n int
	pool.QueryRow(context.Background(), `SELECT count(*) FROM fulfillment.hosting_provisions WHERE user_id='hv2-u1'`).Scan(&n)
	if n != 0 {
		t.Fatalf("deferred provision created %d rows", n)
	}
}

func seedActive(t *testing.T, s *ProvisionService, id, user, tier, kind string) {
	hp := &models.HostingProvision{ID: id, SubscriptionID: "sub-" + id, UserID: user, Channel: "apple",
		HostingNodeID: "obox-0-" + id, Provider: "aws", Region: "ap-northeast-1", Status: models.StatusActive,
		PlanTier: tier, TrafficLimit: 1, NodeKind: kind}
	if err := s.hostingRepo.Create(context.Background(), hp); err != nil {
		t.Fatal(err)
	}
}

func TestProvision_TierChangeMarksRebuild(t *testing.T) {
	pool := hv2Pool(t)
	s := hv2Service(pool, "") // 不许调 hosting 删机
	ctx := context.Background()

	// 新式机换套餐:只打标记
	seedActive(t, s, "11111111-1111-1111-1111-111111111111", "hv2-u2", "basic", models.NodeKindHostedV2)
	resp, err := s.Provision(ctx, &models.ProvisionRequest{UserID: "hv2-u2", SubscriptionID: "sub-new", PlanTier: "pro"})
	if err != nil {
		t.Fatal(err)
	}
	hp, _ := s.hostingRepo.GetByID(ctx, resp.ResourceID)
	if hp == nil || !hp.NeedsRebuild || hp.Status != models.StatusActive || hp.NodeKind != models.NodeKindHostedV2 {
		t.Fatalf("v2 tier change: %+v", hp)
	}

	// 老式机 + 新版 App 购买(defer)换套餐:同样只打标记,不删了重建一台老式机
	seedActive(t, s, "22222222-2222-2222-2222-222222222222", "hv2-u3", "basic", models.NodeKindHostedLegacy)
	resp, err = s.Provision(ctx, &models.ProvisionRequest{UserID: "hv2-u3", SubscriptionID: "sub-new3", PlanTier: "pro", DeferProvision: true})
	if err != nil {
		t.Fatal(err)
	}
	hp, _ = s.hostingRepo.GetByID(ctx, resp.ResourceID)
	if hp == nil || !hp.NeedsRebuild || hp.Status != models.StatusActive {
		t.Fatalf("legacy+defer tier change: %+v", hp)
	}
}

func TestProvision_OwnerKeyCreatesV2AndForwardsKey(t *testing.T) {
	pool := hv2Pool(t)
	got := make(chan map[string]any, 1)
	hosting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/nodes"):
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			select {
			case got <- body:
			default:
			}
			w.Write([]byte(`{"node_id":"obox-0-hv2x","status":"creating"}`))
		case r.Method == http.MethodGet:
			w.Write([]byte(`{"node_id":"obox-0-hv2x","status":"failed","error_message":"test"}`))
		default:
			w.Write([]byte(`{}`))
		}
	}))
	defer hosting.Close()
	s := hv2Service(pool, hosting.URL)

	key := edKey(t) + " iPhone"
	resp, err := s.Provision(context.Background(), &models.ProvisionRequest{
		UserID: "hv2-u4", SubscriptionID: "sub-hv2-4", PlanTier: "basic", Region: "ap-northeast-1", OwnerKey: key,
	})
	if err != nil {
		t.Fatal(err)
	}
	hp, _ := s.hostingRepo.GetByID(context.Background(), resp.ResourceID)
	if hp == nil || hp.NodeKind != models.NodeKindHostedV2 {
		t.Fatalf("row: %+v", hp)
	}
	select {
	case body := <-got:
		if body["owner_key"] != key {
			t.Fatalf("owner_key not forwarded: %v", body["owner_key"])
		}
	case <-time.After(5 * time.Second):
		t.Fatal("hosting CreateNode not called")
	}
	// owner key 不入库:整行里找不到它
	var row string
	pool.QueryRow(context.Background(), `SELECT row_to_json(h)::text FROM fulfillment.hosting_provisions h WHERE id=$1`, resp.ResourceID).Scan(&row)
	if strings.Contains(row, strings.Fields(key)[1]) {
		t.Fatal("owner key persisted")
	}
}
