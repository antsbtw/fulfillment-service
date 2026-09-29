package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wenwu/saas-platform/fulfillment-service/internal/client"
	"github.com/wenwu/saas-platform/fulfillment-service/internal/config"
)

type fakeRegionLister struct {
	calls   int
	err     error
	regions []client.CloudRegion
}

func (f *fakeRegionLister) ListCloudRegions(context.Context) ([]client.CloudRegion, error) {
	f.calls++
	return f.regions, f.err
}

func regionTestCatalog(f *fakeRegionLister) *RegionCatalog {
	cfg := &config.Config{}
	cfg.Hosting.RegionDeny = []string{"ap-south-1"}
	cfg.Hosting.RegionOrder = []string{"ap-northeast-1", "ap-southeast-1", "us-east-1"}
	return NewRegionCatalog(cfg, f, nil)
}

func TestRegionCatalog_FilterOrderName(t *testing.T) {
	f := &fakeRegionLister{regions: []client.CloudRegion{
		{Code: "us-east-1", Name: "Virginia", Available: true},
		{Code: "eu-north-1", Name: "Stockholm", Available: true},
		{Code: "ap-south-1", Name: "Mumbai", Available: true},
		{Code: "ap-southeast-1", Name: "Singapore", Available: false},
		{Code: "ap-northeast-1", Name: "Tokyo", Available: true},
		{Code: "ca-central-1", Name: "Montreal", Available: true},
	}}
	c := regionTestCatalog(f)
	got, err := c.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []struct {
		code, name string
		avail      bool
	}{
		{"ap-northeast-1", "Asia Pacific (Tokyo)", true},
		{"ap-southeast-1", "Asia Pacific (Singapore)", false},
		{"us-east-1", "US East (Virginia)", true},
		{"ca-central-1", "Canada (Montreal)", true},
		{"eu-north-1", "Europe (Stockholm)", true},
	}
	if len(got) != len(want) {
		t.Fatalf("got %+v", got)
	}
	for i, w := range want {
		g := got[i]
		if g.Code != w.code || g.Name != w.name || g.Available != w.avail || g.Provider != "lightsail" {
			t.Errorf("[%d] got %+v want %+v", i, g, w)
		}
	}

	if r, _ := c.Lookup(context.Background(), "ap-south-1"); r != nil {
		t.Error("denied region must not be creatable")
	}
	if r, _ := c.Lookup(context.Background(), "xx-nowhere-1"); r != nil {
		t.Error("unknown region must not be creatable")
	}
	if r, _ := c.Lookup(context.Background(), "ap-northeast-1"); r == nil || !r.Available {
		t.Error("listed available region must be creatable")
	}
	if f.calls != 1 {
		t.Fatalf("cache: calls=%d", f.calls)
	}
}

func TestRegionCatalog_StaleOnHostingError(t *testing.T) {
	f := &fakeRegionLister{regions: []client.CloudRegion{{Code: "ap-northeast-1", Name: "Tokyo", Available: true}}}
	c := regionTestCatalog(f)
	now := time.Now()
	c.now = func() time.Time { return now }
	c.List(context.Background())

	now = now.Add(2 * regionListCacheTTL)
	f.err = errors.New("hosting down")
	got, err := c.List(context.Background())
	if err != nil || len(got) != 1 || f.calls != 2 {
		t.Fatalf("stale fallback: got=%v err=%v calls=%d", got, err, f.calls)
	}
}

func TestCheckRegion(t *testing.T) {
	f := &fakeRegionLister{regions: []client.CloudRegion{
		{Code: "ap-northeast-1", Name: "Tokyo", Available: true},
		{Code: "ap-southeast-1", Name: "Singapore", Available: false},
	}}
	s := &ProvisionService{regions: regionTestCatalog(f)}
	ctx := context.Background()
	if r := s.checkRegion(ctx, "ap-northeast-1"); r != nil {
		t.Fatalf("available region rejected: %+v", r)
	}
	if r := s.checkRegion(ctx, "ap-southeast-1"); r == nil || r.Status != "region_unavailable" {
		t.Fatalf("unavailable: %+v", r)
	}
	if r := s.checkRegion(ctx, "ap-south-1"); r == nil || r.Status != "invalid_region" {
		t.Fatalf("not listed: %+v", r)
	}
}
