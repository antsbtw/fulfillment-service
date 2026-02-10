package http

import (
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/wenwu/saas-platform/fulfillment-service/internal/config"
	"github.com/wenwu/saas-platform/fulfillment-service/internal/service"
)

// RateLimiter 简单的内存速率限制器
type RateLimiter struct {
	mu       sync.Mutex
	requests map[string][]time.Time
	limit    int           // 最大请求数
	window   time.Duration // 时间窗口
}

// NewRateLimiter 创建速率限制器
func NewRateLimiter(limit int, window time.Duration) *RateLimiter {
	return &RateLimiter{
		requests: make(map[string][]time.Time),
		limit:    limit,
		window:   window,
	}
}

// Allow 检查是否允许请求
func (rl *RateLimiter) Allow(key string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := time.Now()
	windowStart := now.Add(-rl.window)

	// 清理过期请求
	var valid []time.Time
	for _, t := range rl.requests[key] {
		if t.After(windowStart) {
			valid = append(valid, t)
		}
	}

	// 检查是否超过限制
	if len(valid) >= rl.limit {
		rl.requests[key] = valid
		return false
	}

	// 记录新请求
	rl.requests[key] = append(valid, now)
	return true
}

// RateLimitMiddleware 速率限制中间件
func RateLimitMiddleware(rl *RateLimiter) gin.HandlerFunc {
	return func(c *gin.Context) {
		// 使用用户 ID 或 IP 作为限制 key
		key := c.GetString("userID")
		if key == "" {
			key = c.ClientIP()
		}

		if !rl.Allow(key) {
			c.JSON(http.StatusTooManyRequests, gin.H{
				"error": "rate limit exceeded, please try again later",
			})
			c.Abort()
			return
		}

		c.Next()
	}
}

type Server struct {
	router  *gin.Engine
	handler *Handler
	cfg     *config.Config
}

// 全局速率限制器: 每用户每分钟最多 30 次请求
var userRateLimiter = NewRateLimiter(30, time.Minute)

// 资源创建速率限制器: 每用户每小时最多 5 次创建请求
// 说明: 业务规则限制每用户只能有一个托管节点，5 次足够处理重试和重建场景
var createRateLimiter = NewRateLimiter(5, time.Hour)

func NewServer(cfg *config.Config, provisionService *service.ProvisionService, vpnService *service.VPNService) *Server {
	gin.SetMode(cfg.Server.Mode)
	router := gin.New()

	// Global middleware
	router.Use(gin.Recovery())
	router.Use(gin.Logger())

	handler := NewHandler(provisionService, vpnService)

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

		// User resource queries (called by user-portal)
		internal.GET("/users/:user_id/resources", s.handler.GetUserResources)

		// VPN resource update (extend/upgrade)
		internal.PUT("/resources/:id/vpn", s.handler.UpdateVPNResource)
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
	user.Use(RateLimitMiddleware(userRateLimiter)) // 用户 API 速率限制
	{
		// Hosting Node management
		user.GET("/my/node", s.handler.GetMyNode) // 获取节点状态（含订阅信息）
		// 创建节点使用更严格的速率限制
		user.POST("/my/node", RateLimitMiddleware(createRateLimiter), s.handler.CreateMyNode)
		user.DELETE("/my/node", s.handler.DeleteMyNode) // 删除节点

		// VPN management
		user.GET("/my/vpn", s.handler.GetMyVPN)                    // 获取 VPN 状态
		user.GET("/my/vpn/subscribe", s.handler.GetMyVPNSubscribe) // 获取 VPN 订阅配置

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
