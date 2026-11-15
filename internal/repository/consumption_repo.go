package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/wenwu/saas-platform/fulfillment-service/internal/models"
)

// Apple 退款申请回报使用量(payment-service docs/APPLE_CONSUMPTION_RESPONSE_DESIGN.md §4.2)用的只读查询。

// ListByUserChannelFace 取该用户某渠道、某产品面的全部行(含非 current),
// 用来判断"这一笔交易有没有真的开通到线路上"。
func (r *VPNProvisionRepository) ListByUserChannelFace(ctx context.Context, userID, channel, face string) ([]*models.VPNProvision, error) {
	query := fmt.Sprintf(`
		SELECT %s FROM fulfillment.vpn_provisions
		WHERE user_id = $1 AND channel = $2 AND COALESCE(product_face, 'basic') = $3
		ORDER BY updated_at DESC
	`, vpnColumns)
	rows, err := r.pool.Query(ctx, query, userID, channel, face)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return r.scanMany(rows)
}

// ListAliveSince 取该用户在 since 之后仍存活过的托管机(含已删除:删除时间晚于 since)。
// 托管行的 channel 经常为空,所以只按用户取。
func (r *HostingProvisionRepository) ListAliveSince(ctx context.Context, userID string, since time.Time) ([]*models.HostingProvision, error) {
	query := `
		SELECT id, subscription_id, user_id, channel,
			   hosting_node_id, provider, region,
			   public_ip, api_port, api_key, vless_port, ss_port, public_key, short_id,
			   status, error_message, plan_tier, traffic_limit, traffic_used, needs_cleanup, node_kind, needs_rebuild,
			   created_at, updated_at, ready_at, deleted_at
		FROM fulfillment.hosting_provisions
		WHERE user_id = $1 AND (deleted_at IS NULL OR deleted_at >= $2)
		ORDER BY created_at
	`
	rows, err := r.pool.Query(ctx, query, userID, since)
	if err != nil {
		return nil, fmt.Errorf("query hosting_provisions: %w", err)
	}
	defer rows.Close()
	return r.scanMany(rows)
}
