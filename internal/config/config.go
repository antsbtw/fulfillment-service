package config

import (
	"log"
	"os"
	"strconv"
)

type Config struct {
	Server         ServerConfig
	Database       DatabaseConfig
	JWT            JWTConfig
	Hosting        HostingConfig
	Node           NodeConfig
	Encryption     EncryptionConfig
	Services       ServicesConfig
	InternalSecret string
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
}

func Load() *Config {
	cfg := &Config{
		Server: ServerConfig{
			Port: getEnv("SERVER_PORT", "8005"),
			Mode: getEnv("GIN_MODE", "debug"),
		},
		Database: DatabaseConfig{
			Host:     getEnv("DB_HOST", "localhost"),
			Port:     getEnv("DB_PORT", "5432"),
			User:     getEnv("DB_USER", "saas_user"),
			Password: getEnv("DB_PASSWORD", "saas_pass"),
			DBName:   getEnv("DB_NAME", "saas_db"),
			Schema:   getEnv("DB_SCHEMA", "fulfillment"),
			SSLMode:  getEnv("DB_SSLMODE", "disable"),
		},
		JWT: JWTConfig{
			SecretKey: getEnv("JWT_SECRET_KEY", "your-secret-key-change-in-production"),
		},
		Hosting: HostingConfig{
			ServiceURL:    getEnv("HOSTING_SERVICE_URL", "http://localhost:8010"),
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
			SubscriptionServiceURL: getEnv("SUBSCRIPTION_SERVICE_URL", "http://localhost:8003"),
			LicenseServiceURL:      getEnv("LICENSE_SERVICE_URL", "http://localhost:8004"),
		},
		InternalSecret: getEnv("INTERNAL_SECRET", "internal-service-secret"),
	}

	log.Printf("[config] Fulfillment Service loaded: port=%s db=%s/%s.%s hosting=%s",
		cfg.Server.Port, cfg.Database.Host, cfg.Database.DBName, cfg.Database.Schema, cfg.Hosting.ServiceURL)

	return cfg
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
