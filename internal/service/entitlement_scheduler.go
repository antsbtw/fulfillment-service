package service

import (
	"context"
	"log"
	"time"
)

// EntitlementScheduler 时间驱动的生效切换（变更清单 §2.5）：
// 每 interval 扫一遍"生效 profile 将在 now+lead 前到期 且 该面有 waiting 订购桶"的 (user, face)，
// 逐个 Resolve+Sync（幂等）。lead 必须 ≥ otun-manager UserCleanupService 间隔（1h，
// otun-manager/internal/service/user_cleanup_service.go:30），保证先把 otun expire 推后再到期，
// 不给 cleanup 留下把账号 disable 的窗口（Sync 内桥接推送 + 显式 enabled=true 双保险）。
// 读请求（/vpn/all、/status）读前也会调 Resolve+Sync 兜底调度器漏拍。
type EntitlementScheduler struct {
	svc      *EntitlementProfileService
	interval time.Duration
	lead     time.Duration
	batch    int
}

// NewEntitlementScheduler 仿 CleanupScheduler。
func NewEntitlementScheduler(svc *EntitlementProfileService, interval, lead time.Duration) *EntitlementScheduler {
	return &EntitlementScheduler{svc: svc, interval: interval, lead: lead, batch: 200}
}

// Start 阻塞运行，应在 goroutine 中调用；ctx 取消即停。
func (s *EntitlementScheduler) Start(ctx context.Context) {
	log.Printf("[EntitlementScheduler] Started (interval=%v, lead=%v)", s.interval, s.lead)
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Println("[EntitlementScheduler] Stopped")
			return
		case <-ticker.C:
			s.runOnce(ctx)
		}
	}
}

// runOnce 一轮扫描。
func (s *EntitlementScheduler) runOnce(ctx context.Context) {
	if s.svc == nil {
		return
	}
	horizon := s.svc.now().Add(s.lead)
	faces, err := s.svc.store.ListFacesDueForResolve(ctx, horizon, s.batch)
	if err != nil {
		log.Printf("[EntitlementScheduler] list due faces failed: %v", err)
		return
	}
	if len(faces) == 0 {
		return
	}
	log.Printf("[EntitlementScheduler] %d face(s) due for resolve (horizon=%s)", len(faces), horizon.Format(time.RFC3339))
	for _, uf := range faces {
		if _, err := s.svc.Sync(ctx, uf[0], uf[1]); err != nil {
			log.Printf("[EntitlementScheduler] sync user=%s face=%s failed: %v", uf[0], uf[1], err)
		}
	}
	if len(faces) >= s.batch {
		log.Printf("[EntitlementScheduler] batch cap %d reached; remaining faces will be picked up next tick", s.batch)
	}
}
