package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/wenwu/saas-platform/fulfillment-service/internal/models"
)

// SubscriptionClient handles communication with subscription-service
type SubscriptionClient struct {
	baseURL      string
	internalKey  string
	httpClient   *http.Client
}

// NewSubscriptionClient creates a new subscription service client
func NewSubscriptionClient(baseURL, internalKey string) *SubscriptionClient {
	return &SubscriptionClient{
		baseURL:     baseURL,
		internalKey: internalKey,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// NotifyResourceStatus sends resource status callback to subscription-service (v3.1 简化版)
func (c *SubscriptionClient) NotifyResourceStatus(ctx context.Context, callback *models.SubscriptionCallback) error {
	url := fmt.Sprintf("%s/api/internal/fulfillment/callback", c.baseURL)

	body, err := json.Marshal(callback)
	if err != nil {
		return fmt.Errorf("marshal callback: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Internal-Secret", c.internalKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("send request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return fmt.Errorf("subscription-service returned status %d", resp.StatusCode)
	}

	return nil
}

// NotifyProvisioningStarted notifies that provisioning has started (no-op in v3.1, subscription-service no longer tracks intermediate states)
func (c *SubscriptionClient) NotifyProvisioningStarted(ctx context.Context, subscriptionID, resourceID string) error {
	// v3.1: subscription-service 不再跟踪中间状态 (creating/installing)，只关心最终状态 (active/failed/deleted)
	// 保留方法签名以避免破坏调用方，但不发送回调
	return nil
}

// NotifyInstalling notifies that software installation has started (no-op in v3.1)
func (c *SubscriptionClient) NotifyInstalling(ctx context.Context, subscriptionID, resourceID, publicIP string) error {
	// v3.1: subscription-service 不再跟踪中间状态
	return nil
}

// NotifyActive notifies that resource is active and ready
func (c *SubscriptionClient) NotifyActive(ctx context.Context, subscriptionID, resourceID string, info *models.NodeReadyCallback) error {
	// Hosting (obox) 资源就绪
	return c.NotifyResourceStatus(ctx, &models.SubscriptionCallback{
		SubscriptionID: subscriptionID,
		App:            "obox",
		Status:         models.StatusActive,
		Message:        fmt.Sprintf("Resource %s is active", resourceID),
	})
}

// NotifyFailed notifies that provisioning has failed
func (c *SubscriptionClient) NotifyFailed(ctx context.Context, subscriptionID, resourceID, errorMsg string) error {
	// Hosting (obox) 开通失败
	return c.NotifyResourceStatus(ctx, &models.SubscriptionCallback{
		SubscriptionID: subscriptionID,
		App:            "obox",
		Status:         models.StatusFailed,
		Error:          errorMsg,
	})
}

// NotifyDeleted notifies that resource has been deleted
func (c *SubscriptionClient) NotifyDeleted(ctx context.Context, subscriptionID, resourceID string) error {
	// Hosting (obox) 资源删除
	return c.NotifyResourceStatus(ctx, &models.SubscriptionCallback{
		SubscriptionID: subscriptionID,
		App:            "obox",
		Status:         models.StatusDeleted,
		Message:        fmt.Sprintf("Resource %s deleted", resourceID),
	})
}

// NotifyVPNActive notifies that VPN user is active
func (c *SubscriptionClient) NotifyVPNActive(ctx context.Context, subscriptionID, resourceID string) error {
	return c.NotifyResourceStatus(ctx, &models.SubscriptionCallback{
		SubscriptionID: subscriptionID,
		App:            "otun",
		Status:         models.StatusActive,
		Message:        fmt.Sprintf("VPN resource %s is active", resourceID),
	})
}

// NotifyVPNFailed notifies that VPN provisioning failed
func (c *SubscriptionClient) NotifyVPNFailed(ctx context.Context, subscriptionID, resourceID, errorMsg string) error {
	return c.NotifyResourceStatus(ctx, &models.SubscriptionCallback{
		SubscriptionID: subscriptionID,
		App:            "otun",
		Status:         models.StatusFailed,
		Error:          errorMsg,
	})
}

// NotifyVPNDeleted notifies that VPN resource has been deleted
func (c *SubscriptionClient) NotifyVPNDeleted(ctx context.Context, subscriptionID, resourceID string) error {
	return c.NotifyResourceStatus(ctx, &models.SubscriptionCallback{
		SubscriptionID: subscriptionID,
		App:            "otun",
		Status:         models.StatusDeleted,
		Message:        fmt.Sprintf("VPN resource %s deleted", resourceID),
	})
}

// SubscriptionStatusResponse is the response from subscription-service
type SubscriptionStatusResponse struct {
	HasActive      bool   `json:"has_active"`
	SubscriptionID string `json:"subscription_id,omitempty"`
	Status         string `json:"status,omitempty"`
	PlanTier       string `json:"plan_tier,omitempty"`
	ExpiresAt      string `json:"expires_at,omitempty"`
	AutoRenew      bool   `json:"auto_renew,omitempty"`
}

// GetUserHostingSubscription checks if user has an active hosting subscription
func (c *SubscriptionClient) GetUserHostingSubscription(ctx context.Context, userID string) (*SubscriptionStatusResponse, error) {
	return c.getUserSubscription(ctx, userID, "obox", "hosting")
}

// GetUserVPNSubscription checks if user has an active VPN subscription
func (c *SubscriptionClient) GetUserVPNSubscription(ctx context.Context, userID string) (*SubscriptionStatusResponse, error) {
	return c.getUserSubscription(ctx, userID, "otun", "vpn")
}

// getUserSubscription is a generic method to check user subscription status
func (c *SubscriptionClient) getUserSubscription(ctx context.Context, userID, app, serviceType string) (*SubscriptionStatusResponse, error) {
	url := fmt.Sprintf("%s/api/internal/users/%s/active/%s/%s", c.baseURL, userID, app, serviceType)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("X-Internal-Secret", c.internalKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("send request: %w", err)
	}
	defer resp.Body.Close()

	// 404 means no active subscription
	if resp.StatusCode == http.StatusNotFound {
		return &SubscriptionStatusResponse{HasActive: false}, nil
	}

	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("subscription-service returned status %d", resp.StatusCode)
	}

	var result SubscriptionStatusResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	result.HasActive = true
	return &result, nil
}
