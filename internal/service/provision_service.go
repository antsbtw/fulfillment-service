package service

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/google/uuid"
	"github.com/wenwu/saas-platform/fulfillment-service/internal/client"
	"github.com/wenwu/saas-platform/fulfillment-service/internal/config"
	"github.com/wenwu/saas-platform/fulfillment-service/internal/models"
	"github.com/wenwu/saas-platform/fulfillment-service/internal/repository"
)

// ProvisionService handles resource provisioning operations
type ProvisionService struct {
	cfg                *config.Config
	resourceRepo       *repository.ResourceRepository
	regionRepo         *repository.RegionRepository
	logRepo            *repository.LogRepository
	hostingClient      *client.HostingClient
	subscriptionClient *client.SubscriptionClient
}

// NewProvisionService creates a new provision service
func NewProvisionService(
	cfg *config.Config,
	resourceRepo *repository.ResourceRepository,
	regionRepo *repository.RegionRepository,
	logRepo *repository.LogRepository,
	hostingClient *client.HostingClient,
	subscriptionClient *client.SubscriptionClient,
) *ProvisionService {
	return &ProvisionService{
		cfg:                cfg,
		resourceRepo:       resourceRepo,
		regionRepo:         regionRepo,
		logRepo:            logRepo,
		hostingClient:      hostingClient,
		subscriptionClient: subscriptionClient,
	}
}

// Provision starts the provisioning process for a new resource
func (s *ProvisionService) Provision(ctx context.Context, req *models.ProvisionRequest) (*models.ProvisionResponse, error) {
	log.Printf("[Provision] Starting provisioning for subscription=%s, user=%s, type=%s",
		req.SubscriptionID, req.UserID, req.ResourceType)

	// Validate region if specified
	region := req.Region
	if region == "" {
		region = s.cfg.Hosting.DefaultRegion
	}

	// Check if user already has an active resource of this type
	existing, err := s.resourceRepo.GetActiveByUserAndType(ctx, req.UserID, req.ResourceType)
	if err == nil && existing != nil {
		return nil, fmt.Errorf("user already has an active %s resource", req.ResourceType)
	}

	// Create resource record
	resourceID := uuid.New().String()
	resource := &models.Resource{
		ID:             resourceID,
		SubscriptionID: req.SubscriptionID,
		UserID:         req.UserID,
		ResourceType:   req.ResourceType,
		Provider:       s.cfg.Hosting.CloudProvider,
		Region:         region,
		Status:         models.StatusPending,
		PlanTier:       req.PlanTier,
		TrafficLimit:   req.TrafficLimit,
	}

	if err := s.resourceRepo.Create(ctx, resource); err != nil {
		return nil, fmt.Errorf("create resource record: %w", err)
	}

	// Log action
	s.logRepo.LogAction(ctx, resourceID, "provision_started", "pending",
		fmt.Sprintf("Provisioning started for %s in region %s", req.ResourceType, region))

	// Start async provisioning
	go s.provisionAsync(resourceID, req, region)

	return &models.ProvisionResponse{
		ResourceID:            resourceID,
		Status:                models.StatusPending,
		EstimatedReadySeconds: 300, // 5 minutes for VPS creation + installation
		Message:               "Provisioning started",
	}, nil
}

// provisionAsync handles the actual provisioning in the background
func (s *ProvisionService) provisionAsync(resourceID string, req *models.ProvisionRequest, region string) {
	ctx := context.Background()

	// Notify subscription-service that provisioning started
	if err := s.subscriptionClient.NotifyProvisioningStarted(ctx, req.SubscriptionID, resourceID); err != nil {
		log.Printf("[Provision] Failed to notify subscription-service (start): %v", err)
	}

	// Update status to creating
	s.updateStatus(ctx, resourceID, models.StatusCreating, nil)

	// Get bundle ID based on plan tier
	bundleID := s.getBundleID(req.PlanTier)

	// Call obox-hosting-service to create node
	createReq := &client.CreateNodeRequest{
		CloudProvider: s.cfg.Hosting.CloudProvider,
		Region:        region,
		BundleID:      bundleID,
	}

	createResp, err := s.hostingClient.CreateNode(ctx, createReq)
	if err != nil {
		s.handleProvisionError(ctx, req.SubscriptionID, resourceID, fmt.Sprintf("create node via hosting-service: %v", err))
		return
	}

	// Store the external node ID
	nodeID := createResp.NodeID
	resource, _ := s.resourceRepo.GetByID(ctx, resourceID)
	if resource != nil {
		resource.InstanceID = &nodeID
		resource.Status = models.StatusCreating
		s.resourceRepo.Update(ctx, resource)
	}

	s.logRepo.LogAction(ctx, resourceID, "node_creating", "creating",
		fmt.Sprintf("Node %s created in hosting-service, waiting for active state", nodeID))

	// Wait for node to be active (obox-hosting-service handles VPS creation + installation)
	node, err := s.hostingClient.WaitForNodeReady(ctx, nodeID, 10*time.Minute)
	if err != nil {
		s.handleProvisionError(ctx, req.SubscriptionID, resourceID, fmt.Sprintf("wait for node ready: %v", err))
		return
	}

	// Update resource with node information
	resource, _ = s.resourceRepo.GetByID(ctx, resourceID)
	if resource != nil {
		publicIP := node.PublicIP
		apiKey := node.NodeAPIKey
		publicKey := node.PublicKey
		shortID := node.ShortID
		now := time.Now()

		resource.PublicIP = &publicIP
		resource.APIPort = s.cfg.Node.APIPort
		resource.APIKey = &apiKey
		resource.VlessPort = node.VLESSPort
		resource.SSPort = node.SSPort
		resource.PublicKey = &publicKey
		resource.ShortID = &shortID
		resource.Status = models.StatusActive
		resource.ReadyAt = &now
		s.resourceRepo.Update(ctx, resource)
	}

	s.logRepo.LogAction(ctx, resourceID, "node_ready", "active",
		fmt.Sprintf("Node active at %s", node.PublicIP))

	// Notify subscription-service that node is active
	callback := &models.NodeReadyCallback{
		ResourceID: resourceID,
		PublicIP:   node.PublicIP,
		APIPort:    s.cfg.Node.APIPort,
		APIKey:     node.NodeAPIKey,
		VlessPort:  node.VLESSPort,
		SSPort:     node.SSPort,
		PublicKey:  node.PublicKey,
		ShortID:    node.ShortID,
	}
	if err := s.subscriptionClient.NotifyActive(ctx, req.SubscriptionID, resourceID, callback); err != nil {
		log.Printf("[Provision] Failed to notify subscription-service (active): %v", err)
	}

	log.Printf("[Provision] Resource %s provisioning complete! Node active at %s", resourceID, node.PublicIP)
}

// HandleNodeReady handles callback when node software is ready
func (s *ProvisionService) HandleNodeReady(ctx context.Context, callback *models.NodeReadyCallback) error {
	log.Printf("[Provision] Node ready callback for resource %s", callback.ResourceID)

	resource, err := s.resourceRepo.GetByID(ctx, callback.ResourceID)
	if err != nil {
		return fmt.Errorf("get resource: %w", err)
	}

	// Update resource with node info
	publicIP := callback.PublicIP
	apiKey := callback.APIKey
	publicKey := callback.PublicKey
	shortID := callback.ShortID
	now := time.Now()

	resource.PublicIP = &publicIP
	resource.APIPort = callback.APIPort
	resource.APIKey = &apiKey
	resource.VlessPort = callback.VlessPort
	resource.SSPort = callback.SSPort
	resource.PublicKey = &publicKey
	resource.ShortID = &shortID
	resource.Status = models.StatusActive
	resource.ReadyAt = &now

	if err := s.resourceRepo.Update(ctx, resource); err != nil {
		return fmt.Errorf("update resource: %w", err)
	}

	s.logRepo.LogAction(ctx, resource.ID, "node_ready", "active",
		fmt.Sprintf("Node software installed, resource is active at %s", publicIP))

	// Notify subscription-service
	if err := s.subscriptionClient.NotifyActive(ctx, resource.SubscriptionID, resource.ID, callback); err != nil {
		log.Printf("[Provision] Failed to notify subscription-service (active): %v", err)
	}

	return nil
}

// HandleNodeFailed handles callback when node installation fails
func (s *ProvisionService) HandleNodeFailed(ctx context.Context, callback *models.NodeFailedCallback) error {
	log.Printf("[Provision] Node failed callback for resource %s: %s", callback.ResourceID, callback.ErrorMessage)

	resource, err := s.resourceRepo.GetByID(ctx, callback.ResourceID)
	if err != nil {
		return fmt.Errorf("get resource: %w", err)
	}

	s.handleProvisionError(context.Background(), resource.SubscriptionID, resource.ID, callback.ErrorMessage)
	return nil
}

// Deprovision starts the deprovisioning process
func (s *ProvisionService) Deprovision(ctx context.Context, req *models.DeprovisionRequest) (*models.DeprovisionResponse, error) {
	log.Printf("[Deprovision] Starting deprovisioning for subscription=%s", req.SubscriptionID)

	var resource *models.Resource
	var err error

	if req.ResourceID != "" {
		resource, err = s.resourceRepo.GetByID(ctx, req.ResourceID)
	} else {
		resources, err := s.resourceRepo.GetBySubscriptionID(ctx, req.SubscriptionID)
		if err == nil && len(resources) > 0 {
			resource = resources[0]
		}
	}

	if err != nil || resource == nil {
		return nil, fmt.Errorf("resource not found")
	}

	// Start async deprovisioning
	go s.deprovisionAsync(resource, req.Reason)

	return &models.DeprovisionResponse{
		ResourceID: resource.ID,
		Status:     models.StatusStopping,
		Message:    "Deprovisioning started",
	}, nil
}

// deprovisionAsync handles the actual deprovisioning in the background
func (s *ProvisionService) deprovisionAsync(resource *models.Resource, reason string) {
	ctx := context.Background()

	s.updateStatus(ctx, resource.ID, models.StatusStopping, nil)

	// Delete node via hosting-service
	if resource.InstanceID != nil && *resource.InstanceID != "" {
		_, err := s.hostingClient.DeleteNode(ctx, *resource.InstanceID)
		if err != nil {
			log.Printf("[Deprovision] Warning: failed to delete node: %v", err)
		}
	}

	// Update status
	now := time.Now()
	resource.Status = models.StatusDeleted
	resource.DeletedAt = &now
	s.resourceRepo.Update(ctx, resource)

	s.logRepo.LogAction(ctx, resource.ID, "deprovisioned", "deleted",
		fmt.Sprintf("Resource deprovisioned. Reason: %s", reason))

	// Notify subscription-service
	if err := s.subscriptionClient.NotifyDeleted(ctx, resource.SubscriptionID, resource.ID); err != nil {
		log.Printf("[Deprovision] Failed to notify subscription-service (deleted): %v", err)
	}

	log.Printf("[Deprovision] Resource %s successfully deprovisioned", resource.ID)
}

// GetResourceStatus gets the status of a resource
func (s *ProvisionService) GetResourceStatus(ctx context.Context, resourceID string) (*models.ResourceStatusResponse, error) {
	resource, err := s.resourceRepo.GetByID(ctx, resourceID)
	if err != nil {
		return nil, err
	}

	return s.resourceToStatusResponse(resource), nil
}

// GetResourcesBySubscription gets resources for a subscription
func (s *ProvisionService) GetResourcesBySubscription(ctx context.Context, subscriptionID string) ([]*models.ResourceStatusResponse, error) {
	resources, err := s.resourceRepo.GetBySubscriptionID(ctx, subscriptionID)
	if err != nil {
		return nil, err
	}

	var responses []*models.ResourceStatusResponse
	for _, r := range resources {
		responses = append(responses, s.resourceToStatusResponse(r))
	}

	return responses, nil
}

// GetUserNodeStatus gets node status for a user with full subscription info
func (s *ProvisionService) GetUserNodeStatus(ctx context.Context, userID, resourceType string) (*models.UserNodeStatusResponse, error) {
	// 1. 检查用户订阅状态
	subStatus, err := s.subscriptionClient.GetUserHostingSubscription(ctx, userID)
	if err != nil {
		log.Printf("[GetUserNodeStatus] Error checking subscription: %v", err)
		subStatus = nil
	}

	// 2. 检查用户节点 (包括 failed 状态)
	resource, nodeErr := s.resourceRepo.GetLatestByUserAndType(ctx, userID, resourceType)

	// 3. 构建响应
	resp := &models.UserNodeStatusResponse{}

	// 无订阅
	if subStatus == nil || !subStatus.HasActive {
		resp.HostingStatus = models.HostingStatusNoSubscription
		resp.HasSubscription = false
		resp.HasNode = false
		resp.Message = "No active hosting subscription. Please subscribe to create a node."
		return resp, nil
	}

	// 有订阅
	resp.HasSubscription = true
	resp.Subscription = &models.SubscriptionInfo{
		SubscriptionID: subStatus.SubscriptionID,
		Status:         subStatus.Status,
		PlanTier:       subStatus.PlanTier,
		ExpiresAt:      subStatus.ExpiresAt,
		AutoRenew:      subStatus.AutoRenew,
	}

	// 无节点
	if nodeErr != nil || resource == nil {
		resp.HostingStatus = models.HostingStatusSubscribedNoNode
		resp.HasNode = false
		resp.Message = "You have an active subscription. You can create a node now."
		return resp, nil
	}

	// 有节点 - 根据状态返回不同信息
	resp.HasNode = true

	regionName := resource.Region
	region, err := s.regionRepo.GetByCode(ctx, resource.Region)
	if err == nil && region != nil {
		regionName = region.Name
	}

	trafficLimitGB := float64(resource.TrafficLimit) / (1024 * 1024 * 1024)
	trafficUsedGB := float64(resource.TrafficUsed) / (1024 * 1024 * 1024)
	trafficPercent := 0.0
	if resource.TrafficLimit > 0 {
		trafficPercent = (float64(resource.TrafficUsed) / float64(resource.TrafficLimit)) * 100
	}

	resp.Node = &models.UserNodeInfo{
		ResourceID:     resource.ID,
		Region:         resource.Region,
		RegionName:     regionName,
		Status:         resource.Status,
		PublicIP:       resource.PublicIP,
		APIPort:        resource.APIPort,
		APIKey:         resource.APIKey,
		VlessPort:      resource.VlessPort,
		SSPort:         resource.SSPort,
		PublicKey:      resource.PublicKey,
		ShortID:        resource.ShortID,
		PlanTier:       resource.PlanTier,
		TrafficLimitGB: trafficLimitGB,
		TrafficUsedGB:  trafficUsedGB,
		TrafficPercent: trafficPercent,
		CreatedAt:      resource.CreatedAt.Format(time.RFC3339),
	}

	// 根据节点状态设置 HostingStatus
	switch resource.Status {
	case models.StatusPending, models.StatusCreating, models.StatusRunning, models.StatusInstalling:
		resp.HostingStatus = models.HostingStatusNodeCreating
		resp.CreationProgress = s.buildCreationProgress(resource.Status)
		resp.Message = "Node is being created. Please wait..."
	case models.StatusActive:
		resp.HostingStatus = models.HostingStatusNodeActive
		resp.Message = "Node is active and ready to use."
	case models.StatusFailed:
		resp.HostingStatus = models.HostingStatusNodeFailed
		resp.Message = "Node creation failed. You can delete and recreate the node."
	default:
		resp.HostingStatus = models.HostingStatusSubscribedNoNode
		resp.HasNode = false
		resp.Node = nil
		resp.Message = "You can create a node now."
	}

	return resp, nil
}

// buildCreationProgress builds the creation progress based on status
func (s *ProvisionService) buildCreationProgress(status string) *models.NodeCreationProgress {
	steps := []models.NodeCreationStep{
		{Step: 1, Name: "Payment confirmed", Status: "completed"},
		{Step: 2, Name: "VPS creating", Status: "pending"},
		{Step: 3, Name: "Installing sing-box", Status: "pending"},
		{Step: 4, Name: "Node ready", Status: "pending"},
	}

	currentStep := 1
	stepName := "Payment confirmed"

	switch status {
	case models.StatusPending:
		currentStep = 1
		stepName = "Payment confirmed"
		steps[0].Status = "completed"
	case models.StatusCreating:
		currentStep = 2
		stepName = "VPS creating"
		steps[0].Status = "completed"
		steps[1].Status = "in_progress"
	case models.StatusRunning:
		currentStep = 2
		stepName = "VPS created"
		steps[0].Status = "completed"
		steps[1].Status = "completed"
	case models.StatusInstalling:
		currentStep = 3
		stepName = "Installing sing-box"
		steps[0].Status = "completed"
		steps[1].Status = "completed"
		steps[2].Status = "in_progress"
	case models.StatusActive:
		currentStep = 4
		stepName = "Node ready"
		for i := range steps {
			steps[i].Status = "completed"
		}
	}

	return &models.NodeCreationProgress{
		CurrentStep: currentStep,
		TotalSteps:  4,
		StepName:    stepName,
		Steps:       steps,
	}
}

// GetAvailableRegions gets available regions
func (s *ProvisionService) GetAvailableRegions(ctx context.Context) (*models.RegionListResponse, error) {
	regions, err := s.regionRepo.GetAvailable(ctx)
	if err != nil {
		return nil, err
	}

	var regionInfos []models.RegionInfo
	for _, r := range regions {
		regionInfos = append(regionInfos, models.RegionInfo{
			Code:      r.Code,
			Name:      r.Name,
			Provider:  r.Provider,
			Available: r.Available,
		})
	}

	return &models.RegionListResponse{Regions: regionInfos}, nil
}

// CreateUserNode creates a node for a user after verifying subscription
func (s *ProvisionService) CreateUserNode(ctx context.Context, userID, region string) (*models.CreateNodeResponse, error) {
	log.Printf("[CreateUserNode] Creating node for user=%s, region=%s", userID, region)

	// 1. 检查订阅状态
	subStatus, err := s.subscriptionClient.GetUserHostingSubscription(ctx, userID)
	if err != nil {
		log.Printf("[CreateUserNode] Error checking subscription: %v", err)
		return &models.CreateNodeResponse{
			Success: false,
			Status:  "failed",
			Message: "Unable to verify subscription status. Please try again later.",
		}, nil
	}

	if subStatus == nil || !subStatus.HasActive {
		return &models.CreateNodeResponse{
			Success: false,
			Status:  "failed",
			Message: "No active hosting subscription found. Please subscribe first.",
		}, nil
	}

	// 2. 检查是否已有节点 (包括失败状态)
	existing, _ := s.resourceRepo.GetLatestByUserAndType(ctx, userID, models.ResourceTypeHostingNode)
	if existing != nil {
		switch existing.Status {
		case models.StatusActive:
			return &models.CreateNodeResponse{
				Success: false,
				Status:  "failed",
				Message: "You already have an active node. Please delete it first if you want to create a new one.",
			}, nil
		case models.StatusPending, models.StatusCreating, models.StatusRunning, models.StatusInstalling:
			return &models.CreateNodeResponse{
				Success:          true,
				ResourceID:       existing.ID,
				Status:           "creating",
				CreationProgress: s.buildCreationProgress(existing.Status),
				Message:          "Node is already being created. Please wait.",
			}, nil
		case models.StatusFailed:
			return &models.CreateNodeResponse{
				Success: false,
				Status:  "failed",
				Message: "You have a failed node. Please delete it first before creating a new one.",
			}, nil
		}
	}

	// 3. 开始创建节点
	provisionReq := &models.ProvisionRequest{
		SubscriptionID: subStatus.SubscriptionID,
		UserID:         userID,
		ResourceType:   models.ResourceTypeHostingNode,
		PlanTier:       subStatus.PlanTier,
		Region:         region,
		TrafficLimit:   s.getTrafficLimit(subStatus.PlanTier),
	}

	resp, err := s.Provision(ctx, provisionReq)
	if err != nil {
		return &models.CreateNodeResponse{
			Success: false,
			Status:  "failed",
			Message: fmt.Sprintf("Failed to start node creation: %v", err),
		}, nil
	}

	return &models.CreateNodeResponse{
		Success:          true,
		ResourceID:       resp.ResourceID,
		Status:           "creating",
		CreationProgress: s.buildCreationProgress(models.StatusPending),
		Message:          "Node creation started. This may take a few minutes.",
	}, nil
}

// DeleteUserNode deletes a user's node
func (s *ProvisionService) DeleteUserNode(ctx context.Context, userID string) (*models.DeleteNodeResponse, error) {
	log.Printf("[DeleteUserNode] Deleting node for user=%s", userID)

	// 1. 检查订阅状态
	subStatus, err := s.subscriptionClient.GetUserHostingSubscription(ctx, userID)
	if err != nil {
		log.Printf("[DeleteUserNode] Error checking subscription: %v", err)
	}

	// 2. 查找用户的节点 (包括失败状态)
	resource, err := s.resourceRepo.GetLatestByUserAndType(ctx, userID, models.ResourceTypeHostingNode)
	if err != nil || resource == nil {
		return &models.DeleteNodeResponse{
			Success: false,
			Message: "No node found to delete.",
		}, nil
	}

	// 3. 检查节点状态 - 创建中的节点不能删除
	if resource.Status == models.StatusCreating || resource.Status == models.StatusRunning {
		return &models.DeleteNodeResponse{
			Success: false,
			Message: "Cannot delete node while it's being created. Please wait until creation completes or fails.",
		}, nil
	}

	// 4. 获取订阅 ID
	subscriptionID := resource.SubscriptionID
	if subStatus != nil && subStatus.HasActive {
		subscriptionID = subStatus.SubscriptionID
	}

	// 5. 开始删除节点
	_, err = s.Deprovision(ctx, &models.DeprovisionRequest{
		SubscriptionID: subscriptionID,
		ResourceID:     resource.ID,
		Reason:         "User initiated deletion",
	})
	if err != nil {
		return &models.DeleteNodeResponse{
			Success: false,
			Message: fmt.Sprintf("Failed to delete node: %v", err),
		}, nil
	}

	return &models.DeleteNodeResponse{
		Success: true,
		Message: "Node deletion started. You can create a new node once deletion is complete.",
	}, nil
}

// Helper functions

func (s *ProvisionService) updateStatus(ctx context.Context, resourceID, status string, errorMsg *string) {
	if err := s.resourceRepo.UpdateStatus(ctx, resourceID, status, errorMsg); err != nil {
		log.Printf("[Provision] Failed to update status: %v", err)
	}
}

func (s *ProvisionService) handleProvisionError(ctx context.Context, subscriptionID, resourceID, errorMsg string) {
	log.Printf("[Provision] Provisioning failed for %s: %s", resourceID, errorMsg)

	s.updateStatus(ctx, resourceID, models.StatusFailed, &errorMsg)
	s.logRepo.LogAction(ctx, resourceID, "provision_failed", "failed", errorMsg)

	if err := s.subscriptionClient.NotifyFailed(ctx, subscriptionID, resourceID, errorMsg); err != nil {
		log.Printf("[Provision] Failed to notify subscription-service (failed): %v", err)
	}
}

func (s *ProvisionService) getBundleID(planTier string) string {
	switch planTier {
	case "premium", "3tb":
		return "small_3_0" // 2vCPU, 2GB RAM, 60GB SSD, 3TB traffic
	case "standard", "2tb":
		return "micro_3_0" // 2vCPU, 1GB RAM, 40GB SSD, 2TB traffic
	case "basic", "1tb":
		return "nano_3_0" // 2vCPU, 512MB RAM, 20GB SSD, 1TB traffic
	default:
		return "nano_3_0"
	}
}

// getTrafficLimit returns traffic limit in bytes based on plan tier
func (s *ProvisionService) getTrafficLimit(planTier string) int64 {
	const TB = int64(1024 * 1024 * 1024 * 1024) // 1 TB in bytes
	switch planTier {
	case "premium", "3tb":
		return 3 * TB
	case "standard", "2tb":
		return 2 * TB
	case "basic", "1tb":
		return 1 * TB
	default:
		return 1 * TB
	}
}

func (s *ProvisionService) resourceToStatusResponse(r *models.Resource) *models.ResourceStatusResponse {
	trafficLimitGB := float64(r.TrafficLimit) / (1024 * 1024 * 1024)
	trafficUsedGB := float64(r.TrafficUsed) / (1024 * 1024 * 1024)
	trafficPercent := 0.0
	if r.TrafficLimit > 0 {
		trafficPercent = (float64(r.TrafficUsed) / float64(r.TrafficLimit)) * 100
	}

	resp := &models.ResourceStatusResponse{
		ResourceID:     r.ID,
		SubscriptionID: r.SubscriptionID,
		UserID:         r.UserID,
		ResourceType:   r.ResourceType,
		Provider:       r.Provider,
		Region:         r.Region,
		Status:         r.Status,
		PublicIP:       r.PublicIP,
		APIPort:        r.APIPort,
		APIKey:         r.APIKey,
		VlessPort:      r.VlessPort,
		SSPort:         r.SSPort,
		PublicKey:      r.PublicKey,
		ShortID:        r.ShortID,
		PlanTier:       r.PlanTier,
		TrafficLimitGB: trafficLimitGB,
		TrafficUsedGB:  trafficUsedGB,
		TrafficPercent: trafficPercent,
		CreatedAt:      r.CreatedAt.Format(time.RFC3339),
		ErrorMessage:   r.ErrorMessage,
	}

	if r.ReadyAt != nil {
		readyAt := r.ReadyAt.Format(time.RFC3339)
		resp.ReadyAt = &readyAt
	}

	return resp
}
