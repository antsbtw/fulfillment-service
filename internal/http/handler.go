package http

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/wenwu/saas-platform/fulfillment-service/internal/models"
	"github.com/wenwu/saas-platform/fulfillment-service/internal/service"
)

type Handler struct {
	provisionService   *service.ProvisionService
	vpnService         *service.VPNService
	entitlementService *service.EntitlementService
}

func NewHandler(provisionService *service.ProvisionService, vpnService *service.VPNService, entitlementService *service.EntitlementService) *Handler {
	return &Handler{
		provisionService:   provisionService,
		vpnService:         vpnService,
		entitlementService: entitlementService,
	}
}

// ==================== Internal API Handlers ====================

// Provision handles resource provisioning requests from subscription-service
func (h *Handler) Provision(c *gin.Context) {
	var req models.ProvisionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	var resp *models.ProvisionResponse
	var err error

	// Route based on app_source (new) or resource_type (legacy)
	switch {
	case req.AppSource == "otun" || req.ResourceType == models.ResourceTypeVPNUser:
		resp, err = h.vpnService.ProvisionVPNUser(c.Request.Context(), &req)
	default:
		// obox or hosting_node
		resp, err = h.provisionService.Provision(c.Request.Context(), &req)
	}

	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, resp)
}

// Deprovision handles resource deprovisioning requests
func (h *Handler) Deprovision(c *gin.Context) {
	var req models.DeprovisionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	resp, err := h.provisionService.Deprovision(c.Request.Context(), &req)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, resp)
}

// GetResourceStatus gets resource status by ID
func (h *Handler) GetResourceStatus(c *gin.Context) {
	resourceID := c.Param("id")
	if resourceID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "resource id required"})
		return
	}

	resp, err := h.provisionService.GetResourceStatus(c.Request.Context(), resourceID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "resource not found"})
		return
	}

	c.JSON(http.StatusOK, resp)
}

// GetResourcesBySubscription gets resources for a subscription
func (h *Handler) GetResourcesBySubscription(c *gin.Context) {
	subscriptionID := c.Param("subscription_id")
	if subscriptionID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "subscription id required"})
		return
	}

	resp, err := h.provisionService.GetResourcesBySubscription(c.Request.Context(), subscriptionID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"resources": resp})
}

// GetUserResources gets all resources for a user (internal API, called by user-portal)
func (h *Handler) GetUserResources(c *gin.Context) {
	userID := c.Param("user_id")
	if userID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "user_id required"})
		return
	}

	resp, err := h.provisionService.GetUserNodeStatus(c.Request.Context(), userID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"success": true, "data": resp})
}

// ==================== Node Callback Handlers ====================

// NodeReady handles callback when node software is ready
func (h *Handler) NodeReady(c *gin.Context) {
	var req models.NodeReadyCallback
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if err := h.provisionService.HandleNodeReady(c.Request.Context(), &req); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

// NodeFailed handles callback when node installation fails
func (h *Handler) NodeFailed(c *gin.Context) {
	var req models.NodeFailedCallback
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if err := h.provisionService.HandleNodeFailed(c.Request.Context(), &req); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

// ==================== User API Handlers ====================

// GetMyNode gets the current user's node status
func (h *Handler) GetMyNode(c *gin.Context) {
	userID, exists := c.Get("userID")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "user not authenticated"})
		return
	}

	resp, err := h.provisionService.GetUserNodeStatus(c.Request.Context(), userID.(string))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, resp)
}

// CreateMyNode creates a new node for the current user
func (h *Handler) CreateMyNode(c *gin.Context) {
	userID, exists := c.Get("userID")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "user not authenticated"})
		return
	}

	var req models.CreateNodeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	resp, err := h.provisionService.CreateUserNode(c.Request.Context(), userID.(string), req.Region)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	if !resp.Success {
		c.JSON(http.StatusBadRequest, resp)
		return
	}

	c.JSON(http.StatusOK, resp)
}

// DeleteMyNode deletes the current user's node
func (h *Handler) DeleteMyNode(c *gin.Context) {
	userID, exists := c.Get("userID")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "user not authenticated"})
		return
	}

	resp, err := h.provisionService.DeleteUserNode(c.Request.Context(), userID.(string))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	if !resp.Success {
		c.JSON(http.StatusNotFound, resp)
		return
	}

	c.JSON(http.StatusOK, resp)
}

// GetRegions returns available regions
func (h *Handler) GetRegions(c *gin.Context) {
	resp, err := h.provisionService.GetAvailableRegions(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, resp)
}

// ==================== VPN API Handlers ====================

// GetMyVPN gets the current user's VPN status
func (h *Handler) GetMyVPN(c *gin.Context) {
	userID, exists := c.Get("userID")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "user not authenticated"})
		return
	}

	resp, err := h.vpnService.GetUserVPNStatus(c.Request.Context(), userID.(string))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, resp)
}

// GetMyVPNSubscribe gets the VPN subscription configuration for the current user
func (h *Handler) GetMyVPNSubscribe(c *gin.Context) {
	userID, exists := c.Get("userID")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "user not authenticated"})
		return
	}

	resp, err := h.vpnService.GetUserVPNSubscribeConfig(c.Request.Context(), userID.(string))
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, resp)
}

// GetUserVPNSubscribe gets VPN subscription config for a user (internal API, called by user-portal)
func (h *Handler) GetUserVPNSubscribe(c *gin.Context) {
	userID := c.Param("user_id")
	if userID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "user_id required"})
		return
	}

	resp, err := h.vpnService.GetUserVPNSubscribeConfig(c.Request.Context(), userID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"success": false, "error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"success": true, "data": resp})
}

// GetUserVPNQuickStatus gets lightweight VPN status for a user (internal API, called by user-portal)
func (h *Handler) GetUserVPNQuickStatus(c *gin.Context) {
	userID := c.Param("user_id")
	if userID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "user_id required"})
		return
	}

	resp, err := h.vpnService.GetUserVPNQuickStatus(c.Request.Context(), userID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"success": false, "error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"success": true, "data": resp})
}

// UpdateVPNResource updates a VPN resource (extend/upgrade)
func (h *Handler) UpdateVPNResource(c *gin.Context) {
	resourceID := c.Param("id")
	if resourceID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "resource id required"})
		return
	}

	var req models.UpdateVPNUserRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if err := h.vpnService.UpdateVPNUser(c.Request.Context(), resourceID, &req); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"status": "ok", "message": "VPN user updated successfully"})
}

// ==================== Trial & Entitlement Handlers ====================

// GetTrialConfig returns trial configuration (public, no auth)
func (h *Handler) GetTrialConfig(c *gin.Context) {
	resp := h.entitlementService.GetTrialConfig()
	c.JSON(http.StatusOK, resp)
}

// GiftEntitlement creates a gift entitlement (admin/internal)
func (h *Handler) GiftEntitlement(c *gin.Context) {
	var req models.GiftEntitlementRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	resp, err := h.entitlementService.GiftEntitlement(c.Request.Context(), &req)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusCreated, resp)
}

// ListEntitlements queries entitlements (admin/internal)
func (h *Handler) ListEntitlements(c *gin.Context) {
	userID := c.Query("user_id")
	businessType := c.Query("business_type")
	// Keep backward-compatible: also accept "source" query param
	if businessType == "" {
		businessType = c.Query("source")
	}
	status := c.Query("status")

	resp, err := h.entitlementService.ListEntitlements(c.Request.Context(), userID, businessType, status)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"entitlements": resp})
}
