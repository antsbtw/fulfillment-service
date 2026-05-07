package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

type OBoxClient struct {
	baseURL    string
	secret     string
	httpClient *http.Client
}

func NewOBoxClient(baseURL, secret string) *OBoxClient {
	return &OBoxClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		secret:  secret,
		httpClient: &http.Client{
			Timeout: 5 * time.Second,
		},
	}
}

type OBoxInstance struct {
	UserID   string    `json:"user_id"`
	VPSID    string    `json:"vps_id"`
	ExpireAt time.Time `json:"expire_at"`
	PlanTier string    `json:"plan_tier"`
}

type OBoxRenewRequest struct {
	UserID       string     `json:"user_id"`
	VPSID        string     `json:"vps_id"`
	Channel      string     `json:"channel"`
	ChannelSubID *string    `json:"channel_sub_id,omitempty"`
	ChannelTxID  *string    `json:"channel_tx_id,omitempty"`
	ProductID    string     `json:"product_id"`
	PurchaseType string     `json:"purchase_type"`
	Amount       *int64     `json:"amount,omitempty"`
	Currency     *string    `json:"currency,omitempty"`
	LastOrderAt  time.Time  `json:"last_order_at"`
	PlanTier     string     `json:"plan_tier"`
	Quantity     int        `json:"quantity"`
	Months       int        `json:"months"`
	TrafficLimit int64      `json:"traffic_limit,omitempty"`
	StartedAt    time.Time  `json:"started_at"`
	PeriodStart  time.Time  `json:"period_start"`
	ExpireAt     time.Time  `json:"expire_at"`
}

func (c *OBoxClient) Renew(ctx context.Context, req *OBoxRenewRequest) error {
	body, err := json.Marshal(req)
	if err != nil {
		return err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/internal/obox/renew", bytes.NewReader(body))
	if err != nil {
		return err
	}
	c.setHeaders(httpReq)
	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("obox renew returned status %d", resp.StatusCode)
	}
	return nil
}

func (c *OBoxClient) DeleteInstance(ctx context.Context, userID string) error {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.baseURL+"/api/internal/obox/instance/"+userID, nil)
	if err != nil {
		return err
	}
	c.setHeaders(httpReq)
	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("obox delete returned status %d", resp.StatusCode)
	}
	return nil
}

func (c *OBoxClient) GetInstance(ctx context.Context, userID string) (*OBoxInstance, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/api/internal/obox/instance/"+userID, nil)
	if err != nil {
		return nil, err
	}
	c.setHeaders(httpReq)
	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("obox get returned status %d", resp.StatusCode)
	}
	var inst OBoxInstance
	if err := json.NewDecoder(resp.Body).Decode(&inst); err != nil {
		return nil, err
	}
	return &inst, nil
}

func (c *OBoxClient) setHeaders(req *http.Request) {
	req.Header.Set("Content-Type", "application/json")
	if c.secret != "" {
		req.Header.Set("X-Internal-Secret", c.secret)
	}
}
