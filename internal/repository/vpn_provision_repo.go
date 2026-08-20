package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/wenwu/saas-platform/fulfillment-service/internal/models"
)

type VPNProvisionRepository struct {
	pool *pgxpool.Pool
}

func NewVPNProvisionRepository(pool *pgxpool.Pool) *VPNProvisionRepository {
	return &VPNProvisionRepository{pool: pool}
}

// vpnColumns：★迁移 010 起含 product_face（分区键）。上线顺序：迁移先于本代码。
const vpnColumns = `id, user_id, subscription_id, channel,
	business_type, service_tier, otun_uuid, plan_tier, status,
	traffic_limit, traffic_used, expire_at,
	email, device_id, granted_by, note, is_current,
	created_at, updated_at, COALESCE(product_face, 'basic')`

// notCampaign 是不分区（user 级）查询的默认谓词：campaign 面对 basic/residential 的既有读写路径
// 完全不可见（IMPL_PROMPT §2 "basic 分区显式排除 campaign" + 契约 C2）。
// ★同时排除改名前旧值 'campaign'：迁移 011 与代码滚动之间若有窗口，未改名的存量行会被
// 这些 user 级读路径当成 basic 命中（串面）。两值并排后，迁移与部署的先后顺序不再要紧。
const notCampaign = ` AND COALESCE(product_face, 'basic') NOT IN ('promo', 'campaign')`

func (r *VPNProvisionRepository) Create(ctx context.Context, vp *models.VPNProvision) error {
	query := `
		INSERT INTO fulfillment.vpn_provisions (
			id, user_id, subscription_id, channel,
			business_type, service_tier, otun_uuid, plan_tier, status,
			traffic_limit, traffic_used, expire_at,
			email, device_id, granted_by, note, is_current,
			product_face
		) VALUES (
			$1, $2, $3, $4,
			$5, $6, $7, $8, $9,
			$10, $11, $12,
			$13, $14, $15, $16, $17,
			$18
		)
	`
	// product_face 缺省按 (plan_tier, service_tier) 推导（与迁移 010 回填口径一致）。
	if vp.ProductFace == "" {
		vp.ProductFace = models.ProductFaceFor(vp.PlanTier, vp.ServiceTier)
	}
	_, err := r.pool.Exec(ctx, query,
		vp.ID, vp.UserID, vp.SubscriptionID, vp.Channel,
		vp.BusinessType, vp.ServiceTier, vp.OtunUUID, vp.PlanTier, vp.Status,
		vp.TrafficLimit, vp.TrafficUsed, vp.ExpireAt,
		vp.Email, vp.DeviceID, vp.GrantedBy, vp.Note, vp.IsCurrent,
		vp.ProductFace,
	)
	if err != nil {
		return fmt.Errorf("insert vpn_provision: %w", err)
	}
	return nil
}

func (r *VPNProvisionRepository) GetByID(ctx context.Context, id string) (*models.VPNProvision, error) {
	query := fmt.Sprintf(`SELECT %s FROM fulfillment.vpn_provisions WHERE id = $1`, vpnColumns)
	return r.scanOne(r.pool.QueryRow(ctx, query, id))
}

func (r *VPNProvisionRepository) GetCurrentByUser(ctx context.Context, userID string) (*models.VPNProvision, error) {
	query := fmt.Sprintf(`
		SELECT %s FROM fulfillment.vpn_provisions
		WHERE user_id = $1 AND is_current = TRUE AND status = 'active'`+notCampaign+`
		ORDER BY created_at DESC
		LIMIT 1
	`, vpnColumns)
	return r.scanOne(r.pool.QueryRow(ctx, query, userID))
}

func (r *VPNProvisionRepository) GetCurrentByUserAnyStatus(ctx context.Context, userID string) (*models.VPNProvision, error) {
	query := fmt.Sprintf(`
		SELECT %s FROM fulfillment.vpn_provisions
		WHERE user_id = $1 AND is_current = TRUE`+notCampaign+`
		ORDER BY created_at DESC
		LIMIT 1
	`, vpnColumns)
	return r.scanOne(r.pool.QueryRow(ctx, query, userID))
}

// GetCurrentByUserAndServicePartition 是 GetCurrentByUserAnyStatus 的分区版（MULTI_SERVICE_ENABLED=true 才用）。
// 在原 (user_id, is_current=TRUE) 条件上增加 service_tier 分区过滤：
//   - isResidential=true  → 只取该 user 的 residential current provision
//   - isResidential=false → 只取该 user 的非 residential（standard/premium 等）current provision
//
// 谓词 `(service_tier = 'residential') = $2` 把 service_tier 折叠成「是否 residential」二元分区，
// 让 residential 与 standard 两条 current 记录互不命中，从而不再互相覆盖。
// 原方法 GetCurrentByUserAnyStatus 的签名/SQL 保持不变（开关 false 仍走它，零回归）。
func (r *VPNProvisionRepository) GetCurrentByUserAndServicePartition(ctx context.Context, userID string, isResidential bool) (*models.VPNProvision, error) {
	return r.GetCurrentByUserAndFace(ctx, userID, models.PartitionFace(isResidential))
}

// GetCurrentByUserAndFace 按产品面（迁移 010 product_face）取该 user 的 current provision。
// ★2026-08-16 第三产品面 campaign：分区谓词从 (service_tier='residential')=$2 改为 product_face=$2，
// 否则 campaign 行（service_tier='standard'）会被 basic 分区命中并可能成为 basic 的 current（串面）。
// 所有 *AndServicePartition 方法都收口到 *AndFace（basic ↔ isResidential=false，residential ↔ true）。
func (r *VPNProvisionRepository) GetCurrentByUserAndFace(ctx context.Context, userID, face string) (*models.VPNProvision, error) {
	query := fmt.Sprintf(`
		SELECT %s FROM fulfillment.vpn_provisions
		WHERE user_id = $1 AND is_current = TRUE AND COALESCE(product_face, 'basic') = $2
		ORDER BY created_at DESC
		LIMIT 1
	`, vpnColumns)
	return r.scanOne(r.pool.QueryRow(ctx, query, userID, face))
}

// GetCurrentByUserFaceAndTier 在某产品面内【再按线路细分】取 current 行。
//
// ★为什么活动面需要这一层：迁移 003/036 后 promo 面可同时存在两条线路的账号——
// standard 落 otun users 表、residential 落 realm_users 表，是两个独立 otun 账号
// （设计 D3：两个 face 各自独立并存）。若沿用只按 face 取的 GetCurrentByUserAndFace，
// 用户先领 standard 活动券、再领 residential 活动券时会取到 standard 那行、复用它的
// otun_uuid，然后把一条 residential 开通请求发到一个只存在于 users 表的 uuid 上——
// otun 侧按 (auth_user_id, product_face) 在 realm_users 里查无此行，额度会落到错误的账号。
func (r *VPNProvisionRepository) GetCurrentByUserFaceAndTier(ctx context.Context, userID, face, serviceTier string) (*models.VPNProvision, error) {
	query := fmt.Sprintf(`
		SELECT %s FROM fulfillment.vpn_provisions
		WHERE user_id = $1 AND is_current = TRUE AND COALESCE(product_face, 'basic') = $2
		  AND COALESCE(service_tier, 'standard') = $3
		ORDER BY created_at DESC
		LIMIT 1
	`, vpnColumns)
	return r.scanOne(r.pool.QueryRow(ctx, query, userID, face, serviceTier))
}

// GetOtunUUIDByUserAndServicePartition 是 GetOtunUUIDByUser 的分区版（MULTI_SERVICE_ENABLED=true 才用）。
// 只在本次请求所属分区内找 otun_uuid——residential 请求不会取到 standard 记录的 otun_uuid，
// 从而 residential 走 CreateUser 新建独立 UUID（otun-manager 据此写独立 realm_users 表），
// 而不是复用 standard users.uuid。原方法 GetOtunUUIDByUser 不变。
func (r *VPNProvisionRepository) GetOtunUUIDByUserAndServicePartition(ctx context.Context, userID string, isResidential bool) (*string, error) {
	return r.GetOtunUUIDByUserAndFace(ctx, userID, models.PartitionFace(isResidential))
}

// GetOtunUUIDByUserAndFace 按产品面取可复用 otun_uuid（campaign 面永不复用其它面的 uuid，反之亦然）。
func (r *VPNProvisionRepository) GetOtunUUIDByUserAndFace(ctx context.Context, userID, face string) (*string, error) {
	query := `
		SELECT otun_uuid FROM fulfillment.vpn_provisions
		WHERE user_id = $1 AND otun_uuid IS NOT NULL AND otun_uuid != ''
		  AND COALESCE(product_face, 'basic') = $2
		ORDER BY created_at DESC
		LIMIT 1
	`
	var otunUUID *string
	err := r.pool.QueryRow(ctx, query, userID, face).Scan(&otunUUID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("get otun_uuid (partition): %w", err)
	}
	return otunUUID, nil
}

// GetBySubscriptionIDAndServicePartition 是 GetBySubscriptionID 的分区版（MULTI_SERVICE_ENABLED=true 才用）。
// residential 与 standard 通常是不同 subscription_id（多维订阅本就分 service_type），天然不冲突；
// 但若同一 subscription_id 跨 service_type，幂等短路也按分区取记录，避免 residential 请求命中
// standard 的 subscription provision 而被错误短路。原方法 GetBySubscriptionID 不变。
func (r *VPNProvisionRepository) GetBySubscriptionIDAndServicePartition(ctx context.Context, subscriptionID string, isResidential bool) (*models.VPNProvision, error) {
	return r.GetBySubscriptionIDAndFace(ctx, subscriptionID, models.PartitionFace(isResidential))
}

// GetBySubscriptionIDAndFace 按产品面取该订阅的 current provision（幂等短路用）。
func (r *VPNProvisionRepository) GetBySubscriptionIDAndFace(ctx context.Context, subscriptionID, face string) (*models.VPNProvision, error) {
	query := fmt.Sprintf(`
		SELECT %s FROM fulfillment.vpn_provisions
		WHERE subscription_id = $1 AND is_current = TRUE AND COALESCE(product_face, 'basic') = $2
		ORDER BY created_at DESC
		LIMIT 1
	`, vpnColumns)
	return r.scanOne(r.pool.QueryRow(ctx, query, subscriptionID, face))
}

func (r *VPNProvisionRepository) GetBySubscriptionID(ctx context.Context, subscriptionID string) (*models.VPNProvision, error) {
	query := fmt.Sprintf(`
		SELECT %s FROM fulfillment.vpn_provisions
		WHERE subscription_id = $1 AND is_current = TRUE`+notCampaign+`
		ORDER BY created_at DESC
		LIMIT 1
	`, vpnColumns)
	return r.scanOne(r.pool.QueryRow(ctx, query, subscriptionID))
}

func (r *VPNProvisionRepository) GetByUserAndBusinessType(ctx context.Context, userID, businessType string) (*models.VPNProvision, error) {
	query := fmt.Sprintf(`
		SELECT %s FROM fulfillment.vpn_provisions
		WHERE user_id = $1 AND business_type = $2`+notCampaign+`
		ORDER BY created_at DESC
		LIMIT 1
	`, vpnColumns)
	return r.scanOne(r.pool.QueryRow(ctx, query, userID, businessType))
}

func (r *VPNProvisionRepository) GetOtunUUIDByUser(ctx context.Context, userID string) (*string, error) {
	query := `
		SELECT otun_uuid FROM fulfillment.vpn_provisions
		WHERE user_id = $1 AND otun_uuid IS NOT NULL AND otun_uuid != ''` + notCampaign + `
		ORDER BY created_at DESC
		LIMIT 1
	`
	var otunUUID *string
	err := r.pool.QueryRow(ctx, query, userID).Scan(&otunUUID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("get otun_uuid: %w", err)
	}
	return otunUUID, nil
}

// Update operations

func (r *VPNProvisionRepository) Update(ctx context.Context, vp *models.VPNProvision) error {
	query := `
		UPDATE fulfillment.vpn_provisions SET
			subscription_id = $1, channel = $2,
			business_type = $3, service_tier = $4,
			otun_uuid = $5, plan_tier = $6, status = $7,
			traffic_limit = $8, traffic_used = $9, expire_at = $10,
			email = $11, device_id = $12, granted_by = $13, note = $14,
			is_current = $15, updated_at = NOW(),
			product_face = $17
		WHERE id = $16
	`
	// product_face 随 (plan_tier, service_tier) 重算：旧路径原地续期/升降级会改 service_tier
	//（standard↔residential），分区键必须跟着走（与旧谓词 (service_tier='residential') 语义等价）。
	vp.ProductFace = models.ProductFaceFor(vp.PlanTier, vp.ServiceTier)
	_, err := r.pool.Exec(ctx, query,
		vp.SubscriptionID, vp.Channel,
		vp.BusinessType, vp.ServiceTier,
		vp.OtunUUID, vp.PlanTier, vp.Status,
		vp.TrafficLimit, vp.TrafficUsed, vp.ExpireAt,
		vp.Email, vp.DeviceID, vp.GrantedBy, vp.Note,
		vp.IsCurrent, vp.ID, vp.ProductFace,
	)
	if err != nil {
		return fmt.Errorf("update vpn_provision: %w", err)
	}
	return nil
}

func (r *VPNProvisionRepository) UpdateStatus(ctx context.Context, id, status string) error {
	query := `UPDATE fulfillment.vpn_provisions SET status = $1, updated_at = NOW() WHERE id = $2`
	_, err := r.pool.Exec(ctx, query, status, id)
	if err != nil {
		return fmt.Errorf("update vpn_provision status: %w", err)
	}
	return nil
}

func (r *VPNProvisionRepository) MarkNotCurrent(ctx context.Context, id string) error {
	query := `UPDATE fulfillment.vpn_provisions SET is_current = FALSE, status = 'converted', updated_at = NOW() WHERE id = $1`
	_, err := r.pool.Exec(ctx, query, id)
	if err != nil {
		return fmt.Errorf("mark vpn_provision not current: %w", err)
	}
	return nil
}

// UpdateProjection 更新该面投影行的生效值（entitlement profiles，Sync 调用）：expire_at / traffic_limit /
// channel / active_class。★独立语句、不进 vpnColumns：active_class 列由迁移 009 引入，读路径不依赖它，
// 迁移未先跑只会让开关 true 下的这一条 UPDATE 报错，不影响其它查询。
func (r *VPNProvisionRepository) UpdateProjection(ctx context.Context, id string, expireAt *time.Time, trafficLimit int64, channel, activeClass string) error {
	query := `UPDATE fulfillment.vpn_provisions
		SET expire_at = $2, traffic_limit = $3, channel = $4, active_class = $5, updated_at = NOW()
		WHERE id = $1`
	_, err := r.pool.Exec(ctx, query, id, expireAt, trafficLimit, channel, activeClass)
	if err != nil {
		return fmt.Errorf("update vpn_provision projection: %w", err)
	}
	return nil
}

func (r *VPNProvisionRepository) UpdateTrafficUsed(ctx context.Context, id string, trafficUsed int64) error {
	query := `UPDATE fulfillment.vpn_provisions SET traffic_used = $1, updated_at = NOW() WHERE id = $2`
	_, err := r.pool.Exec(ctx, query, trafficUsed, id)
	if err != nil {
		return fmt.Errorf("update traffic_used: %w", err)
	}
	return nil
}

// ListByFilters queries vpn_provisions with optional filters
func (r *VPNProvisionRepository) ListByFilters(ctx context.Context, userID, businessType, status string) ([]*models.VPNProvision, error) {
	query := fmt.Sprintf(`
		SELECT %s FROM fulfillment.vpn_provisions
		WHERE ($1 = '' OR user_id::text = $1)
		  AND ($2 = '' OR business_type = $2)
		  AND ($3 = '' OR status = $3)
		ORDER BY created_at DESC
		LIMIT 100
	`, vpnColumns)
	rows, err := r.pool.Query(ctx, query, userID, businessType, status)
	if err != nil {
		return nil, fmt.Errorf("list vpn_provisions: %w", err)
	}
	defer rows.Close()
	return r.scanMany(rows)
}

// UpdateEmailByUserID 更新用户邮箱（邮箱绑定事件触发）
func (r *VPNProvisionRepository) UpdateEmailByUserID(ctx context.Context, userID, email string) error {
	query := `UPDATE fulfillment.vpn_provisions SET email = $2, updated_at = NOW() WHERE user_id = $1`
	_, err := r.pool.Exec(ctx, query, userID, email)
	if err != nil {
		return fmt.Errorf("update vpn_provision email: %w", err)
	}
	return nil
}

// IsExpired checks if a vpn provision has expired by time or traffic
func IsVPNExpired(vp *models.VPNProvision) bool {
	if vp.ExpireAt != nil && time.Now().After(*vp.ExpireAt) {
		return true
	}
	if vp.TrafficLimit > 0 && vp.TrafficUsed >= vp.TrafficLimit {
		return true
	}
	return false
}

func (r *VPNProvisionRepository) scanOne(row pgx.Row) (*models.VPNProvision, error) {
	vp := &models.VPNProvision{}
	err := row.Scan(
		&vp.ID, &vp.UserID, &vp.SubscriptionID, &vp.Channel,
		&vp.BusinessType, &vp.ServiceTier, &vp.OtunUUID, &vp.PlanTier, &vp.Status,
		&vp.TrafficLimit, &vp.TrafficUsed, &vp.ExpireAt,
		&vp.Email, &vp.DeviceID, &vp.GrantedBy, &vp.Note, &vp.IsCurrent,
		&vp.CreatedAt, &vp.UpdatedAt, &vp.ProductFace,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("scan vpn_provision: %w", err)
	}
	return vp, nil
}

func (r *VPNProvisionRepository) scanMany(rows pgx.Rows) ([]*models.VPNProvision, error) {
	var results []*models.VPNProvision
	for rows.Next() {
		vp := &models.VPNProvision{}
		err := rows.Scan(
			&vp.ID, &vp.UserID, &vp.SubscriptionID, &vp.Channel,
			&vp.BusinessType, &vp.ServiceTier, &vp.OtunUUID, &vp.PlanTier, &vp.Status,
			&vp.TrafficLimit, &vp.TrafficUsed, &vp.ExpireAt,
			&vp.Email, &vp.DeviceID, &vp.GrantedBy, &vp.Note, &vp.IsCurrent,
			&vp.CreatedAt, &vp.UpdatedAt, &vp.ProductFace,
		)
		if err != nil {
			return nil, fmt.Errorf("scan vpn_provision row: %w", err)
		}
		results = append(results, vp)
	}
	return results, rows.Err()
}
