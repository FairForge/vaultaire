// internal/devops/production.go
package devops

import (
	"os"
	"strings"
	"time"
)

// ProductionConfig holds production-specific configuration
type ProductionConfig struct {
	// Server settings
	Domain      string
	APIEndpoint string
	HTTPPort    int
	MetricsPort int

	// Database
	DatabaseHost     string
	DatabasePort     int
	DatabaseName     string
	DatabaseSSLMode  string
	DatabasePoolSize int

	// Redis
	RedisHost string
	RedisPort int
	RedisDB   int

	// Storage backends
	PrimaryBackend   string
	SecondaryBackend string

	// Security
	TLSEnabled         bool
	TLSCertPath        string
	TLSKeyPath         string
	CORSAllowedOrigins []string

	// Observability
	LogLevel        string
	TracingEnabled  bool
	TracingEndpoint string

	// Rate limiting
	RateLimitEnabled bool
	RateLimitRPS     int
	RateLimitBurst   int
}

// DefaultProductionConfigs returns environment-specific defaults
var DefaultProductionConfigs = map[string]ProductionConfig{
	EnvTypeDevelopment: {
		Domain:           "localhost",
		APIEndpoint:      "http://localhost:8080",
		HTTPPort:         8080,
		MetricsPort:      9090,
		DatabaseHost:     "localhost",
		DatabasePort:     5432,
		DatabaseName:     "vaultaire_dev",
		DatabaseSSLMode:  "disable",
		DatabasePoolSize: 10,
		RedisHost:        "localhost",
		RedisPort:        6379,
		RedisDB:          0,
		PrimaryBackend:   "local",
		SecondaryBackend: "",
		TLSEnabled:       false,
		LogLevel:         "debug",
		TracingEnabled:   false,
		RateLimitEnabled: false,
	},
	EnvTypeStaging: {
		Domain:           "staging.stored.ge",
		APIEndpoint:      "https://staging.stored.ge",
		HTTPPort:         8080,
		MetricsPort:      9090,
		DatabaseHost:     "staging-db.internal",
		DatabasePort:     5432,
		DatabaseName:     "vaultaire_staging",
		DatabaseSSLMode:  "require",
		DatabasePoolSize: 25,
		RedisHost:        "staging-redis.internal",
		RedisPort:        6379,
		RedisDB:          0,
		PrimaryBackend:   "quotaless",
		SecondaryBackend: "onedrive",
		TLSEnabled:       true,
		TLSCertPath:      "/etc/vaultaire/tls/cert.pem",
		TLSKeyPath:       "/etc/vaultaire/tls/key.pem",
		CORSAllowedOrigins: []string{
			"https://staging.stored.ge",
			"https://staging-dashboard.stored.ge",
		},
		LogLevel:         "info",
		TracingEnabled:   true,
		TracingEndpoint:  "http://jaeger.internal:14268/api/traces",
		RateLimitEnabled: true,
		RateLimitRPS:     100,
		RateLimitBurst:   200,
	},
	EnvTypeProduction: {
		Domain:           "stored.ge",
		APIEndpoint:      "https://api.stored.ge",
		HTTPPort:         8080,
		MetricsPort:      9090,
		DatabaseHost:     "prod-db.internal",
		DatabasePort:     5432,
		DatabaseName:     "vaultaire_prod",
		DatabaseSSLMode:  "require",
		DatabasePoolSize: 50,
		RedisHost:        "prod-redis.internal",
		RedisPort:        6379,
		RedisDB:          0,
		PrimaryBackend:   "quotaless",
		SecondaryBackend: "onedrive",
		TLSEnabled:       true,
		TLSCertPath:      "/etc/vaultaire/tls/cert.pem",
		TLSKeyPath:       "/etc/vaultaire/tls/key.pem",
		CORSAllowedOrigins: []string{
			"https://stored.ge",
			"https://www.stored.ge",
			"https://dashboard.stored.ge",
		},
		LogLevel:         "warn",
		TracingEnabled:   true,
		TracingEndpoint:  "http://jaeger.internal:14268/api/traces",
		RateLimitEnabled: true,
		RateLimitRPS:     1000,
		RateLimitBurst:   2000,
	},
}

// GetCurrentEnvironmentType returns the environment type from VAULTAIRE_ENV
func GetCurrentEnvironmentType() string {
	env := os.Getenv("VAULTAIRE_ENV")
	switch strings.ToLower(env) {
	case "production", "prod":
		return EnvTypeProduction
	case "staging", "stage":
		return EnvTypeStaging
	case "testing", "test":
		return EnvTypeTesting
	default:
		return EnvTypeDevelopment
	}
}

// GetProductionConfig returns the configuration for the current environment
func GetProductionConfig() ProductionConfig {
	envType := GetCurrentEnvironmentType()
	if config, ok := DefaultProductionConfigs[envType]; ok {
		return config
	}
	return DefaultProductionConfigs[EnvTypeDevelopment]
}

// IsProductionEnvironment returns true if running in production
func IsProductionEnvironment() bool {
	return GetCurrentEnvironmentType() == EnvTypeProduction
}

// IsDevelopmentEnvironment returns true if running in development
func IsDevelopmentEnvironment() bool {
	return GetCurrentEnvironmentType() == EnvTypeDevelopment
}

// ServerRole defines the role of a server in the infrastructure
type ServerRole string

const (
	RoleHub    ServerRole = "hub"
	RoleWorker ServerRole = "worker"
	RoleEdge   ServerRole = "edge"
	RoleDev    ServerRole = "dev"
)

// Server represents a server in the fleet
type Server struct {
	Name        string     `json:"name"`
	Role        ServerRole `json:"role"`
	Provider    string     `json:"provider"`
	Location    string     `json:"location"`
	PublicIP    string     `json:"public_ip,omitempty"`
	PrivateIP   string     `json:"private_ip,omitempty"`
	CPUCores    int        `json:"cpu_cores"`
	RAMGB       int        `json:"ram_gb"`
	StorageGB   int        `json:"storage_gb"`
	MonthlyCost float64    `json:"monthly_cost"`
	Services    []string   `json:"services"`
}

// ProductionInventory defines the production server fleet
var ProductionInventory = []Server{
	{
		Name:        "hub-nyc-1",
		Role:        RoleHub,
		Provider:    "ReliableSite",
		Location:    "NYC Metro",
		CPUCores:    12,
		RAMGB:       256,
		StorageGB:   8192,
		MonthlyCost: 79.00,
		Services: []string{
			"postgresql",
			"redis",
			"vaultaire-api",
			"vaultaire-worker",
			"prometheus",
			"grafana",
		},
	},
	{
		Name:        "worker-kc-1",
		Role:        RoleWorker,
		Provider:    "Terabit",
		Location:    "Kansas City",
		CPUCores:    8,
		RAMGB:       16,
		StorageGB:   200,
		MonthlyCost: 10.00,
		Services: []string{
			"vaultaire-worker",
			"node-exporter",
		},
	},
	{
		Name:        "worker-mtl-1",
		Role:        RoleWorker,
		Provider:    "Terabit",
		Location:    "Montreal",
		CPUCores:    8,
		RAMGB:       16,
		StorageGB:   200,
		MonthlyCost: 10.00,
		Services: []string{
			"vaultaire-worker",
			"node-exporter",
		},
	},
	{
		Name:        "worker-ams-1",
		Role:        RoleWorker,
		Provider:    "Terabit",
		Location:    "Amsterdam",
		CPUCores:    16,
		RAMGB:       32,
		StorageGB:   400,
		MonthlyCost: 10.00,
		Services: []string{
			"vaultaire-worker",
			"node-exporter",
		},
	},
	{
		Name:        "worker-bom-1",
		Role:        RoleWorker,
		Provider:    "HostDZire",
		Location:    "Mumbai",
		CPUCores:    16,
		RAMGB:       32,
		StorageGB:   240,
		MonthlyCost: 2.67,
		Services: []string{
			"vaultaire-worker",
			"node-exporter",
		},
	},
	{
		Name:        "dev-1",
		Role:        RoleDev,
		Provider:    "MaximumSettings",
		Location:    "Unknown",
		CPUCores:    8,
		RAMGB:       192,
		StorageGB:   4730,
		MonthlyCost: 1.83,
		Services: []string{
			"development",
			"testing",
		},
	},
}

// GetServersByRole returns all servers with a specific role
func GetServersByRole(role ServerRole) []Server {
	var servers []Server
	for _, s := range ProductionInventory {
		if s.Role == role {
			servers = append(servers, s)
		}
	}
	return servers
}

// GetTotalMonthlyCost calculates total infrastructure cost
func GetTotalMonthlyCost() float64 {
	var total float64
	for _, s := range ProductionInventory {
		total += s.MonthlyCost
	}
	return total
}

// GetTotalResources returns aggregate resource counts
func GetTotalResources() (cpuCores, ramGB, storageGB int) {
	for _, s := range ProductionInventory {
		cpuCores += s.CPUCores
		ramGB += s.RAMGB
		storageGB += s.StorageGB
	}
	return
}

// SetupProductionEnvironments creates the standard environment set
func SetupProductionEnvironments(manager *EnvironmentManager) error {
	// Development
	dev, err := manager.Create(&EnvironmentConfig{
		Name:        "development",
		Type:        EnvTypeDevelopment,
		Tier:        TierSecondary,
		Description: "Local development environment",
	})
	if err != nil {
		return err
	}
	_ = dev.SetVariable("DEBUG", "true")
	_ = dev.SetVariable("LOG_LEVEL", "debug")

	// Staging
	staging, err := manager.Create(&EnvironmentConfig{
		Name:        "staging",
		Type:        EnvTypeStaging,
		Tier:        TierSecondary,
		Description: "Pre-production staging environment",
	})
	if err != nil {
		return err
	}
	_ = staging.SetVariable("DEBUG", "false")
	_ = staging.SetVariable("LOG_LEVEL", "info")
	_ = staging.SetMaintenanceWindow(&MaintenanceWindow{
		Day:       time.Sunday,
		StartHour: 2,
		Duration:  4 * time.Hour,
	})

	// Production
	prod, err := manager.Create(&EnvironmentConfig{
		Name:        "production",
		Type:        EnvTypeProduction,
		Tier:        TierPrimary,
		Description: "Production environment - stored.ge",
	})
	if err != nil {
		return err
	}
	_ = prod.SetVariable("DEBUG", "false")
	_ = prod.SetVariable("LOG_LEVEL", "warn")
	_ = prod.SetMaintenanceWindow(&MaintenanceWindow{
		Day:       time.Sunday,
		StartHour: 4,
		Duration:  2 * time.Hour,
	})
	_ = prod.SetResourceLimits(&ResourceLimits{
		MaxCPU:     "12",
		MaxMemory:  "256Gi",
		MaxStorage: "8Ti",
	})

	// Set promotion paths
	_ = manager.SetPromotionPath("development", "staging")
	_ = manager.SetPromotionPath("staging", "production")

	return nil
}
