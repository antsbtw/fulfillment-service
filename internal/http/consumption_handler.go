package http

import (
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/wenwu/saas-platform/fulfillment-service/internal/service"
)

// SetConsumptionService 接线 Apple 退款申请回报用的使用量查询(nil = 接口返回 503)。
func (s *Server) SetConsumptionService(cs *service.ConsumptionService) {
	s.handler.consumption = cs
}

// GetConsumption GET /api/internal/consumption
// ?user_id=&app=&service_type=&plan_tier=&purchase_at=<RFC3339>&period_end=<RFC3339>&traffic_gb=
// 某一笔 Apple 购买交付了没有、用了多少(payment-service APPLE_CONSUMPTION_RESPONSE_DESIGN §4.2)。
func (h *Handler) GetConsumption(c *gin.Context) {
	if h.consumption == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "consumption not configured"})
		return
	}
	q := service.ConsumptionQuery{
		UserID:      c.Query("user_id"),
		App:         c.Query("app"),
		ServiceType: c.Query("service_type"),
		PlanTier:    c.Query("plan_tier"),
	}
	var err error
	if q.PurchaseAt, err = time.Parse(time.RFC3339, c.Query("purchase_at")); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "purchase_at must be RFC3339"})
		return
	}
	if v := c.Query("period_end"); v != "" {
		if q.PeriodEnd, err = time.Parse(time.RFC3339, v); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "period_end must be RFC3339"})
			return
		}
	}
	if v := c.Query("traffic_gb"); v != "" {
		if q.TrafficGB, err = strconv.Atoi(v); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "traffic_gb must be int"})
			return
		}
	}
	resp, err := h.consumption.Get(c.Request.Context(), q)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, resp)
}
