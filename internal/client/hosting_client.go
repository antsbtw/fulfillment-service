package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"
)

// HostingClient calls obox-hosting-service to manage VPS nodes
type HostingClient struct {
	baseURL    string
	adminKey   string
	httpClient *http.Client
}

// NewHostingClient creates a new hosting client
func NewHostingClient(baseURL, adminKey string) *HostingClient {
	return &HostingClient{
		baseURL:  baseURL,
		adminKey: adminKey,
		httpClient: &http.Client{
			Timeout: 60 * time.Second,
		},
	}
}

// CreateNodeRequest is the request to create a node
type CreateNodeRequest struct {
	CloudProvider  string `json:"cloud_provider,omitempty"`  // aws, digitalocean
	Region         string `json:"region,omitempty"`
	BundleID       string `json:"bundle_id,omitempty"`       // nano_3_0, small_3_0, etc.
	SubscriptionID string `json:"subscription_id,omitempty"` // 对账单 ID（hosting-service 要求 fulfillment 必填）
	UserID         string `json:"user_id,omitempty"`         // 用户 ID（hosting-service 要求 fulfillment 必填）
}

// CreateNodeResponse is the response from creating a node
type CreateNodeResponse struct {
	NodeID  string `json:"node_id"`
	Status  string `json:"status"`
	Message string `json:"message"`
	Error   string `json:"error,omitempty"`
}

// NodeInfo contains full node details
type NodeInfo struct {
	NodeID        string `json:"node_id"`
	PublicIP      string `json:"public_ip,omitempty"`
	CloudProvider string `json:"cloud_provider"`
	CloudRegion   string `json:"cloud_region"`
	Status        string `json:"status"`
	NodeAPIKey    string `json:"node_api_key,omitempty"`
	VLESSPort     int    `json:"vless_port"`
	SSPort        int    `json:"ss_port,omitempty"`
	PublicKey     string `json:"public_key,omitempty"`
	ShortID       string `json:"short_id,omitempty"`
	ErrorMessage  string `json:"error_message,omitempty"`
	CreatedAt     string `json:"created_at"`
}

// DeleteNodeResponse is the response from deleting a node
type DeleteNodeResponse struct {
	NodeID  string `json:"node_id"`
	Status  string `json:"status"`
	Message string `json:"message"`
	Error   string `json:"error,omitempty"`
}

// CreateNode creates a new VPS node via obox-hosting-service
func (c *HostingClient) CreateNode(ctx context.Context, req *CreateNodeRequest) (*CreateNodeResponse, error) {
	log.Printf("[HostingClient] Creating node (provider: %s, region: %s)", req.CloudProvider, req.Region)

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", c.baseURL+"/api/admin/nodes", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("X-Admin-Key", c.adminKey)

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("send request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	var result CreateNodeResponse
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("decode response: %w (body: %s)", err, string(respBody))
	}

	if resp.StatusCode != http.StatusOK {
		return &result, fmt.Errorf("hosting-service returned status %d: %s", resp.StatusCode, result.Error)
	}

	log.Printf("[HostingClient] Node created: %s (status: %s)", result.NodeID, result.Status)
	return &result, nil
}

// GetNode gets node details by ID
func (c *HostingClient) GetNode(ctx context.Context, nodeID string) (*NodeInfo, error) {
	httpReq, err := http.NewRequestWithContext(ctx, "GET", c.baseURL+"/api/admin/nodes/"+nodeID, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	httpReq.Header.Set("X-Admin-Key", c.adminKey)

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("send request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("node not found: %s", nodeID)
	}

	var result NodeInfo
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("decode response: %w (body: %s)", err, string(respBody))
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("hosting-service returned status %d", resp.StatusCode)
	}

	return &result, nil
}

// DeleteNode deletes a node by ID
func (c *HostingClient) DeleteNode(ctx context.Context, nodeID string) (*DeleteNodeResponse, error) {
	log.Printf("[HostingClient] Deleting node: %s", nodeID)

	httpReq, err := http.NewRequestWithContext(ctx, "DELETE", c.baseURL+"/api/admin/nodes/"+nodeID, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	httpReq.Header.Set("X-Admin-Key", c.adminKey)

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("send request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	var result DeleteNodeResponse
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("decode response: %w (body: %s)", err, string(respBody))
	}

	if resp.StatusCode != http.StatusOK {
		return &result, fmt.Errorf("hosting-service returned status %d: %s", resp.StatusCode, result.Error)
	}

	log.Printf("[HostingClient] Node deleted: %s", nodeID)
	return &result, nil
}

// WaitForNodeReady polls until node is active or failed
func (c *HostingClient) WaitForNodeReady(ctx context.Context, nodeID string, maxWait time.Duration) (*NodeInfo, error) {
	log.Printf("[HostingClient] Waiting for node %s to be ready (max %v)", nodeID, maxWait)

	deadline := time.Now().Add(maxWait)
	pollInterval := 5 * time.Second

	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		node, err := c.GetNode(ctx, nodeID)
		if err != nil {
			log.Printf("[HostingClient] Error getting node status: %v", err)
			time.Sleep(pollInterval)
			continue
		}

		log.Printf("[HostingClient] Node %s status: %s", nodeID, node.Status)

		switch node.Status {
		case "active":
			return node, nil
		case "failed":
			return node, fmt.Errorf("node creation failed: %s", node.ErrorMessage)
		case "deleted":
			return nil, fmt.Errorf("node was deleted")
		}

		time.Sleep(pollInterval)
	}

	return nil, fmt.Errorf("timeout waiting for node to be ready")
}
