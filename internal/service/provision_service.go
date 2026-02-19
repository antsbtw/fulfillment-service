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

// ProvisionService handles hosting node provisioning operations
type ProvisionService struct {
	cfg                *config.Config
	hostingRepo        *repository.HostingProvisionRepository
	regionRepo         *repository.RegionRepository
	logRepo            *repository.LogRepository
	hostingClient      *client.HostingClient
	subscriptionClient *client.SubscriptionClient
}

// NewProvisionService creates a new provision service
func NewProvisionService(
	cfg *config.Config,
	hostingRepo *repository.HostingProvisionRepository,
	regionRepo *repository.RegionRepository,
	logRepo *repository.LogRepository,
	hostingClient *client.HostingClient,
	subscriptionClient *client.SubscriptionClient,
) *ProvisionService {
	return &ProvisionService{
		cfg:                cfg,
		hostingRepo:        hostingRepo,
		regionRepo:         regionRepo,
		logRepo:            logRepo,
		hostingClient:      hostingClient,
		subscriptionClient: subscriptionClient,
	}
}

// Provision starts the provisioning process for a new hosting node
func (s *ProvisionService) Provision(ctx context.Context, req *models.ProvisionRequest) (*models.ProvisionResponse, error) {
	log.Printf("[Provision] Starting provisioning for subscription=%s, user=%s",
		req.SubscriptionID, req.UserID)

	// Validate region if specified
	region := req.Region
	if region == "" {
		region = s.cfg.Hosting.DefaultRegion
	}

	// Check if user already has an active hosting node
	existing, err := s.hostingRepo.GetActiveByUser(ctx, req.UserID)
	if err == nil && existing != nil {
		return nil, fmt.Errorf("user already has an active hosting_node resource")
	}

	// Create hosting provision record
	provisionID := uuid.New().String()
	hp := &models.HostingProvision{
		ID:             provisionID,
		SubscriptionID: req.SubscriptionID,
		UserID:         req.UserID,
		Channel:        req.Channel,
		Provider:       s.cfg.Hosting.CloudProvider,
		Region:         region,
		Status:         models.StatusPending,
		PlanTier:       req.PlanTier,
		TrafficLimit:   req.TrafficLimit,
	}

	if err := s.hostingRepo.Create(ctx, hp); err != nil {
		return nil, fmt.Errorf("create hosting provision: %w", err)
	}

	// Log action
	s.logRepo.LogAction(ctx, provisionID, "hosting", "provision_started", "pending",
		fmt.Sprintf("Provisioning started for hosting_node in region %s", region))

	// Start async provisioning
	go s.provisionAsync(provisionID, req, region)

	return &models.ProvisionResponse{
		ResourceID:            provisionID,
		Status:                models.StatusPending,
		EstimatedReadySeconds: 300,
		Message:               "Provisioning started",
	}, nil
}

// provisionAsync handles the actual provisioning in the background
func (s *ProvisionService) provisionAsync(provisionID string, req *models.ProvisionRequest, region string) {
	ctx := context.Background()

	// Notify subscription-service that provisioning started
	if err := s.subscriptionClient.NotifyProvisioningStarted(ctx, req.SubscriptionID, provisionID); err != nil {
		log.Printf("[Provision] Failed to notify subscription-service (start): %v", err)
	}

	// Update status to creating
	s.updateStatus(ctx, provisionID, models.StatusCreating, nil)

	// Get bundle ID based on plan tier
	bundleID := s.getBundleID(req.PlanTier)

	// Call obox-hosting-service to create node
	createReq := &client.CreateNodeRequest{
		CloudProvider:  s.cfg.Hosting.CloudProvider,
		Region:         region,
		BundleID:       bundleID,
		SubscriptionID: req.SubscriptionID,
		UserID:         req.UserID,
	}

	createResp, err := s.hostingClient.CreateNode(ctx, createReq)
	if err != nil {
		s.handleProvisionError(ctx, req.SubscriptionID, provisionID, fmt.Sprintf("create node via hosting-service: %v", err))
		return
	}

	// Store the external node ID
	nodeID := createResp.NodeID
	hp, _ := s.hostingRepo.GetByID(ctx, provisionID)
	if hp != nil {
		hp.HostingNodeID = nodeID
		hp.Status = models.StatusCreating
		s.hostingRepo.Update(ctx, hp)
	}

	s.logRepo.LogAction(ctx, provisionID, "hosting", "node_creating", "creating",
		fmt.Sprintf("Node %s created in hosting-service, waiting for active state", nodeID))

	// Wait for node to be active
	node, err := s.hostingClient.WaitForNodeReady(ctx, nodeID, 10*time.Minute)
	if err != nil {
		s.handleProvisionError(ctx, req.SubscriptionID, provisionID, fmt.Sprintf("wait for node ready: %v", err))
		return
	}

	// Update hosting provision with node information
	hp, _ = s.hostingRepo.GetByID(ctx, provisionID)
	if hp != nil {
		publicIP := node.PublicIP
		apiKey := node.NodeAPIKey
		publicKey := node.PublicKey
		shortID := node.ShortID
		now := time.Now()

		hp.PublicIP = &publicIP
		hp.APIPort = s.cfg.Node.APIPort
		hp.APIKey = &apiKey
		hp.VlessPort = node.VLESSPort
		hp.SSPort = node.SSPort
		hp.PublicKey = &publicKey
		hp.ShortID = &shortID
		hp.Status = models.StatusActive
		hp.ReadyAt = &now
		s.hostingRepo.Update(ctx, hp)
	}

	s.logRepo.LogAction(ctx, provisionID, "hosting", "node_ready", "active",
		fmt.Sprintf("Node active at %s", node.PublicIP))

	// Notify subscription-service that node is active
	callback := &models.NodeReadyCallback{
		ResourceID: provisionID,
		PublicIP:   node.PublicIP,
		APIPort:    s.cfg.Node.APIPort,
		APIKey:     node.NodeAPIKey,
		VlessPort:  node.VLESSPort,
		SSPort:     node.SSPort,
		PublicKey:  node.PublicKey,
		ShortID:    node.ShortID,
	}
	if err := s.subscriptionClient.NotifyActive(ctx, req.SubscriptionID, provisionID, callback); err != nil {
		log.Printf("[Provision] Failed to notify subscription-service (active): %v", err)
	}

	log.Printf("[Provision] Resource %s provisioning complete! Node active at %s", provisionID, node.PublicIP)
}

// HandleNodeReady handles callback when node software is ready
func (s *ProvisionService) HandleNodeReady(ctx context.Context, callback *models.NodeReadyCallback) error {
	log.Printf("[Provision] Node ready callback for resource %s", callback.ResourceID)

	hp, err := s.hostingRepo.GetByID(ctx, callback.ResourceID)
	if err != nil {
		return fmt.Errorf("get hosting provision: %w", err)
	}

	publicIP := callback.PublicIP
	apiKey := callback.APIKey
	publicKey := callback.PublicKey
	shortID := callback.ShortID
	now := time.Now()

	hp.PublicIP = &publicIP
	hp.APIPort = callback.APIPort
	hp.APIKey = &apiKey
	hp.VlessPort = callback.VlessPort
	hp.SSPort = callback.SSPort
	hp.PublicKey = &publicKey
	hp.ShortID = &shortID
	hp.Status = models.StatusActive
	hp.ReadyAt = &now

	if err := s.hostingRepo.Update(ctx, hp); err != nil {
		return fmt.Errorf("update hosting provision: %w", err)
	}

	s.logRepo.LogAction(ctx, hp.ID, "hosting", "node_ready", "active",
		fmt.Sprintf("Node software installed, resource is active at %s", publicIP))

	// Notify subscription-service
	if err := s.subscriptionClient.NotifyActive(ctx, hp.SubscriptionID, hp.ID, callback); err != nil {
		log.Printf("[Provision] Failed to notify subscription-service (active): %v", err)
	}

	return nil
}

// HandleNodeFailed handles callback when node installation fails
func (s *ProvisionService) HandleNodeFailed(ctx context.Context, callback *models.NodeFailedCallback) error {
	log.Printf("[Provision] Node failed callback for resource %s: %s", callback.ResourceID, callback.ErrorMessage)

	hp, err := s.hostingRepo.GetByID(ctx, callback.ResourceID)
	if err != nil {
		return fmt.Errorf("get hosting provision: %w", err)
	}

	s.handleProvisionError(context.Background(), hp.SubscriptionID, hp.ID, callback.ErrorMessage)
	return nil
}

// Deprovision starts the deprovisioning process
func (s *ProvisionService) Deprovision(ctx context.Context, req *models.DeprovisionRequest) (*models.DeprovisionResponse, error) {
	log.Printf("[Deprovision] Starting deprovisioning for subscription=%s", req.SubscriptionID)

	var hp *models.HostingProvision
	var err error

	if req.ResourceID != "" {
		hp, err = s.hostingRepo.GetByID(ctx, req.ResourceID)
	} else {
		provisions, qErr := s.hostingRepo.GetBySubscriptionID(ctx, req.SubscriptionID)
		if qErr == nil && len(provisions) > 0 {
			hp = provisions[0]
		} else {
			err = qErr
		}
	}

	if err != nil || hp == nil {
		return nil, fmt.Errorf("resource not found")
	}

	go s.deprovisionAsync(hp, req.Reason)

	return &models.DeprovisionResponse{
		ResourceID: hp.ID,
		Status:     models.StatusStopping,
		Message:    "Deprovisioning started",
	}, nil
}

// deprovisionAsync handles the actual deprovisioning in the background
func (s *ProvisionService) deprovisionAsync(hp *models.HostingProvision, reason string) {
	ctx := context.Background()

	s.updateStatus(ctx, hp.ID, models.StatusStopping, nil)

	// Delete node via hosting-service
	if hp.HostingNodeID != "" {
		_, err := s.hostingClient.DeleteNode(ctx, hp.HostingNodeID)
		if err != nil {
			log.Printf("[Deprovision] Warning: failed to delete node: %v", err)
		}
	}

	now := time.Now()
	hp.Status = models.StatusDeleted
	hp.DeletedAt = &now
	s.hostingRepo.Update(ctx, hp)

	s.logRepo.LogAction(ctx, hp.ID, "hosting", "deprovisioned", "deleted",
		fmt.Sprintf("Resource deprovisioned. Reason: %s", reason))

	// Notify subscription-service
	if err := s.subscriptionClient.NotifyDeleted(ctx, hp.SubscriptionID, hp.ID); err != nil {
		log.Printf("[Deprovision] Failed to notify subscription-service (deleted): %v", err)
	}

	log.Printf("[Deprovision] Resource %s successfully deprovisioned", hp.ID)
}

// GetResourceStatus gets the status of a hosting provision
func (s *ProvisionService) GetResourceStatus(ctx context.Context, resourceID string) (*models.ResourceStatusResponse, error) {
	hp, err := s.hostingRepo.GetByID(ctx, resourceID)
	if err != nil {
		return nil, err
	}
	return s.hostingToStatusResponse(hp), nil
}

// GetResourcesBySubscription gets hosting provisions for a subscription
func (s *ProvisionService) GetResourcesBySubscription(ctx context.Context, subscriptionID string) ([]*models.ResourceStatusResponse, error) {
	provisions, err := s.hostingRepo.GetBySubscriptionID(ctx, subscriptionID)
	if err != nil {
		return nil, err
	}

	var responses []*models.ResourceStatusResponse
	for _, hp := range provisions {
		responses = append(responses, s.hostingToStatusResponse(hp))
	}
	return responses, nil
}

// GetUserNodeStatus gets node status for a user with full subscription info
func (s *ProvisionService) GetUserNodeStatus(ctx context.Context, userID string) (*models.UserNodeStatusResponse, error) {
	// 1. Check subscription status
	subStatus, err := s.subscriptionClient.GetUserHostingSubscription(ctx, userID)
	if err != nil {
		log.Printf("[GetUserNodeStatus] Error checking subscription: %v", err)
		subStatus = nil
	}

	// 2. Check hosting provision (including failed)
	hp, nodeErr := s.hostingRepo.GetLatestByUser(ctx, userID)

	// 3. Build response
	resp := &models.UserNodeStatusResponse{}

	if subStatus == nil || !subStatus.HasActive {
		resp.HostingStatus = models.HostingStatusNoSubscription
		resp.HasSubscription = false
		resp.HasNode = false
		resp.Message = "No active hosting subscription. Please subscribe to create a node."
		return resp, nil
	}

	resp.HasSubscription = true
	resp.Subscription = &models.SubscriptionInfo{
		SubscriptionID: subStatus.SubscriptionID,
		Status:         subStatus.Status,
		PlanTier:       subStatus.PlanTier,
		ExpiresAt:      subStatus.ExpiresAt,
		AutoRenew:      subStatus.AutoRenew,
	}

	if nodeErr != nil || hp == nil {
		resp.HostingStatus = models.HostingStatusSubscribedNoNode
		resp.HasNode = false
		resp.Message = "You have an active subscription. You can create a node now."
		return resp, nil
	}

	resp.HasNode = true

	regionName := hp.Region
	region, err := s.regionRepo.GetByCode(ctx, hp.Region)
	if err == nil && region != nil {
		regionName = region.Name
	}

	trafficLimitGB := float64(hp.TrafficLimit) / (1024 * 1024 * 1024)
	trafficUsedGB := float64(hp.TrafficUsed) / (1024 * 1024 * 1024)
	trafficPercent := 0.0
	if hp.TrafficLimit > 0 {
		trafficPercent = (float64(hp.TrafficUsed) / float64(hp.TrafficLimit)) * 100
	}

	resp.Node = &models.UserNodeInfo{
		ResourceID:     hp.ID,
		Region:         hp.Region,
		RegionName:     regionName,
		Status:         hp.Status,
		PublicIP:       hp.PublicIP,
		APIPort:        hp.APIPort,
		APIKey:         hp.APIKey,
		VlessPort:      hp.VlessPort,
		SSPort:         hp.SSPort,
		PublicKey:      hp.PublicKey,
		ShortID:        hp.ShortID,
		PlanTier:       hp.PlanTier,
		TrafficLimitGB: trafficLimitGB,
		TrafficUsedGB:  trafficUsedGB,
		TrafficPercent: trafficPercent,
		CreatedAt:      hp.CreatedAt.Format(time.RFC3339),
	}

	switch hp.Status {
	case models.StatusPending, models.StatusCreating, models.StatusRunning, models.StatusInstalling:
		resp.HostingStatus = models.HostingStatusNodeCreating
		resp.CreationProgress = s.buildCreationProgress(hp.Status)
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

	existing, _ := s.hostingRepo.GetLatestByUser(ctx, userID)
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
			log.Printf("[CreateUserNode] Auto-cleaning failed node: resource_id=%s", existing.ID)
			if err := s.cleanupFailedProvision(ctx, existing); err != nil {
				log.Printf("[CreateUserNode] Warning: failed to cleanup failed node: %v", err)
			}
		}
	}

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

	subStatus, err := s.subscriptionClient.GetUserHostingSubscription(ctx, userID)
	if err != nil {
		log.Printf("[DeleteUserNode] Error checking subscription: %v", err)
	}

	hp, err := s.hostingRepo.GetLatestByUser(ctx, userID)
	if err != nil || hp == nil {
		return &models.DeleteNodeResponse{
			Success: false,
			Message: "No node found to delete.",
		}, nil
	}

	if hp.Status == models.StatusCreating || hp.Status == models.StatusRunning {
		return &models.DeleteNodeResponse{
			Success: false,
			Message: "Cannot delete node while it's being created. Please wait until creation completes or fails.",
		}, nil
	}

	subscriptionID := hp.SubscriptionID
	if subStatus != nil && subStatus.HasActive {
		subscriptionID = subStatus.SubscriptionID
	}

	_, err = s.Deprovision(ctx, &models.DeprovisionRequest{
		SubscriptionID: subscriptionID,
		ResourceID:     hp.ID,
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

func (s *ProvisionService) updateStatus(ctx context.Context, provisionID, status string, errorMsg *string) {
	if err := s.hostingRepo.UpdateStatus(ctx, provisionID, status, errorMsg); err != nil {
		log.Printf("[Provision] Failed to update status: %v", err)
	}
}

func (s *ProvisionService) handleProvisionError(ctx context.Context, subscriptionID, provisionID, errorMsg string) {
	log.Printf("[Provision] Provisioning failed for %s: %s", provisionID, errorMsg)

	s.updateStatus(ctx, provisionID, models.StatusFailed, &errorMsg)
	s.logRepo.LogAction(ctx, provisionID, "hosting", "provision_failed", "failed", errorMsg)

	if err := s.subscriptionClient.NotifyFailed(ctx, subscriptionID, provisionID, errorMsg); err != nil {
		log.Printf("[Provision] Failed to notify subscription-service (failed): %v", err)
	}
}

func (s *ProvisionService) getBundleID(planTier string) string {
	switch planTier {
	case "premium", "3tb":
		return "small_3_0"
	case "standard", "2tb":
		return "micro_3_0"
	case "basic", "1tb":
		return "nano_3_0"
	default:
		return "nano_3_0"
	}
}

func (s *ProvisionService) getTrafficLimit(planTier string) int64 {
	const TB = int64(1024 * 1024 * 1024 * 1024)
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

func (s *ProvisionService) hostingToStatusResponse(hp *models.HostingProvision) *models.ResourceStatusResponse {
	trafficLimitGB := float64(hp.TrafficLimit) / (1024 * 1024 * 1024)
	trafficUsedGB := float64(hp.TrafficUsed) / (1024 * 1024 * 1024)
	trafficPercent := 0.0
	if hp.TrafficLimit > 0 {
		trafficPercent = (float64(hp.TrafficUsed) / float64(hp.TrafficLimit)) * 100
	}

	resp := &models.ResourceStatusResponse{
		ResourceID:     hp.ID,
		SubscriptionID: hp.SubscriptionID,
		UserID:         hp.UserID,
		ResourceType:   models.ResourceTypeHostingNode,
		Provider:       hp.Provider,
		Region:         hp.Region,
		Status:         hp.Status,
		PublicIP:       hp.PublicIP,
		APIPort:        hp.APIPort,
		APIKey:         hp.APIKey,
		VlessPort:      hp.VlessPort,
		SSPort:         hp.SSPort,
		PublicKey:      hp.PublicKey,
		ShortID:        hp.ShortID,
		PlanTier:       hp.PlanTier,
		TrafficLimitGB: trafficLimitGB,
		TrafficUsedGB:  trafficUsedGB,
		TrafficPercent: trafficPercent,
		CreatedAt:      hp.CreatedAt.Format(time.RFC3339),
		ErrorMessage:   hp.ErrorMessage,
	}

	if hp.ReadyAt != nil {
		readyAt := hp.ReadyAt.Format(time.RFC3339)
		resp.ReadyAt = &readyAt
	}

	return resp
}

func (s *ProvisionService) cleanupFailedProvision(ctx context.Context, hp *models.HostingProvision) error {
	log.Printf("[cleanupFailedProvision] Cleaning up failed provision: id=%s, user=%s", hp.ID, hp.UserID)

	if hp.HostingNodeID != "" {
		log.Printf("[cleanupFailedProvision] Attempting to cleanup external resource: %s", hp.HostingNodeID)
		if _, err := s.hostingClient.DeleteNode(ctx, hp.HostingNodeID); err != nil {
			log.Printf("[cleanupFailedProvision] Warning: failed to delete external resource: %v", err)
		}
	}

	now := time.Now()
	hp.Status = models.StatusDeleted
	hp.DeletedAt = &now

	if err := s.hostingRepo.Update(ctx, hp); err != nil {
		return fmt.Errorf("failed to mark provision as deleted: %w", err)
	}

	s.logRepo.LogAction(ctx, hp.ID, "hosting", "auto_cleanup", "deleted", "Auto-cleaned failed resource to allow retry")

	log.Printf("[cleanupFailedProvision] Successfully cleaned up failed provision: %s", hp.ID)
	return nil
}
