package service

import (
	"context"
	"log"
	"time"

	"github.com/wenwu/saas-platform/fulfillment-service/internal/client"
	"github.com/wenwu/saas-platform/fulfillment-service/internal/repository"
)

// CleanupScheduler 后台兜底清理任务
// 定时扫描需要清理的失败 provision 和孤立云实例，防止资源泄漏
type CleanupScheduler struct {
	hostingRepo   *repository.HostingProvisionRepository
	hostingClient *client.HostingClient
	interval      time.Duration
	failedNodeAge time.Duration // 失败节点清理阈值（创建超过多久才清理）
}

// NewCleanupScheduler 创建清理调度器
func NewCleanupScheduler(
	hostingRepo *repository.HostingProvisionRepository,
	hostingClient *client.HostingClient,
	interval time.Duration,
	failedNodeAge time.Duration,
) *CleanupScheduler {
	return &CleanupScheduler{
		hostingRepo:   hostingRepo,
		hostingClient: hostingClient,
		interval:      interval,
		failedNodeAge: failedNodeAge,
	}
}

// Start 启动清理调度器（阻塞运行，应在 goroutine 中调用）
func (s *CleanupScheduler) Start(ctx context.Context) {
	log.Printf("[CleanupScheduler] Started (interval=%v, failed_node_age=%v)", s.interval, s.failedNodeAge)

	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Println("[CleanupScheduler] Stopped")
			return
		case <-ticker.C:
			s.runCleanupCycle(ctx)
		}
	}
}

// runCleanupCycle 执行一轮清理
func (s *CleanupScheduler) runCleanupCycle(ctx context.Context) {
	s.cleanupFailedProvisions(ctx)
	s.cleanupOrphanedNodes(ctx)
	s.cleanupOrphanedActiveNodes(ctx)
}

// cleanupFailedProvisions 清理标记了 needs_cleanup 的失败 provision
// 这些是 provisionAsync 中 DeleteNode 失败后留下的记录
func (s *CleanupScheduler) cleanupFailedProvisions(ctx context.Context) {
	provisions, err := s.hostingRepo.ListNeedsCleanup(ctx, 20)
	if err != nil {
		log.Printf("[CleanupScheduler] Failed to list needs_cleanup provisions: %v", err)
		return
	}

	if len(provisions) == 0 {
		return
	}

	log.Printf("[CleanupScheduler] Found %d provisions needing cleanup", len(provisions))

	for _, p := range provisions {
		if p.HostingNodeID == "" {
			// 没有 hosting_node_id，无需清理云资源，直接清除标记
			if err := s.hostingRepo.ClearCleanupFlag(ctx, p.ID); err != nil {
				log.Printf("[CleanupScheduler] Failed to clear flag for %s: %v", p.ID, err)
			}
			continue
		}

		if _, err := s.hostingClient.DeleteNode(ctx, p.HostingNodeID); err != nil {
			log.Printf("[CleanupScheduler] Failed to delete orphaned node %s (provision=%s): %v", p.HostingNodeID, p.ID, err)
			continue
		}

		if err := s.hostingRepo.ClearCleanupFlag(ctx, p.ID); err != nil {
			log.Printf("[CleanupScheduler] Failed to clear flag for %s: %v", p.ID, err)
		}

		log.Printf("[CleanupScheduler] Cleaned up orphaned node %s (provision=%s)", p.HostingNodeID, p.ID)
	}
}

// cleanupOrphanedActiveNodes 核心对账逻辑：
// 每个活跃 VPS 必须有一个活跃的 hosting_provision 对应。
// 找不到对应 provision 的活跃 VPS → 孤立节点 → 自动删除。
//
// 这防止了以下场景导致的资源泄漏：
// - 订阅过期/取消但 deprovision 事件丢失
// - deprovision 执行失败且重试也失败
// - 任何导致 VPS 和订阅不一致的 bug
func (s *CleanupScheduler) cleanupOrphanedActiveNodes(ctx context.Context) {
	nodes, err := s.hostingClient.ListActiveOBoxNodes(ctx)
	if err != nil {
		log.Printf("[CleanupScheduler] Failed to list active nodes from hosting-service: %v", err)
		return
	}

	if len(nodes) == 0 {
		return
	}

	orphanCount := 0
	for _, node := range nodes {
		provision, _ := s.hostingRepo.GetByHostingNodeID(ctx, node.NodeID)
		if provision != nil && (provision.Status == "active" || provision.Status == "creating" || provision.Status == "pending" || provision.Status == "running" || provision.Status == "installing") {
			continue // 有活跃的 provision 对应，正常
		}

		// 孤立节点：VPS 在运行但没有活跃的 provision
		orphanCount++
		log.Printf("[CleanupScheduler] ORPHAN DETECTED: node %s (status=active) has no active provision, deleting...", node.NodeID)

		if _, err := s.hostingClient.DeleteNode(ctx, node.NodeID); err != nil {
			log.Printf("[CleanupScheduler] Failed to delete orphaned active node %s: %v", node.NodeID, err)
		} else {
			log.Printf("[CleanupScheduler] Deleted orphaned active node %s", node.NodeID)
		}
	}

	if orphanCount > 0 {
		log.Printf("[CleanupScheduler] Cleaned up %d orphaned active nodes out of %d total active nodes", orphanCount, len(nodes))
	}
}

// cleanupOrphanedNodes 交叉校验：从 hosting-service 获取失败节点，
// 检查 fulfillment 中是否有对应的活跃 provision，若无则删除
func (s *CleanupScheduler) cleanupOrphanedNodes(ctx context.Context) {
	nodes, err := s.hostingClient.ListFailedNodes(ctx, s.failedNodeAge)
	if err != nil {
		log.Printf("[CleanupScheduler] Failed to list failed nodes from hosting-service: %v", err)
		return
	}

	if len(nodes) == 0 {
		return
	}

	log.Printf("[CleanupScheduler] Found %d failed nodes older than %v in hosting-service", len(nodes), s.failedNodeAge)

	for _, node := range nodes {
		// 检查 fulfillment 中是否有活跃的 provision 引用此节点
		provision, _ := s.hostingRepo.GetByHostingNodeID(ctx, node.NodeID)
		if provision != nil && provision.Status == "active" {
			continue // 有活跃 provision，跳过
		}

		log.Printf("[CleanupScheduler] Deleting orphaned node %s (status=%s, no active provision)", node.NodeID, node.Status)
		if _, err := s.hostingClient.DeleteNode(ctx, node.NodeID); err != nil {
			log.Printf("[CleanupScheduler] Failed to delete orphaned node %s: %v", node.NodeID, err)
		}
	}
}
