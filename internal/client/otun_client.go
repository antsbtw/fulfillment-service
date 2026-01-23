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

// OTunClient calls otun-manager to manage VPN users
type OTunClient struct {
	baseURL    string
	httpClient *http.Client
}

// NewOTunClient creates a new OTun manager client
func NewOTunClient(baseURL string) *OTunClient {
	return &OTunClient{
		baseURL: baseURL,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// CreateVPNUserRequest is the request to create a VPN user
type CreateVPNUserRequest struct {
	UUID         string   `json:"uuid"`
	Email        string   `json:"email,omitempty"`
	ExternalID   string   `json:"external_id,omitempty"`
	Protocols    []string `json:"protocols"`
	SSPassword   string   `json:"ss_password"`
	TrafficLimit int64    `json:"traffic_limit"`
	ExpireAt     string   `json:"expire_at"`
	ServiceTier  string   `json:"service_tier,omitempty"` // basic, premium, residential
}

// CreateVPNUserResponse is the response from creating a VPN user
type CreateVPNUserResponse struct {
	UUID         string `json:"uuid"`
	Email        string `json:"email,omitempty"`
	SSPassword   string `json:"ss_password,omitempty"`
	TrafficLimit int64  `json:"traffic_limit"`
	TrafficUsed  int64  `json:"traffic_used"`
	ExpireAt     string `json:"expire_at"`
	Enabled      bool   `json:"enabled"`
	Error        string `json:"error,omitempty"`
}

// UpdateVPNUserRequest is the request to update a VPN user
type UpdateVPNUserRequest struct {
	TrafficLimit int64  `json:"traffic_limit,omitempty"`
	TrafficUsed  int64  `json:"traffic_used,omitempty"`
	ExpireAt     string `json:"expire_at,omitempty"`
	Enabled      *bool  `json:"enabled,omitempty"`
}

// VPNUserInfo contains VPN user details
type VPNUserInfo struct {
	UUID          string  `json:"uuid"`
	Email         string  `json:"email,omitempty"`
	ExternalID    string  `json:"external_id,omitempty"`
	Protocols     []string `json:"protocols"`
	TrafficLimit  int64   `json:"traffic_limit"`
	TrafficUsed   int64   `json:"traffic_used"`
	ExpireAt      string  `json:"expire_at"`
	Enabled       bool    `json:"enabled"`
	PrimaryNodeID *string `json:"primary_node_id,omitempty"`
	BackupNodeID  *string `json:"backup_node_id,omitempty"`
	CreatedAt     string  `json:"created_at"`
	UpdatedAt     string  `json:"updated_at"`
}

// SubscribeRequest is the request for subscribe endpoint
type SubscribeRequest struct {
	DeviceID    string `json:"device_id"`
	TrafficGB   int    `json:"traffic_gb"`
	DaysValid   int    `json:"days_valid"`
	RoutingMode string `json:"routing_mode,omitempty"`
}

// SubscribeResponse is the response from subscribe endpoint
type SubscribeResponse struct {
	UUID         string     `json:"uuid"`
	TrafficLimit int64      `json:"traffic_limit"`
	TrafficUsed  int64      `json:"traffic_used"`
	ExpireAt     string     `json:"expire_at"`
	Enabled      bool       `json:"enabled"`
	Protocols    []Protocol `json:"protocols"`
	Error        string     `json:"error,omitempty"`
}

// Protocol represents a VPN protocol configuration
type Protocol struct {
	Protocol string `json:"protocol"`
	URL      string `json:"url"`
	Node     string `json:"node"`
}

// CreateUser creates a new VPN user in otun-manager
func (c *OTunClient) CreateUser(ctx context.Context, req *CreateVPNUserRequest) (*CreateVPNUserResponse, error) {
	// 日志脱敏: 不记录 email 等 PII 信息
	log.Printf("[OTunClient] Creating VPN user (uuid: %s)", req.UUID)

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", c.baseURL+"/api/users", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("send request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	var result CreateVPNUserResponse
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("decode response: %w (body: %s)", err, string(respBody))
	}

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		errMsg := result.Error
		if errMsg == "" {
			errMsg = string(respBody)
		}
		return nil, fmt.Errorf("otun-manager returned status %d: %s", resp.StatusCode, errMsg)
	}

	log.Printf("[OTunClient] VPN user created: %s", result.UUID)
	return &result, nil
}

// GetUser gets VPN user details by UUID
func (c *OTunClient) GetUser(ctx context.Context, uuid string) (*VPNUserInfo, error) {
	httpReq, err := http.NewRequestWithContext(ctx, "GET", c.baseURL+"/api/users/"+uuid, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

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
		return nil, fmt.Errorf("user not found: %s", uuid)
	}

	var result VPNUserInfo
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("decode response: %w (body: %s)", err, string(respBody))
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("otun-manager returned status %d", resp.StatusCode)
	}

	return &result, nil
}

// UpdateUser updates a VPN user
func (c *OTunClient) UpdateUser(ctx context.Context, uuid string, req *UpdateVPNUserRequest) error {
	log.Printf("[OTunClient] Updating VPN user: %s", uuid)

	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "PUT", c.baseURL+"/api/users/"+uuid, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return fmt.Errorf("send request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("otun-manager returned status %d: %s", resp.StatusCode, string(respBody))
	}

	log.Printf("[OTunClient] VPN user updated: %s", uuid)
	return nil
}

// DisableUser disables a VPN user
func (c *OTunClient) DisableUser(ctx context.Context, uuid string) error {
	log.Printf("[OTunClient] Disabling VPN user: %s", uuid)
	enabled := false
	return c.UpdateUser(ctx, uuid, &UpdateVPNUserRequest{Enabled: &enabled})
}

// EnableUser enables a VPN user
func (c *OTunClient) EnableUser(ctx context.Context, uuid string) error {
	log.Printf("[OTunClient] Enabling VPN user: %s", uuid)
	enabled := true
	return c.UpdateUser(ctx, uuid, &UpdateVPNUserRequest{Enabled: &enabled})
}

// DeleteUser deletes a VPN user
func (c *OTunClient) DeleteUser(ctx context.Context, uuid string) error {
	log.Printf("[OTunClient] Deleting VPN user: %s", uuid)

	httpReq, err := http.NewRequestWithContext(ctx, "DELETE", c.baseURL+"/api/users/"+uuid, nil)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return fmt.Errorf("send request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("otun-manager returned status %d: %s", resp.StatusCode, string(respBody))
	}

	log.Printf("[OTunClient] VPN user deleted: %s", uuid)
	return nil
}

// GetSubscribeConfig gets VPN subscription config for a device
func (c *OTunClient) GetSubscribeConfig(ctx context.Context, req *SubscribeRequest) (*SubscribeResponse, error) {
	log.Printf("[OTunClient] Getting subscribe config for device: %s", req.DeviceID)

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", c.baseURL+"/api/subscribe", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("send request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	var result SubscribeResponse
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("decode response: %w (body: %s)", err, string(respBody))
	}

	if resp.StatusCode != http.StatusOK {
		errMsg := result.Error
		if errMsg == "" {
			errMsg = string(respBody)
		}
		return nil, fmt.Errorf("otun-manager returned status %d: %s", resp.StatusCode, errMsg)
	}

	return &result, nil
}

// GetUserStats gets VPN user traffic statistics
func (c *OTunClient) GetUserStats(ctx context.Context, uuid string) (*VPNUserInfo, error) {
	httpReq, err := http.NewRequestWithContext(ctx, "GET", c.baseURL+"/api/users/"+uuid+"/stats", nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("send request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("otun-manager returned status %d: %s", resp.StatusCode, string(respBody))
	}

	var result VPNUserInfo
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("decode response: %w (body: %s)", err, string(respBody))
	}

	return &result, nil
}
