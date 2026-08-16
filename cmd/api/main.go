package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/wenwu/saas-platform/fulfillment-service/internal/client"
	"github.com/wenwu/saas-platform/fulfillment-service/internal/config"
	"github.com/wenwu/saas-platform/fulfillment-service/internal/db"
	"github.com/wenwu/saas-platform/fulfillment-service/internal/http"
	"github.com/wenwu/saas-platform/fulfillment-service/internal/repository"
	"github.com/wenwu/saas-platform/fulfillment-service/internal/service"
)

func main() {
	log.Println("Starting Fulfillment Service...")

	// Load configuration
	cfg := config.Load()

	// SECURITY (2026-07-12): validate secret strength. Validate() was defined but
	// never called; an empty/default INTERNAL_SECRET makes ConstantTimeCompare("","")==1,
	// opening internal endpoints (provision/deprovision) to unauthenticated callers.
	// Fail closed by refusing to boot.
	if err := cfg.Validate(); err != nil {
		log.Fatalf("Insecure configuration: %v", err)
	}

	// Initialize database
	pool, err := db.NewPool(cfg.Database.DSN())
	if err != nil {
		log.Fatalf("Failed to connect to database: %v", err)
	}
	defer pool.Close()

	// Initialize repositories
	hostingRepo := repository.NewHostingProvisionRepository(pool)
	vpnRepo := repository.NewVPNProvisionRepository(pool)
	regionRepo := repository.NewRegionRepository(pool)
	logRepo := repository.NewLogRepository(pool)

	// Initialize clients
	hostingClient := client.NewHostingClient(
		cfg.Hosting.ServiceURL,
		cfg.Hosting.AdminKey,
	)

	subscriptionClient := client.NewSubscriptionClient(
		cfg.Services.SubscriptionServiceURL,
		cfg.InternalSecret,
	)

	otunClient := client.NewOTunClient(cfg.Services.OTunManagerURL, cfg.InternalSecret)
	oboxClient := client.NewOBoxClient(cfg.Services.OBoxManagerURL, cfg.InternalSecret)

	// Initialize services
	provisionService := service.NewProvisionService(
		cfg,
		hostingRepo,
		regionRepo,
		logRepo,
		hostingClient,
		subscriptionClient,
		oboxClient,
	)

	// 订阅/订购 profile 记账层（document/subscription-entitlement/*）。开关 false 只影子写。
	entitlementProfileRepo := repository.NewEntitlementProfileRepository(pool)
	entitlementProfiles := service.NewEntitlementProfileService(
		entitlementProfileRepo,
		vpnRepo,
		otunClient,
		cfg.Entitlement.Enabled,
		cfg.Entitlement.SwitchLead,
	)

	// 第三产品面 campaign 的入账台账（迁移 010 campaign_grants；document/marketing-campaign/*）
	campaignGrantRepo := repository.NewCampaignGrantRepository(pool)

	vpnService := service.NewVPNService(
		cfg,
		vpnRepo,
		logRepo,
		otunClient,
		subscriptionClient,
		entitlementProfiles,
		campaignGrantRepo,
	)

	entitlementService := service.NewEntitlementService(
		cfg,
		vpnRepo,
		otunClient,
	)

	// Initialize CleanupScheduler (后台兜底清理失败的 VPS 实例)
	cleanupScheduler := service.NewCleanupScheduler(
		hostingRepo,
		hostingClient,
		1*time.Hour,  // 每小时运行一次
		24*time.Hour, // 清理创建超过 24 小时的失败节点
	)

	// Start CleanupScheduler in background
	cleanupCtx, cleanupCancel := context.WithCancel(context.Background())
	go cleanupScheduler.Start(cleanupCtx)

	// EntitlementScheduler：时间驱动的订阅→订购接续（仅开关 true 启动；1 min 一轮，
	// 提前量 = ENTITLEMENT_SWITCH_LEAD_MINUTES ≥ otun-manager cleanup 间隔 1h）
	if cfg.Entitlement.Enabled {
		entitlementScheduler := service.NewEntitlementScheduler(entitlementProfiles, 1*time.Minute, cfg.Entitlement.SwitchLead)
		go entitlementScheduler.Start(cleanupCtx)
	}

	// CampaignCleanupScheduler：活动账号到期 + 保留期（默认 7 天）后 is_current=false + otun disable（1h 一轮）
	campaignCleanup := service.NewCampaignCleanupScheduler(vpnService, 1*time.Hour,
		time.Duration(cfg.Campaign.RetentionDays)*24*time.Hour)
	go campaignCleanup.Start(cleanupCtx)

	// Initialize HTTP server
	server := http.NewServer(cfg, pool, provisionService, vpnService, entitlementService, entitlementProfiles)

	// Start server in goroutine
	go func() {
		addr := fmt.Sprintf(":%s", cfg.Server.Port)
		log.Printf("Server starting on %s", addr)
		if err := server.Run(addr); err != nil {
			log.Fatalf("Server failed: %v", err)
		}
	}()

	// Graceful shutdown
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	log.Println("Shutting down server...")
	cleanupCancel() // 停止 CleanupScheduler

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_ = ctx // Used for graceful shutdown if needed

	log.Println("Server exited")
}
