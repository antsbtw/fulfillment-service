package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"time"
)

// OTunClient calls otun-manager to manage VPN users
type OTunClient struct {
	baseURL        string
	internalSecret string
	httpClient     *http.Client
}

// NewOTunClient creates a new OTun manager client
func NewOTunClient(baseURL, internalSecret string) *OTunClient {
	return &OTunClient{
		baseURL:        baseURL,
		internalSecret: internalSecret,
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
	AuthUserID   string   `json:"auth_user_id,omitempty"`
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
	TrafficLimit int64   `json:"traffic_limit,omitempty"`
	TrafficUsed  int64   `json:"traffic_used,omitempty"`
	ExpireAt     string  `json:"expire_at,omitempty"`
	Enabled      *bool   `json:"enabled,omitempty"`
	Email        *string `json:"email,omitempty"`
	// ServiceTier 套餐升降级（如 standard→residential）才传；空则 manager 保留原值。
	// 修复：已存在用户升级套餐时 service_tier 不更新 → residential 链在 UpdateUser 路径断。
	ServiceTier string `json:"service_tier,omitempty"`
}

// VPNUserInfo contains VPN user details
type VPNUserInfo struct {
	UUID          string   `json:"uuid"`
	Email         string   `json:"email,omitempty"`
	ExternalID    string   `json:"external_id,omitempty"`
	Protocols     []string `json:"protocols"`
	TrafficLimit  int64    `json:"traffic_limit"`
	TrafficUsed   int64    `json:"traffic_used"`
	ExpireAt      string   `json:"expire_at"`
	Enabled       bool     `json:"enabled"`
	PrimaryNodeID *string  `json:"primary_node_id,omitempty"`
	BackupNodeID  *string  `json:"backup_node_id,omitempty"`
	CreatedAt     string   `json:"created_at"`
	UpdatedAt     string   `json:"updated_at"`
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

	// B2（2026-07-08 整改）：standard 面同样带出口国 + 分流策略（rules 模型，与 realm 面同款）。
	// SmartStrategy 用 RawMessage 原样透传，fulfillment 不解析内部结构。老 otun 缺字段 → 零值，
	// omitempty 下不输出（前端按"无此字段"降级，与升级前行为一致）。
	ExitCountry   string          `json:"exit_country"`
	SmartStrategy json.RawMessage `json:"smart_strategy"`
}

// Protocol represents a VPN protocol configuration
type Protocol struct {
	Protocol string `json:"protocol"`
	URL      string `json:"url"`
	Node     string `json:"node"`
}

// setAuthHeader 设置内部服务认证头
func (c *OTunClient) setAuthHeader(req *http.Request) {
	if c.internalSecret != "" {
		req.Header.Set("X-Internal-Secret", c.internalSecret)
	}
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
	c.setAuthHeader(httpReq)

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

	c.setAuthHeader(httpReq)

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
	c.setAuthHeader(httpReq)

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

	c.setAuthHeader(httpReq)

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
	c.setAuthHeader(httpReq)

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

// SyncUser gets VPN user sync info including protocols (GET /api/users/:uuid/sync)
func (c *OTunClient) SyncUser(ctx context.Context, uuid string) (*SubscribeResponse, error) {
	httpReq, err := http.NewRequestWithContext(ctx, "GET", c.baseURL+"/api/users/"+uuid+"/sync", nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	c.setAuthHeader(httpReq)

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
		return nil, fmt.Errorf("otun-manager sync returned status %d: %s", resp.StatusCode, string(respBody))
	}

	var result SubscribeResponse
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("decode response: %w (body: %s)", err, string(respBody))
	}

	return &result, nil
}

// GetUserStats gets VPN user traffic statistics
func (c *OTunClient) GetUserStats(ctx context.Context, uuid string) (*VPNUserInfo, error) {
	httpReq, err := http.NewRequestWithContext(ctx, "GET", c.baseURL+"/api/users/"+uuid+"/stats", nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	c.setAuthHeader(httpReq)

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

// RealmConnectURLResponse 是 otun-manager realm connect-url 接口的响应。
// P0：ExitCountry + SmartStrategy 透传 manager 下发的出口国家与客户端分流策略。
// SmartStrategy 用 RawMessage 原样透传，fulfillment 不解析内部结构。
type RealmConnectURLResponse struct {
	OK       bool   `json:"ok"`
	EgressID string `json:"egress_id"`
	// ConnectURL：hy2 单协议 URL，向后兼容旧客户端（只认单 URL）。
	ConnectURL string `json:"connect_url"`
	// ★ConnectURLs：manager 下发的六协议各自 <proto>-realm:// URL。老 struct 无此字段会静默丢弃，
	// 补上后 select/connect-url 可选链才能把六协议透传给 App（六协议下发到端的关键中间环节）。
	ConnectURLs   []string        `json:"connect_urls,omitempty"`
	ExitCountry   string          `json:"exit_country,omitempty"`
	SmartStrategy json.RawMessage `json:"smart_strategy,omitempty"`
	// ★2c：otun-manager 下发的 N=2 出口块（primary + backup，各自六协议 URL + region）。
	// 老 otun / 空则该字段缺省，fulfillment 退回单出口 ConnectURLs（向后兼容）。
	Nodes []RealmNode `json:"nodes,omitempty"`
	// ★residential 真实用量：otun-manager 从 realm_users 下发（agent 上报按 uuid 跨出口聚合的真源）。
	// /all residential 项用它回显，替代 vpn_provisions.traffic_used（那列恒 0 从不回写）。
	// 老 otun 无此字段 → 0，buildSubscribeResponse 侧退回 vp.TrafficUsed（向后兼容）。
	TrafficUsed  int64 `json:"traffic_used,omitempty"`
	TrafficLimit int64 `json:"traffic_limit,omitempty"`
}

// RealmNode 是 otun-manager /connect-url 返回的一个 N=2 出口块（2b-1/2c）。
type RealmNode struct {
	EgressID    string   `json:"egress_id"`
	Role        string   `json:"role"` // primary / backup
	Region      string   `json:"region,omitempty"`
	ConnectURL  string   `json:"connect_url"`
	ConnectURLs []string `json:"connect_urls"`
}

// GetRealmConnectURL 取 residential 用户【当前出口】的 realm 连接 URL（hysteria2-realm://...）。
// 调 otun-manager 的 GET /api/v1/internal/realm/connect-url?user_uuid=（走 X-Internal-Secret）。
// userUUID 必须是 otun VPN users.uuid（= vpn_provision.OtunUUID），不是 auth uuid。
func (c *OTunClient) GetRealmConnectURL(ctx context.Context, userUUID string) (*RealmConnectURLResponse, error) {
	httpReq, err := http.NewRequestWithContext(ctx, "GET",
		c.baseURL+"/api/v1/internal/realm/connect-url?user_uuid="+url.QueryEscape(userUUID), nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	c.setAuthHeader(httpReq)

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("send request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	// 未分配出口（404 no_assignment）等视为"暂无 realm URL"，返回 nil 让调用方降级，不报错中断。
	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("otun-manager realm connect-url status %d: %s", resp.StatusCode, string(respBody))
	}

	var result RealmConnectURLResponse
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("decode response: %w (body: %s)", err, string(respBody))
	}
	return &result, nil
}

// ==================== Realm 区域选择/切换（§4.5，BFF 透传链路）====================
//
// 这两个方法是给 user-portal BFF 用的：list 出口 + 切换出口。它们要把
// otun-manager 的「业务级失败」（switch_rate_limited / egress_offline /
// no_assignment / egress_not_found）连同原始 HTTP 状态码原样透传上去，
// 而不是塞进一个泛化的 5xx error 里——前端靠状态码 + error 字符串分流。
// 因此用 RealmAPIError 承载结构化失败，调用方据此把同样的 body+status 回给前端。

// RealmEgressItem 是面向 App 的安全投影出口项（绝不含 token/sni/stun）。
type RealmEgressItem struct {
	EgressID    string `json:"egress_id"`
	Region      string `json:"region"`
	DisplayName string `json:"display_name"`
	Online      bool   `json:"online"`
	IsCurrent   bool   `json:"is_current"`
}

// RealmEgressListResponse 对应 manager GET /egresses 的裸响应。
type RealmEgressListResponse struct {
	Egresses []RealmEgressItem `json:"egresses"`
}

// RealmSelectResponse 对应 manager POST /select 的裸响应（成功/失败同结构）。
// P1：成功切换附带新出口的 exit_country + smart_strategy，原样透传给前端。
type RealmSelectResponse struct {
	OK         bool   `json:"ok"`
	ConnectURL string `json:"connect_url,omitempty"`
	// ★六协议：切出口后 manager 也下发六协议 URL；补此字段以透传给 App（BFF 逐字节透传，不用改）。
	ConnectURLs   []string        `json:"connect_urls,omitempty"`
	Error         string          `json:"error,omitempty"`
	RetryAfterSec int             `json:"retry_after_sec,omitempty"`
	ExitCountry   string          `json:"exit_country,omitempty"`
	SmartStrategy json.RawMessage `json:"smart_strategy,omitempty"`
	// ★2c N=2：otun-manager /select 也下发 primary+backup 出口块（各自六协议 URL + region）。
	// 老 struct 无此字段 → BFF 反序列化时静默丢弃 → App 只收到 flat connect_urls（单节点），
	// 拿不到 backup、拿不到 node 角色 → 无法做 urltest 主备容灾。补齐后与 /select-country
	// 的 RealmConnectURLResponse.Nodes 对齐。空/老 otun 时该字段缺省，退回单出口（向后兼容）。
	Nodes []RealmNode `json:"nodes,omitempty"`
	// ★切换确认握手 P4：manager /select 下发的 ready（目标出口 agent 是否已确认应用该用户）。
	// false=已落库未生效，App 应轮询 region-status 到 true 再启动连接（契约
	// REALM_FRONTEND_CONTRACT_SWITCH_READY.md）。不加 omitempty——App 要能看到 false。
	Ready bool `json:"ready"`
}

// RealmReadyResponse 对应 manager GET /api/v1/internal/realm/ready 的裸响应。
type RealmReadyResponse struct {
	Ready    bool   `json:"ready"`
	EgressID string `json:"egress_id"`
}

// GetRealmUserReady 查目标出口 agent 是否已确认应用该用户（切换确认握手轮询下游）。
// egressID 可空（manager 缺省取用户当前 assignment 的出口）。404（无 assignment）返回 nil。
func (c *OTunClient) GetRealmUserReady(ctx context.Context, userUUID, egressID string) (*RealmReadyResponse, error) {
	u := c.baseURL + "/api/v1/internal/realm/ready?user_uuid=" + url.QueryEscape(userUUID)
	if egressID != "" {
		u += "&egress_id=" + url.QueryEscape(egressID)
	}
	httpReq, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	c.setAuthHeader(httpReq)

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
		return nil, nil // no_assignment → 调用方按 ErrNoRealmAssignment 处理
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("otun-manager realm ready status %d: %s", resp.StatusCode, string(respBody))
	}

	var result RealmReadyResponse
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("decode response: %w (body: %s)", err, string(respBody))
	}
	return &result, nil
}

// RealmAPIError 承载 otun-manager 返回的「业务级失败」，保留原始 HTTP 状态码
// 与结构化字段，供上游（fulfillment handler → BFF）原样透传给前端。
type RealmAPIError struct {
	HTTPStatus    int
	Code          string // 如 switch_rate_limited / egress_offline / no_assignment / egress_not_found
	RetryAfterSec int
}

func (e *RealmAPIError) Error() string {
	return fmt.Sprintf("realm api error: status=%d code=%s retry_after_sec=%d", e.HTTPStatus, e.Code, e.RetryAfterSec)
}

// ListRealmEgresses 列出用户可选出口（manager GET /egresses）。
// userUUID 必须是 otun VPN users.uuid（= vpn_provision.OtunUUID），不是 auth uuid。
func (c *OTunClient) ListRealmEgresses(ctx context.Context, userUUID string) (*RealmEgressListResponse, error) {
	httpReq, err := http.NewRequestWithContext(ctx, "GET",
		c.baseURL+"/api/v1/internal/realm/egresses?user_uuid="+url.QueryEscape(userUUID), nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	c.setAuthHeader(httpReq)

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
		return nil, fmt.Errorf("otun-manager realm egresses status %d: %s", resp.StatusCode, string(respBody))
	}

	var result RealmEgressListResponse
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("decode response: %w (body: %s)", err, string(respBody))
	}
	return &result, nil
}

// SelectRealmEgress 切换用户当前出口（manager POST /select）。
// 业务级失败（429/409/404 + error 字符串）以 *RealmAPIError 返回，原始状态码与字段保留，
// 由上游原样透传前端；网络/解码等系统级错误以普通 error 返回。
func (c *OTunClient) SelectRealmEgress(ctx context.Context, userUUID, egressID string) (*RealmSelectResponse, error) {
	reqBody, err := json.Marshal(map[string]string{
		"user_uuid": userUUID,
		"egress_id": egressID,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST",
		c.baseURL+"/api/v1/internal/realm/select", bytes.NewReader(reqBody))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	c.setAuthHeader(httpReq)

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("send request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode == http.StatusOK {
		var result RealmSelectResponse
		if err := json.Unmarshal(respBody, &result); err != nil {
			return nil, fmt.Errorf("decode response: %w (body: %s)", err, string(respBody))
		}
		return &result, nil
	}

	// 业务级失败：429/409/404，body 形如 {"ok":false,"error":"...","retry_after_sec":N}。
	// 解析出结构化字段，连同原始状态码以 RealmAPIError 上抛，供透传。
	if resp.StatusCode == http.StatusTooManyRequests ||
		resp.StatusCode == http.StatusConflict ||
		resp.StatusCode == http.StatusNotFound {
		var parsed RealmSelectResponse
		_ = json.Unmarshal(respBody, &parsed)
		return nil, &RealmAPIError{
			HTTPStatus:    resp.StatusCode,
			Code:          parsed.Error,
			RetryAfterSec: parsed.RetryAfterSec,
		}
	}

	return nil, fmt.Errorf("otun-manager realm select status %d: %s", resp.StatusCode, string(respBody))
}

// RealmCountry 是 /countries 聚合结果的一行（§7.2，零凭证）。
type RealmCountry struct {
	Country         string `json:"country"`
	DisplayName     string `json:"display_name"`
	OnlineNodeCount int    `json:"online_node_count"`
	IsCurrent       bool   `json:"is_current"`
}

// RealmCountriesResponse 是 manager /countries 的响应。
type RealmCountriesResponse struct {
	Countries []RealmCountry `json:"countries"`
}

// ListRealmCountries 按国家聚合 online 出口（manager GET /countries，§7.2 零凭证）。
func (c *OTunClient) ListRealmCountries(ctx context.Context, userUUID string) (*RealmCountriesResponse, error) {
	httpReq, err := http.NewRequestWithContext(ctx, "GET",
		c.baseURL+"/api/v1/internal/realm/countries?user_uuid="+url.QueryEscape(userUUID), nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	c.setAuthHeader(httpReq)

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
		return nil, fmt.Errorf("otun-manager realm countries status %d: %s", resp.StatusCode, string(respBody))
	}
	var result RealmCountriesResponse
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("decode response: %w (body: %s)", err, string(respBody))
	}
	return &result, nil
}

// SelectRealmCountry 切目的国（manager POST /select-country）。业务级失败(403/409/429)以 *RealmAPIError 上抛。
func (c *OTunClient) SelectRealmCountry(ctx context.Context, userUUID, country string) (*RealmConnectURLResponse, error) {
	reqBody, err := json.Marshal(map[string]string{"user_uuid": userUUID, "country": country})
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, "POST",
		c.baseURL+"/api/v1/internal/realm/select-country", bytes.NewReader(reqBody))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	c.setAuthHeader(httpReq)

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("send request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode == http.StatusOK {
		var result RealmConnectURLResponse
		if err := json.Unmarshal(respBody, &result); err != nil {
			return nil, fmt.Errorf("decode response: %w (body: %s)", err, string(respBody))
		}
		return &result, nil
	}
	// 业务级失败：403 not_residential / 409 no_online_egress_in_country / 429 switch_rate_limited。
	if resp.StatusCode == http.StatusForbidden ||
		resp.StatusCode == http.StatusConflict ||
		resp.StatusCode == http.StatusTooManyRequests {
		var parsed RealmSelectResponse
		_ = json.Unmarshal(respBody, &parsed)
		return nil, &RealmAPIError{HTTPStatus: resp.StatusCode, Code: parsed.Error, RetryAfterSec: parsed.RetryAfterSec}
	}
	return nil, fmt.Errorf("otun-manager realm select-country status %d: %s", resp.StatusCode, string(respBody))
}
