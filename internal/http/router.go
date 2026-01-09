package http

import (
	"github.com/gin-gonic/gin"
	"github.com/wenwu/saas-platform/fulfillment-service/internal/config"
	"github.com/wenwu/saas-platform/fulfillment-service/internal/service"
)

type Server struct {
	router  *gin.Engine
	handler *Handler
	cfg     *config.Config
}

func NewServer(cfg *config.Config, provisionService *service.ProvisionService) *Server {
	gin.SetMode(cfg.Server.Mode)
	router := gin.New()

	// Global middleware
	router.Use(gin.Recovery())
	router.Use(gin.Logger())

	handler := NewHandler(provisionService)

	s := &Server{
		router:  router,
		handler: handler,
		cfg:     cfg,
	}

	s.setupRoutes()
	return s
}

func (s *Server) setupRoutes() {
	// Health check
	s.router.GET("/health", func(c *gin.Context) {
		c.JSON(200, gin.H{
			"status":  "ok",
			"service": "fulfillment-service",
		})
	})

	// Internal API - called by subscription-service
	internal := s.router.Group("/api/internal")
	internal.Use(InternalAuthMiddleware(s.cfg.InternalSecret))
	{
		// Provisioning
		internal.POST("/provision", s.handler.Provision)
		internal.POST("/deprovision", s.handler.Deprovision)

		// Resource status queries
		internal.GET("/resources/:id", s.handler.GetResourceStatus)
		internal.GET("/subscriptions/:subscription_id/resources", s.handler.GetResourcesBySubscription)
	}

	// Node callback API - called by node-agent
	callback := s.router.Group("/api/callback")
	callback.Use(InternalAuthMiddleware(s.cfg.InternalSecret))
	{
		callback.POST("/node/ready", s.handler.NodeReady)
		callback.POST("/node/failed", s.handler.NodeFailed)
	}

	// User API - requires JWT authentication
	user := s.router.Group("/api/v1")
	user.Use(JWTAuthMiddleware(s.cfg.JWT.SecretKey))
	{
		// Node management
		user.GET("/my/node", s.handler.GetMyNode)       // 获取节点状态（含订阅信息）
		user.POST("/my/node", s.handler.CreateMyNode)   // 创建节点（检查订阅和活跃节点）
		user.DELETE("/my/node", s.handler.DeleteMyNode) // 删除节点

		// Regions
		user.GET("/regions", s.handler.GetRegions)
	}

	// Public API - no authentication required
	public := s.router.Group("/api/v1/public")
	{
		public.GET("/regions", s.handler.GetRegions)
	}
}

func (s *Server) Run(addr string) error {
	return s.router.Run(addr)
}
