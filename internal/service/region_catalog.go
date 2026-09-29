package service

import (
	"context"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/wenwu/saas-platform/fulfillment-service/internal/client"
	"github.com/wenwu/saas-platform/fulfillment-service/internal/config"
	"github.com/wenwu/saas-platform/fulfillment-service/internal/models"
	"github.com/wenwu/saas-platform/fulfillment-service/internal/repository"
)

// 托管区域目录(客户端需求 2026-09-29):区域来源 = hosting 的 Lightsail GetRegions(hosting 缓存 1h),
// 这里套运营名单(deny + 排序)后下发 /public/regions,POST /my/node 按同一份列表校验。
// hosting 不可达且没有缓存时退回 fulfillment.regions 表(旧的固定 5 区),接口不因此 500。

const regionListCacheTTL = time.Minute // 只挡每请求一跳内网;AWS 侧 1h 缓存在 hosting

type regionLister interface {
	ListCloudRegions(ctx context.Context) ([]client.CloudRegion, error)
}

type RegionCatalog struct {
	hosting regionLister
	repo    *repository.RegionRepository
	deny    map[string]bool
	order   map[string]int
	now     func() time.Time

	mu        sync.Mutex
	cached    []models.RegionInfo
	fetchedAt time.Time
}

func NewRegionCatalog(cfg *config.Config, hosting regionLister, repo *repository.RegionRepository) *RegionCatalog {
	c := &RegionCatalog{hosting: hosting, repo: repo, deny: map[string]bool{}, order: map[string]int{}, now: time.Now}
	if cfg != nil {
		for _, code := range cfg.Hosting.RegionDeny {
			c.deny[code] = true
		}
		for i, code := range cfg.Hosting.RegionOrder {
			if _, dup := c.order[code]; !dup {
				c.order[code] = i
			}
		}
	}
	return c
}

// List 对外区域列表(已过 deny、已排序;available=false 的也在,App 自行隐藏)。
func (c *RegionCatalog) List(ctx context.Context) ([]models.RegionInfo, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cached != nil && c.now().Sub(c.fetchedAt) < regionListCacheTTL {
		return c.cached, nil
	}
	list, err := c.fromHosting(ctx)
	if err != nil {
		log.Printf("[RegionCatalog] hosting regions unavailable: %v", err)
		if c.cached != nil {
			return c.cached, nil
		}
		return c.fromTable(ctx)
	}
	c.cached, c.fetchedAt = list, c.now()
	return list, nil
}

// Lookup 按 code 查对外列表里的区域;不在列表(未知或被 deny)返回 nil。
func (c *RegionCatalog) Lookup(ctx context.Context, code string) (*models.RegionInfo, error) {
	list, err := c.List(ctx)
	if err != nil {
		return nil, err
	}
	for i := range list {
		if list[i].Code == code {
			return &list[i], nil
		}
	}
	return nil, nil
}

func (c *RegionCatalog) fromHosting(ctx context.Context) ([]models.RegionInfo, error) {
	if c.hosting == nil {
		return nil, errNoHostingClient
	}
	ctx, cancel := context.WithTimeout(ctx, 40*time.Second)
	defer cancel()
	regions, err := c.hosting.ListCloudRegions(ctx)
	if err != nil {
		return nil, err
	}
	names := c.tableNames(ctx)
	out := make([]models.RegionInfo, 0, len(regions))
	for _, r := range regions {
		if r.Code == "" || c.deny[r.Code] {
			continue
		}
		name := names[r.Code]
		if name == "" {
			name = regionDisplayName(r.Code, r.Name)
		}
		out = append(out, models.RegionInfo{Code: r.Code, Name: name, Provider: models.ProviderLightsail, Available: r.Available})
	}
	c.sort(out)
	return out, nil
}

// fromTable 兜底:旧的 fulfillment.regions 表(同样过 deny)。
func (c *RegionCatalog) fromTable(ctx context.Context) ([]models.RegionInfo, error) {
	rows, err := c.repo.GetAvailable(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]models.RegionInfo, 0, len(rows))
	for _, r := range rows {
		if c.deny[r.Code] {
			continue
		}
		out = append(out, models.RegionInfo{Code: r.Code, Name: r.Name, Provider: r.Provider, Available: r.Available})
	}
	c.sort(out)
	return out, nil
}

// tableNames 表里已有的英文名优先(老区域名字保持不变)。
func (c *RegionCatalog) tableNames(ctx context.Context) map[string]string {
	names := map[string]string{}
	if c.repo == nil {
		return names
	}
	rows, err := c.repo.GetAll(ctx)
	if err != nil {
		return names
	}
	for _, r := range rows {
		names[r.Code] = r.Name
	}
	return names
}

func (c *RegionCatalog) sort(list []models.RegionInfo) {
	rank := func(code string) int {
		if i, ok := c.order[code]; ok {
			return i
		}
		return len(c.order)
	}
	sort.SliceStable(list, func(i, j int) bool {
		ri, rj := rank(list[i].Code), rank(list[j].Code)
		if ri != rj {
			return ri < rj
		}
		return list[i].Code < list[j].Code
	})
}

// regionDisplayName 拼成与老数据同风格的完整英文名:"Asia Pacific (Tokyo)"、"US East (Virginia)"。
func regionDisplayName(code, displayName string) string {
	if displayName == "" {
		return code
	}
	prefix := ""
	switch {
	case strings.HasPrefix(code, "us-east-"):
		prefix = "US East"
	case strings.HasPrefix(code, "us-west-"):
		prefix = "US West"
	case strings.HasPrefix(code, "ap-"):
		prefix = "Asia Pacific"
	case strings.HasPrefix(code, "eu-"):
		prefix = "Europe"
	case strings.HasPrefix(code, "ca-"):
		prefix = "Canada"
	case strings.HasPrefix(code, "sa-"):
		prefix = "South America"
	case strings.HasPrefix(code, "me-"):
		prefix = "Middle East"
	case strings.HasPrefix(code, "af-"):
		prefix = "Africa"
	case strings.HasPrefix(code, "il-"):
		prefix = "Israel"
	case strings.HasPrefix(code, "mx-"):
		prefix = "Mexico"
	}
	if prefix == "" || strings.Contains(displayName, "(") {
		return displayName
	}
	return prefix + " (" + displayName + ")"
}

type regionErr string

func (e regionErr) Error() string { return string(e) }

const errNoHostingClient = regionErr("hosting client not configured")
