package config

import (
	"fmt"
	"log"
	"os"
	"strconv"
	"time"
)

// 不安全的默认值列表 (生产环境不应使用)
var insecureDefaults = map[string]bool{
	"your-secret-key-change-in-production": true,
	"internal-secret":                      true,
	"internal-service-secret":              true,
	"":                                     true,
}

type Config struct {
	Server         ServerConfig
	Database       DatabaseConfig
	JWT            JWTConfig
	Hosting        HostingConfig
	Node           NodeConfig
	Encryption     EncryptionConfig
	Services       ServicesConfig
	InternalSecret string
	Trial          TrialConfig
	MultiService   MultiServiceConfig
	Entitlement    EntitlementConfig
	Campaign       CampaignConfig
}

// CampaignConfig 第三产品面 campaign（document/marketing-campaign/*）。
//   - StackHardMaxDays / StackHardMaxTrafficGB：全局叠加硬上限（与 campaign-service 同名 env 同源，
//     8 周 / 200G 默认），只用于 /vpn/all campaign 元素的 stack_limit 展示；真正的拒领在 campaign-service。
//   - RetentionDays：活动账号到期后保留期（契约 C5，默认 7 天）：期内 /vpn/all 仍下发 status=expired，
//     之后 cleanup 标记 is_current=false 并 deprovision otun 账号，不再下发。
type CampaignConfig struct {
	StackHardMaxDays      int
	StackHardMaxTrafficGB int
	RetentionDays         int
}

// EntitlementConfig 订阅/订购 profile 记账层开关（document/subscription-entitlement/*）。
// Enabled=false（默认）：旧履约路径不动 + 影子写记账表（profiles/entries）+ 响应不带新字段，不驱动 otun。
// Enabled=true：记账层接管 otun 同步（Resolve+Sync）、响应带 active_class/profiles[]、调度器启动。
// 回退只需关开关，记账表不删。
// SwitchLead：订阅到期前多久开始"桥接推送"（把 otun expire 提前推到订阅到期+桶天数），
// 必须 ≥ otun-manager UserCleanupService 间隔（1h），默认 65 分钟。
type EntitlementConfig struct {
	Enabled    bool
	SwitchLead time.Duration
}

type TrialConfig struct {
	Enabled       bool
	DurationHours int
	TrafficGB     int
}

// MultiServiceConfig 控制 residential 与 standard(basic/premium) 的 VPN 履约是否并行。
// Enabled=false（默认）：现状互斥——同一 user 任意 service_tier 共用一条 current provision，
//
//	后到的 tier 覆盖先到的（升级即覆盖）。行为与开关引入前逐字节一致。
//
// Enabled=true：standard 与 residential 按 service_tier 分区并行——同一 user 可同时持有
//
//	一条 standard provision 和一条 residential provision，互不覆盖；幂等/续期的 user 级查询
//	按本次请求的分区取记录，residential 不复用 standard 的 otun_uuid（走 CreateUser 新建）。
type MultiServiceConfig struct {
	Enabled bool
}

type ServerConfig struct {
	Port string
	Mode string
}

type DatabaseConfig struct {
	Host     string
	Port     string
	User     string
	Password string
	DBName   string
	Schema   string
	SSLMode  string
}

type JWTConfig struct {
	SecretKey string
}

type HostingConfig struct {
	ServiceURL    string
	AdminKey      string
	CloudProvider string
	DefaultRegion string
}

type NodeConfig struct {
	APIPort   int
	VlessPort int
	SSPort    int
}

type EncryptionConfig struct {
	Key string
}

type ServicesConfig struct {
	SubscriptionServiceURL string
	LicenseServiceURL      string
	OTunManagerURL         string
	OBoxManagerURL         string
	// PublicBaseURL 是【下发给客户端】的公网基址（B6：subscribe_url 等对外 URL 只准拼它，
	// 不准拼上面的内网互调地址）。portal 统一门户按路径分发到本服务。
	PublicBaseURL string
}

func Load() *Config {
	cfg := &Config{
		Server: ServerConfig{
			Port: getEnv("SERVER_PORT", "8014"),
			Mode: getEnv("GIN_MODE", "release"), // 默认为 release 模式
		},
		Database: DatabaseConfig{
			Host:     getEnv("DB_HOST", "localhost"),
			Port:     getEnv("DB_PORT", "5432"),
			User:     getEnv("DB_USER", "saas_user"),
			Password: getEnv("DB_PASSWORD", "saas_pass"),
			DBName:   getEnv("DB_NAME", "fulfillment_db"),
			Schema:   getEnv("DB_SCHEMA", "fulfillment"),
			SSLMode:  getEnv("DB_SSLMODE", "disable"),
		},
		JWT: JWTConfig{
			SecretKey: getEnv("JWT_SECRET_KEY", ""),
		},
		Hosting: HostingConfig{
			ServiceURL:    getEnv("HOSTING_SERVICE_URL", "http://localhost:8023"),
			AdminKey:      getEnv("HOSTING_ADMIN_KEY", ""),
			CloudProvider: getEnv("HOSTING_CLOUD_PROVIDER", "lightsail"),
			DefaultRegion: getEnv("HOSTING_DEFAULT_REGION", "us-east-1"),
		},
		Node: NodeConfig{
			APIPort:   getEnvInt("NODE_API_PORT", 8080),
			VlessPort: getEnvInt("NODE_VLESS_PORT", 443),
			SSPort:    getEnvInt("NODE_SS_PORT", 8388),
		},
		Encryption: EncryptionConfig{
			Key: getEnv("ENCRYPTION_KEY", ""),
		},
		Services: ServicesConfig{
			SubscriptionServiceURL: getEnv("SUBSCRIPTION_SERVICE_URL", "http://localhost:8012"),
			LicenseServiceURL:      getEnv("LICENSE_SERVICE_URL", "http://localhost:8004"),
			OTunManagerURL:         getEnv("OTUN_MANAGER_URL", "http://localhost:8022"),
			OBoxManagerURL:         getEnv("OBOX_MANAGER_URL", "http://localhost:8024"),
			PublicBaseURL:          getEnv("PUBLIC_BASE_URL", "https://portal.situstechnologies.com"),
		},
		InternalSecret: getEnv("INTERNAL_SECRET", ""),
		Trial: TrialConfig{
			Enabled:       getEnv("TRIAL_ENABLED", "true") == "true",
			DurationHours: getEnvInt("TRIAL_DURATION_HOURS", 1),
			TrafficGB:     getEnvInt("TRIAL_TRAFFIC_GB", 1),
		},
		MultiService: MultiServiceConfig{
			Enabled: getEnv("MULTI_SERVICE_ENABLED", "false") == "true",
		},
		Entitlement: EntitlementConfig{
			Enabled:    getEnv("ENTITLEMENT_PROFILES_ENABLED", "false") == "true",
			SwitchLead: time.Duration(getEnvInt("ENTITLEMENT_SWITCH_LEAD_MINUTES", 65)) * time.Minute,
		},
		Campaign: CampaignConfig{
			StackHardMaxDays:      getEnvInt("CAMPAIGN_STACK_HARD_MAX_WEEKS", 8) * 7,
			StackHardMaxTrafficGB: getEnvInt("CAMPAIGN_STACK_HARD_MAX_TRAFFIC_GB", 200),
			RetentionDays:         getEnvInt("CAMPAIGN_RETENTION_DAYS", 7),
		},
	}

	// 日志脱敏: 不记录敏感配置
	log.Printf("[config] Fulfillment Service loaded: port=%s db=%s/%s.%s hosting=%s trial_enabled=%v multi_service_enabled=%v entitlement_profiles_enabled=%v switch_lead=%v",
		cfg.Server.Port, cfg.Database.Host, cfg.Database.DBName, cfg.Database.Schema, cfg.Hosting.ServiceURL, cfg.Trial.Enabled, cfg.MultiService.Enabled, cfg.Entitlement.Enabled, cfg.Entitlement.SwitchLead)

	return cfg
}

// Validate 验证配置有效性，生产环境必须设置安全的密钥
func (c *Config) Validate() error {
	// 检查 JWT 密钥
	if insecureDefaults[c.JWT.SecretKey] {
		return fmt.Errorf("JWT_SECRET_KEY must be set to a secure value (current value is insecure or empty)")
	}
	if len(c.JWT.SecretKey) < 32 {
		return fmt.Errorf("JWT_SECRET_KEY must be at least 32 characters long")
	}

	// 检查内部服务密钥
	if insecureDefaults[c.InternalSecret] {
		return fmt.Errorf("INTERNAL_SECRET must be set to a secure value (current value is insecure or empty)")
	}
	if len(c.InternalSecret) < 32 {
		return fmt.Errorf("INTERNAL_SECRET must be at least 32 characters long")
	}

	return nil
}

func (c *DatabaseConfig) DSN() string {
	return "postgres://" + c.User + ":" + c.Password + "@" + c.Host + ":" + c.Port + "/" + c.DBName + "?sslmode=" + c.SSLMode
}

func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

func getEnvInt(key string, defaultValue int) int {
	if value := os.Getenv(key); value != "" {
		if intValue, err := strconv.Atoi(value); err == nil {
			return intValue
		}
	}
	return defaultValue
}
