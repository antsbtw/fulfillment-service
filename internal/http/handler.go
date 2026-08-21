package http

import (
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/wenwu/saas-platform/fulfillment-service/internal/client"
	"github.com/wenwu/saas-platform/fulfillment-service/internal/models"
	"github.com/wenwu/saas-platform/fulfillment-service/internal/repository"
	"github.com/wenwu/saas-platform/fulfillment-service/internal/service"
)

type Handler struct {
	provisionService    *service.ProvisionService
	vpnService          *service.VPNService
	entitlementService  *service.EntitlementService
	entitlementProfiles *service.EntitlementProfileService
}

func NewHandler(provisionService *service.ProvisionService, vpnService *service.VPNService, entitlementService *service.EntitlementService, entitlementProfiles *service.EntitlementProfileService) *Handler {
	return &Handler{
		provisionService:    provisionService,
		vpnService:          vpnService,
		entitlementService:  entitlementService,
		entitlementProfiles: entitlementProfiles,
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

// DeprovisionVPNByUser 按 user_id 回收某用户当前的 VPN 用户（订阅换绑时由 subscription-service 调用）。
// 用户无可回收的 VPN 用户时返回 404（调用方视为幂等成功）。
func (h *Handler) DeprovisionVPNByUser(c *gin.Context) {
	userID := c.Param("user_id")
	if userID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "user_id required"})
		return
	}

	// ★P0：换绑只回收该订阅所在的服务面。调用方带 plan_tier（缺省按事件 plan_tier 推导）或
	// 显式 is_residential；两者都缺（老调用方）时退回不分区旧行为。
	var req struct {
		Reason        string `json:"reason"`
		PlanTier      string `json:"plan_tier"`
		IsResidential *bool  `json:"is_residential"`
	}
	_ = c.ShouldBindJSON(&req)
	if req.Reason == "" {
		req.Reason = "subscription reassigned to another account"
	}
	// ★验收 F3：plan_tier=campaign 走 MapPlanToServiceTier 会折成 standard 命中 basic 行（串面）。
	// 活动账号不参与换绑/回收（有自己的 revoke 口），按产品面判定：campaign → 400 拒绝。
	isResidential, reject := deprovisionPartitionFor(req.PlanTier, req.IsResidential)
	if reject {
		c.JSON(http.StatusBadRequest, gin.H{"error": "campaign face is not deprovisionable via reassign; use /api/internal/vpn/campaign/revoke"})
		return
	}

	if err := h.vpnService.DeprovisionVPNByUser(c.Request.Context(), userID, req.Reason, isResidential); err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "no current VPN user for this user"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"status": "deprovisioned", "user_id": userID})
}

// deprovisionPartitionFor 把换绑回收请求折成分区参数：显式 is_residential 优先；否则按 plan_tier 的
// 【产品面】（MapPlanToProductFace）判定——residential → true，basic/premium/unlimited → false，
// campaign → reject（活动账号不在换绑语义内，且按 service_tier 折算会串到 basic 面）。两者都缺 → nil（旧行为）。
func deprovisionPartitionFor(planTier string, explicit *bool) (isResidential *bool, reject bool) {
	if planTier != "" && models.MapPlanToProductFace(planTier) == models.ProductFaceCampaign {
		return nil, true
	}
	if explicit != nil {
		return explicit, false
	}
	if planTier == "" {
		return nil, false
	}
	v := models.MapPlanToProductFace(planTier) == models.ProductFaceResidential
	return &v, false
}

func (h *Handler) DeprovisionOBox(c *gin.Context) {
	var req models.OBoxDeprovisionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	resp, err := h.provisionService.DeprovisionOBox(c.Request.Context(), &req)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusAccepted, resp)
}

func (h *Handler) SuspendOBox(c *gin.Context) {
	var req models.OBoxSuspendRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	resp, err := h.provisionService.SuspendOBox(c.Request.Context(), &req)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusAccepted, resp)
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
		respondVPNConfigError(c, err)
		return
	}

	c.JSON(http.StatusOK, gin.H{"success": true, "data": resp})
}

// respondVPNConfigError 区分"无有效订阅"业务态与其他失败:
//   - 订阅过期/缺失(含 otun-manager 403 subscription_expired) → 403 + code=SUBSCRIPTION_EXPIRED,
//     BFF 原样透传后 App 可引导续费,而不是当成未知 500(实测会拿空配置强启内核报 kernel_fatal);
//   - 其余(provision 缺失/下游抖动) → 维持原 404 语义不动。
func respondVPNConfigError(c *gin.Context, err error) {
	if errors.Is(err, service.ErrNoActiveSubscription) || strings.Contains(err.Error(), "subscription_expired") {
		c.JSON(http.StatusForbidden, gin.H{
			"success": false,
			"code":    "SUBSCRIPTION_EXPIRED",
			"error":   "no active subscription: expired or never subscribed",
		})
		return
	}
	c.JSON(http.StatusNotFound, gin.H{"success": false, "error": err.Error()})
}

// GetUserVPNConfigVersion 返回用户 VPN 配置的 config_version（§8.3 轻量版本端点，internal API）。
// 前端轻量拉此值判断是否需拉全量 + 静默重连。零凭证（只出一个 hash 串）。
func (h *Handler) GetUserVPNConfigVersion(c *gin.Context) {
	userID := c.Param("user_id")
	if userID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "user_id required"})
		return
	}

	// ★第三产品面门控（契约 C1/C6）：BFF 透传的 X-Client-Capabilities；无头 = 老客户端，值与改动前逐字节一致。
	caps := service.ParseClientCapabilities(c.GetHeader("X-Client-Capabilities"))
	version, err := h.vpnService.GetUserConfigVersionWithCaps(c.Request.Context(), userID, caps)
	if err != nil {
		respondVPNConfigError(c, err)
		return
	}

	c.JSON(http.StatusOK, gin.H{"success": true, "data": gin.H{"config_version": version}})
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

// GetUserVPNSubscribeAll 一次返回该用户所有服务面（标准 + 住宅）的订阅配置（方案 C，internal API）。
// 与单条接口不同：未持有/无有效订阅时返回 200 + 空数组（非 404），由前端按 service_tier 渲染。
func (h *Handler) GetUserVPNSubscribeAll(c *gin.Context) {
	userID := c.Param("user_id")
	if userID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "user_id required"})
		return
	}

	// ★第三产品面门控（契约 C1）：只有请求头 X-Client-Capabilities 含 campaign-profile 才追加 campaign 元素；
	// 无头 → 与改动前逐字节一致（golden 锁定）。头由 user-portal BFF 原样透传，本服务不按 UA/版本猜。
	caps := service.ParseClientCapabilities(c.GetHeader("X-Client-Capabilities"))
	resp, err := h.vpnService.GetUserVPNSubscribeConfigAllWithCaps(c.Request.Context(), userID, caps)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"success": true, "data": resp})
}

// ==================== 第三产品面 campaign（internal API）====================

// RevokeCampaign 从活动账号扣减 days/traffic（下限 0），同步 otun；只动活动账号。
// POST /api/internal/vpn/campaign/revoke（subscription-service 收到 subscription.revoked 后调用）。
func (h *Handler) RevokeCampaign(c *gin.Context) {
	var req service.CampaignRevokeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": err.Error()})
		return
	}
	res, err := h.vpnService.RevokeCampaign(c.Request.Context(), &req)
	if err != nil {
		if errors.Is(err, service.ErrNoCampaignProfile) {
			c.JSON(http.StatusNotFound, gin.H{"success": false, "error": "no campaign profile"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": res})
}

// GetCampaignProfile 活动账号现状 + grant 聚合（campaign-service 叠加闸 / me / preview 读口）。
// GET /api/internal/vpn/campaign/user/:user_id
func (h *Handler) GetCampaignProfile(c *gin.Context) {
	userID := c.Param("user_id")
	if userID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": "user_id required"})
		return
	}
	view, err := h.vpnService.GetCampaignProfile(c.Request.Context(), userID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": view})
}

// GetUserVPNQuickStatusAll 一次返回该用户所有服务面的轻量状态（方案 C，internal API）。空时 200 + 空数组。
func (h *Handler) GetUserVPNQuickStatusAll(c *gin.Context) {
	userID := c.Param("user_id")
	if userID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "user_id required"})
		return
	}

	resp, err := h.vpnService.GetUserVPNQuickStatusAll(c.Request.Context(), userID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"success": true, "data": resp})
}

// ==================== Realm 区域选择/切换（internal API，user-portal BFF 调用）====================
//
// 这三个 handler 是 BFF realm 链路的下游。约定的对外形态（与 user-portal BFF 一致，§Q4/Q7）：
//   成功： {"success":true,"data":{...}}（regions: data.egresses；region: data.ok/data.connect_url）
//   业务级失败：{"success":false,"data":{"ok":false,"error":"<code>","retry_after_sec":N}} + 原始 HTTP 429/409/404
//   系统级失败：{"success":false,"error":"..."} + 5xx
// BFF 只需 user_id→注入、原样透传本服务的状态码与 body。

// GetUserRealmRegions 列出用户可选出口（internal，对应 BFF GET /resources/vpn/regions）。
func (h *Handler) GetUserRealmRegions(c *gin.Context) {
	userID := c.Param("user_id")
	if userID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": "user_id required"})
		return
	}

	resp, err := h.vpnService.ListRealmEgresses(c.Request.Context(), userID, realmFaceFromQuery(c))
	if err != nil {
		// 未订阅 residential / 未开通 → no_assignment（404），前端据此隐藏入口（a+b 兜底）。
		if errors.Is(err, service.ErrNoRealmAssignment) {
			c.JSON(http.StatusNotFound, gin.H{
				"success": false,
				"data":    gin.H{"ok": false, "error": "no_assignment"},
			})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"success": true, "data": resp})
}

// GetUserRealmRegionStatus 查切换目标出口是否已就绪（internal，对应 BFF
// GET /resources/vpn/region/status，切换确认握手 P4）。?egress_id= 可空（缺省=当前 assignment）。
// App 在 select 返回 ready:false 后以 ~1s 轮询本端点，ready:true 才启动连接。
func (h *Handler) GetUserRealmRegionStatus(c *gin.Context) {
	userID := c.Param("user_id")
	if userID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": "user_id required"})
		return
	}

	resp, err := h.vpnService.GetRealmSwitchReady(c.Request.Context(), userID, c.Query("egress_id"), realmFaceFromQuery(c))
	if err != nil {
		if errors.Is(err, service.ErrNoRealmAssignment) {
			c.JSON(http.StatusNotFound, gin.H{
				"success": false,
				"data":    gin.H{"ok": false, "error": "no_assignment"},
			})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"success": true, "data": resp})
}

// SelectUserRealmRegion 切换用户当前出口（internal，对应 BFF POST /resources/vpn/region）。
// 请求体 {"egress_id":"..."}；user_id 来自路径（BFF 从 JWT 注入）。
// realmFaceFromQuery 从 query 解析 realm 系列接口的【产品面】入参（契约 v0.6 分键二元组）。
//
// ★2026-08-21 P2：缺省（两参数都不传）= 付费住宅面，行为与修复前逐字节一致，
// 老客户端与所有付费面调用方零改动。活动面须显式传 plan_tier=promo&service_tier=residential。
// realmFaceFromBody 取 body 里的面参数，任一为空则回落到 query（两种传法都支持）。
func realmFaceFromBody(c *gin.Context, planTier, serviceTier string) service.RealmFace {
	f := realmFaceFromQuery(c)
	if planTier != "" {
		f.PlanTier = planTier
	}
	if serviceTier != "" {
		f.ServiceTier = serviceTier
	}
	return f
}

func realmFaceFromQuery(c *gin.Context) service.RealmFace {
	return service.RealmFace{
		PlanTier:    c.Query("plan_tier"),
		ServiceTier: c.Query("service_tier"),
	}
}

func (h *Handler) SelectUserRealmRegion(c *gin.Context) {
	userID := c.Param("user_id")
	if userID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": "user_id required"})
		return
	}

	var req struct {
		EgressID string `json:"egress_id" binding:"required"`
		// ★P2：产品面二元组（可选，缺省=付费住宅面）。也可用 query 传，body 优先。
		PlanTier    string `json:"plan_tier"`
		ServiceTier string `json:"service_tier"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": "egress_id required"})
		return
	}

	resp, err := h.vpnService.SelectRealmEgress(c.Request.Context(), userID, req.EgressID, realmFaceFromBody(c, req.PlanTier, req.ServiceTier))
	if err != nil {
		// 业务级失败：原样透传 manager 的状态码 + error 字符串（switch_rate_limited / egress_offline /
		// egress_not_found）+ retry_after_sec，供前端按状态码分流与倒计时（§Q7）。
		var realmErr *client.RealmAPIError
		if errors.As(err, &realmErr) {
			data := gin.H{"ok": false, "error": realmErr.Code}
			if realmErr.RetryAfterSec > 0 {
				data["retry_after_sec"] = realmErr.RetryAfterSec
			}
			c.JSON(realmErr.HTTPStatus, gin.H{"success": false, "data": data})
			return
		}
		// 无 otun_uuid → no_assignment（404）。
		if errors.Is(err, service.ErrNoRealmAssignment) {
			c.JSON(http.StatusNotFound, gin.H{
				"success": false,
				"data":    gin.H{"ok": false, "error": "no_assignment"},
			})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"success": true, "data": resp})
}

// GetUserRealmCountries 按国家聚合 online 出口（internal，对应 BFF GET /resources/vpn/countries，§2c）。
func (h *Handler) GetUserRealmCountries(c *gin.Context) {
	userID := c.Param("user_id")
	if userID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": "user_id required"})
		return
	}
	resp, err := h.vpnService.ListRealmCountries(c.Request.Context(), userID, realmFaceFromQuery(c))
	if err != nil {
		if errors.Is(err, service.ErrNoRealmAssignment) {
			// 未开通 residential → 空国家列表（前端隐藏选国入口）。
			c.JSON(http.StatusOK, gin.H{"success": true, "data": gin.H{"countries": []interface{}{}}})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": resp})
}

// SelectUserRealmCountry 切目的国（internal，对应 BFF POST /resources/vpn/select-country，§2c）。
// 请求体 {"country":"..."}；user_id 来自路径（BFF 从 JWT 注入）。
func (h *Handler) SelectUserRealmCountry(c *gin.Context) {
	userID := c.Param("user_id")
	if userID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": "user_id required"})
		return
	}
	var req struct {
		Country string `json:"country" binding:"required"`
		// ★P2：产品面二元组（可选，缺省=付费住宅面）。也可用 query 传，body 优先。
		PlanTier    string `json:"plan_tier"`
		ServiceTier string `json:"service_tier"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": "country required"})
		return
	}
	resp, err := h.vpnService.SelectRealmCountry(c.Request.Context(), userID, req.Country, realmFaceFromBody(c, req.PlanTier, req.ServiceTier))
	if err != nil {
		// 业务级失败：透传 manager 状态码 + error（not_residential 403 / no_online_egress_in_country 409 /
		// switch_rate_limited 429）+ retry_after_sec（§Q7）。
		var realmErr *client.RealmAPIError
		if errors.As(err, &realmErr) {
			data := gin.H{"ok": false, "error": realmErr.Code}
			if realmErr.RetryAfterSec > 0 {
				data["retry_after_sec"] = realmErr.RetryAfterSec
			}
			c.JSON(realmErr.HTTPStatus, gin.H{"success": false, "data": data})
			return
		}
		if errors.Is(err, service.ErrNoRealmAssignment) {
			c.JSON(http.StatusNotFound, gin.H{"success": false, "data": gin.H{"ok": false, "error": "no_assignment"}})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": resp})
}

// GetUserRealmConnectURL 取用户当前出口连接 URL（internal，对应 BFF GET /resources/vpn/connect-url，可选）。
func (h *Handler) GetUserRealmConnectURL(c *gin.Context) {
	userID := c.Param("user_id")
	if userID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": "user_id required"})
		return
	}

	resp, err := h.vpnService.GetRealmConnectURLForUser(c.Request.Context(), userID, realmFaceFromQuery(c))
	if err != nil {
		if errors.Is(err, service.ErrNoRealmAssignment) {
			c.JSON(http.StatusNotFound, gin.H{
				"success": false,
				"data":    gin.H{"ok": false, "error": "no_assignment"},
			})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": err.Error()})
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

// UpdateUserEmail 更新用户邮箱（subscription-service 邮箱绑定事件触发）
// PUT /api/internal/users/:user_id/email
func (h *Handler) UpdateUserEmail(c *gin.Context) {
	userID := c.Param("user_id")

	var req struct {
		Email string `json:"email" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "email is required"})
		return
	}

	if err := h.vpnService.UpdateUserEmail(c.Request.Context(), userID, req.Email); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "email updated"})
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

// GetUserVPNProfiles 后台排障读口：GET /api/internal/admin/users/:user_id/vpn-profiles
// 返回该用户两面（standard/residential）的 entitlement profiles + entries（记账层原样，不做裁决）。
func (h *Handler) GetUserVPNProfiles(c *gin.Context) {
	userID := c.Param("user_id")
	if userID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "user_id required"})
		return
	}
	if h.entitlementProfiles == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"success": false, "error": "entitlement profiles not configured"})
		return
	}
	resp, err := h.entitlementProfiles.AdminView(c.Request.Context(), userID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": resp})
}
