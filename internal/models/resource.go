package models

import (
	"time"
)

// Resource status constants
const (
	StatusPending    = "pending"
	StatusCreating   = "creating"
	StatusRunning    = "running"
	StatusInstalling = "installing"
	StatusActive     = "active"
	StatusStopping   = "stopping"
	StatusStopped    = "stopped"
	StatusDeleted    = "deleted"
	StatusFailed     = "failed"
)

// Resource type constants
const (
	ResourceTypeHostingNode = "hosting_node"
	ResourceTypeOTunNode    = "otun_node"
)

// Cloud provider constants
const (
	ProviderLightsail   = "lightsail"
	ProviderDigitalOcean = "digitalocean"
)

// Resource represents a provisioned cloud resource
type Resource struct {
	ID             string
	SubscriptionID string
	UserID         string

	ResourceType string
	Provider     string
	Region       string
	InstanceID   *string

	PublicIP  *string
	PrivateIP *string

	// Node configuration
	APIPort   int
	APIKey    *string
	VlessPort int
	SSPort    int
	PublicKey *string
	ShortID   *string

	// SSH credentials
	SSHPrivateKey *string
	SSHKeyName    *string

	Status       string
	ErrorMessage *string

	PlanTier     string
	TrafficLimit int64
	TrafficUsed  int64

	CreatedAt time.Time
	UpdatedAt time.Time
	ReadyAt   *time.Time
	DeletedAt *time.Time
}

// Region represents an available cloud region
type Region struct {
	Code      string
	Name      string
	Provider  string
	Available bool
	CreatedAt time.Time
	UpdatedAt time.Time
}

// ResourceLog represents an operation log entry
type ResourceLog struct {
	ID         string
	ResourceID string
	Action     string
	Status     string
	Message    string
	Metadata   map[string]interface{}
	CreatedAt  time.Time
}
