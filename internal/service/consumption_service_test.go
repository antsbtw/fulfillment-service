package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wenwu/saas-platform/fulfillment-service/internal/client"
	"github.com/wenwu/saas-platform/fulfillment-service/internal/models"
)

type fakeAppleVPN struct{ rows []*models.VPNProvision }

func (f fakeAppleVPN) ListByUserChannelFace(_ context.Context, _, channel, face string) ([]*models.VPNProvision, error) {
	var out []*models.VPNProvision
	for _, r := range f.rows {
		if r.Channel == channel && r.ProductFace == face {
			out = append(out, r)
		}
	}
	return out, nil
}

type fakeOtunUser struct {
	used int64
	err  error
}

func (f fakeOtunUser) GetUser(context.Context, string) (*client.VPNUserInfo, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &client.VPNUserInfo{TrafficUsed: f.used}, nil
}

type fakeHosting struct{ nodes []*models.HostingProvision }

func (f fakeHosting) ListAliveSince(context.Context, string, time.Time) ([]*models.HostingProvision, error) {
	return f.nodes, nil
}

type fakeTraffic map[string]int64

func (f fakeTraffic) GetNodeTraffic(_ context.Context, id string) (*client.NodeTraffic, error) {
	v, ok := f[id]
	if !ok {
		return nil, errors.New("404")
	}
	return &client.NodeTraffic{NodeID: id, TrafficUsed: v}, nil
}

func ts(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}
func tp(s string) *time.Time { t := ts(s); return &t }
func sp(s string) *string    { return &s }

// link19900511(11-15):Apple 重新订阅没开通;账号上的流量来自 11-04 Stripe 那笔(apple 行已 converted)。
func TestConsumption_OTunResubscribeNotProvisioned(t *testing.T) {
	rows := []*models.VPNProvision{{
		ID: "2986d97b", Channel: "apple", ProductFace: "residential", Status: "converted",
		OtunUUID: sp("3a5f782b"), ExpireAt: tp("2026-11-14T02:04:52Z"), UpdatedAt: ts("2026-11-04T10:43:15Z"),
	}}
	s := NewConsumptionService(fakeAppleVPN{rows}, fakeOtunUser{used: 190 << 30}, nil, nil)
	got, err := s.Get(context.Background(), ConsumptionQuery{
		UserID: "a44e07b9", App: "otun", ServiceType: "vpn", PlanTier: "residential",
		PurchaseAt: ts("2026-11-15T04:37:35Z"), PeriodEnd: ts("2026-12-15T04:37:35Z"), TrafficGB: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Delivered || got.UndeliveredReason != "not_provisioned" || got.UsedBytes != 0 {
		t.Fatalf("want undelivered, got %+v", got)
	}
}

// 正常首购:verify 开通在购买后几秒,到期 = Apple 本期到期。用量封顶为本笔额度。
func TestConsumption_OTunDeliveredUsageCapped(t *testing.T) {
	rows := []*models.VPNProvision{{
		ID: "r1", Channel: "apple", ProductFace: "residential", Status: "active", IsCurrent: true,
		OtunUUID: sp("u"), ExpireAt: tp("2026-12-14T09:59:00Z"), UpdatedAt: ts("2026-11-14T09:59:10Z"),
	}}
	s := NewConsumptionService(fakeAppleVPN{rows}, fakeOtunUser{used: 150 << 30}, nil, nil)
	s.now = func() time.Time { return ts("2026-11-18T10:00:00Z") }
	got, err := s.Get(context.Background(), ConsumptionQuery{
		UserID: "x", App: "otun", ServiceType: "vpn", PlanTier: "residential",
		PurchaseAt: ts("2026-11-14T09:59:00Z"), PeriodEnd: ts("2026-12-14T09:59:00Z"), TrafficGB: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !got.Delivered || got.UsedBytes != 100<<30 || got.UsagePercent != 100 || got.ActiveDays != 4 {
		t.Fatalf("got %+v", got)
	}
}

// 上一期的行(到期早于本期 - 7 天)不算本笔交付;basic 面的 apple 行也不能顶 residential。
func TestConsumption_OTunOldPeriodOrOtherFaceNotCounted(t *testing.T) {
	rows := []*models.VPNProvision{
		{ID: "old", Channel: "apple", ProductFace: "residential", OtunUUID: sp("u"),
			ExpireAt: tp("2026-11-07T15:51:06Z"), UpdatedAt: ts("2026-11-15T05:00:00Z")},
		{ID: "basic", Channel: "apple", ProductFace: "basic", OtunUUID: sp("b"),
			ExpireAt: tp("2026-12-20T00:00:00Z"), UpdatedAt: ts("2026-11-15T05:00:00Z")},
	}
	s := NewConsumptionService(fakeAppleVPN{rows}, fakeOtunUser{}, nil, nil)
	got, err := s.Get(context.Background(), ConsumptionQuery{
		UserID: "x", App: "otun", ServiceType: "vpn", PlanTier: "residential",
		PurchaseAt: ts("2026-11-15T04:37:35Z"), PeriodEnd: ts("2026-12-15T04:37:35Z"), TrafficGB: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Delivered {
		t.Fatalf("want undelivered, got %+v", got)
	}
}

// 已交付但取不到用量:报错,不能拿 0 冒充(设计 §4.6)。
func TestConsumption_OTunUsageUnavailableIsError(t *testing.T) {
	rows := []*models.VPNProvision{{ID: "r1", Channel: "apple", ProductFace: "basic", OtunUUID: sp("u"),
		ExpireAt: tp("2026-12-14T00:00:00Z"), UpdatedAt: ts("2026-11-14T00:00:05Z")}}
	s := NewConsumptionService(fakeAppleVPN{rows}, fakeOtunUser{err: errors.New("down")}, nil, nil)
	_, err := s.Get(context.Background(), ConsumptionQuery{UserID: "x", App: "otun", ServiceType: "vpn",
		PlanTier: "basic", PurchaseAt: ts("2026-11-14T00:00:00Z"), PeriodEnd: ts("2026-12-14T00:00:00Z"), TrafficGB: 100})
	if err == nil {
		t.Fatal("want error")
	}
}

// 32282f8e(10-22):11 台托管机都没开起来 → 未交付。
func TestConsumption_HostingNeverReady(t *testing.T) {
	nodes := []*models.HostingProvision{{HostingNodeID: "obox-0-56f41c"}, {HostingNodeID: "obox-0-40f97c"}}
	s := NewConsumptionService(nil, nil, fakeHosting{nodes}, fakeTraffic{})
	got, err := s.Get(context.Background(), ConsumptionQuery{UserID: "x", App: "obox", ServiceType: "hosting",
		PurchaseAt: ts("2026-10-22T19:55:00Z"), TrafficGB: 1024})
	if err != nil {
		t.Fatal(err)
	}
	if got.Delivered || got.UndeliveredReason != "node_never_ready" {
		t.Fatalf("got %+v", got)
	}
}

// f9d02ff1(11-01):换了几次机器,新机继承已用,取最大值;已删机器读不到也不影响。
func TestConsumption_HostingMaxAcrossNodes(t *testing.T) {
	nodes := []*models.HostingProvision{
		{HostingNodeID: "a", ReadyAt: tp("2026-11-01T01:46:04Z")},
		{HostingNodeID: "gone", ReadyAt: tp("2026-11-01T05:11:33Z")},
		{HostingNodeID: "c", ReadyAt: tp("2026-11-01T07:42:38Z")},
		{HostingNodeID: "never"},
	}
	s := NewConsumptionService(nil, nil, fakeHosting{nodes}, fakeTraffic{"a": 3 << 30, "c": 12 << 30})
	s.now = func() time.Time { return ts("2026-11-02T03:00:00Z") }
	got, err := s.Get(context.Background(), ConsumptionQuery{UserID: "x", App: "obox", ServiceType: "hosting",
		PurchaseAt: ts("2026-11-01T01:44:00Z"), TrafficGB: 1024})
	if err != nil {
		t.Fatal(err)
	}
	if !got.Delivered || got.UsedBytes != 12<<30 || got.DeliveredAt == nil || !got.DeliveredAt.Equal(ts("2026-11-01T01:46:04Z")) {
		t.Fatalf("got %+v", got)
	}
}

func TestConsumption_HostingNoNodeAndProUnsupported(t *testing.T) {
	s := NewConsumptionService(nil, nil, fakeHosting{}, fakeTraffic{})
	got, _ := s.Get(context.Background(), ConsumptionQuery{UserID: "x", App: "obox", ServiceType: "hosting",
		PurchaseAt: ts("2026-11-01T00:00:00Z")})
	if got.Delivered || got.UndeliveredReason != "no_node" {
		t.Fatalf("got %+v", got)
	}
	pro, _ := s.Get(context.Background(), ConsumptionQuery{UserID: "x", App: "obox", ServiceType: "license",
		PurchaseAt: ts("2026-11-01T00:00:00Z")})
	if pro.Supported {
		t.Fatalf("pro should be unsupported: %+v", pro)
	}
}
