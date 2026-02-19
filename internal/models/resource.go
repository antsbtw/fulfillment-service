package models

import (
	"time"
)

// Resource status constants (shared by hosting and vpn provisions)
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

// Resource type constants (legacy, used by adaptLegacyRequest)
const (
	ResourceTypeHostingNode = "hosting_node"
	ResourceTypeOTunNode    = "otun_node"
	ResourceTypeVPNUser     = "vpn_user"
)

// Cloud provider constants
const (
	ProviderLightsail    = "lightsail"
	ProviderDigitalOcean = "digitalocean"
)

// Region represents an available cloud region
type Region struct {
	Code      string
	Name      string
	Provider  string
	Available bool
	CreatedAt time.Time
	UpdatedAt time.Time
}

// ProvisionLog represents an operation log entry
type ProvisionLog struct {
	ID            string
	ProvisionID   string
	ProvisionType string // "hosting" or "vpn"
	Action        string
	Status        string
	Message       string
	Metadata      map[string]interface{}
	CreatedAt     time.Time
}
