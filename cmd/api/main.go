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

	// Initialize services
	provisionService := service.NewProvisionService(
		cfg,
		hostingRepo,
		regionRepo,
		logRepo,
		hostingClient,
		subscriptionClient,
	)

	vpnService := service.NewVPNService(
		cfg,
		vpnRepo,
		logRepo,
		otunClient,
		subscriptionClient,
	)

	entitlementService := service.NewEntitlementService(
		cfg,
		vpnRepo,
		otunClient,
	)

	// Initialize HTTP server
	server := http.NewServer(cfg, pool, provisionService, vpnService, entitlementService)

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

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_ = ctx // Used for graceful shutdown if needed

	log.Println("Server exited")
}
