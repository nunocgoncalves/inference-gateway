package config

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the top-level gateway configuration.
type Config struct {
	Server   ServerConfig   `yaml:"server"`
	Database DatabaseConfig `yaml:"database"`
	Redis    RedisConfig    `yaml:"redis"`
	Auth     AuthConfig     `yaml:"auth"`
	Snapshot SnapshotConfig `yaml:"snapshot"`
	Logging  LoggingConfig  `yaml:"logging"`
	Workload WorkloadConfig `yaml:"workload"`
}

type ServerConfig struct {
	Port         int           `yaml:"port"`
	ReadTimeout  time.Duration `yaml:"read_timeout"`
	WriteTimeout time.Duration `yaml:"write_timeout"`
	IdleTimeout  time.Duration `yaml:"idle_timeout"`
}

type DatabaseConfig struct {
	URL          string `yaml:"url"`
	MaxOpenConns int    `yaml:"max_open_conns"`
	MaxIdleConns int    `yaml:"max_idle_conns"`
}

type RedisConfig struct {
	URL    string `yaml:"url"`
	CAFile string `yaml:"ca_file"` // PEM CA cert to verify rediss:// (env REDIS_TLS_CA_FILE); empty = plaintext
}

type AuthConfig struct {
	AdminKey string `yaml:"admin_key"`
}

// SnapshotConfig configures the in-memory control-plane snapshot cache
// (catalog/api keys/capabilities/rate-limits). Redis is used only for the
// rate-limit counters.
type SnapshotConfig struct {
	RefreshInterval    time.Duration `yaml:"refresh_interval"`    // poll fallback (LISTEN/NOTIFY drives prompt updates)
	ReadinessStaleness time.Duration `yaml:"readiness_staleness"` // /readyz is unhealthy if the snapshot is older than this
}

type LoggingConfig struct {
	Level  string `yaml:"level"`
	Format string `yaml:"format"`
}

// WorkloadConfig configures the supervisor mTLS workload listener (HOR-398;
// ARCH-010/011). When Enabled, a second HTTP/2 mTLS server listens on Port
// and accepts only supervisor callers whose SPIFFE-bound workload identity +
// active durable turn context validate against control-plane state. The
// existing API-key listener is unaffected (separate policy path).
// ServerCertFile/ServerKeyFile are the gateway's serving cert; ClientCAFile is
// the workload trust bundle (SPIFFE CA) used to verify client certs.
type WorkloadConfig struct {
	Enabled        bool          `yaml:"enabled"`
	Port           int           `yaml:"port"`
	TrustDomain    string        `yaml:"trust_domain"`
	ServerCertFile string        `yaml:"server_cert_file"`
	ServerKeyFile  string        `yaml:"server_key_file"`
	ClientCAFile   string        `yaml:"client_ca_file"`
	ReadTimeout    time.Duration `yaml:"read_timeout"`
	WriteTimeout   time.Duration `yaml:"write_timeout"`
	IdleTimeout    time.Duration `yaml:"idle_timeout"`
}

// Load reads the configuration from a YAML file and applies environment
// variable overrides. If path is empty, only defaults and env vars are used.
func Load(path string) (*Config, error) {
	cfg := defaults()

	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("reading config file: %w", err)
		}
		// Expand environment variables in the YAML content.
		expanded := os.ExpandEnv(string(data))
		if err := yaml.Unmarshal([]byte(expanded), cfg); err != nil {
			return nil, fmt.Errorf("parsing config file: %w", err)
		}
	}

	applyEnvOverrides(cfg)

	if err := validate(cfg); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}

	return cfg, nil
}

func defaults() *Config {
	return &Config{
		Server: ServerConfig{
			Port:         8080,
			ReadTimeout:  30 * time.Second,
			WriteTimeout: 300 * time.Second,
			IdleTimeout:  120 * time.Second,
		},
		Database: DatabaseConfig{
			MaxOpenConns: 25,
			MaxIdleConns: 10,
		},
		Snapshot: SnapshotConfig{
			RefreshInterval:    30 * time.Second,
			ReadinessStaleness: 60 * time.Second,
		},
		Logging: LoggingConfig{
			Level:  "info",
			Format: "json",
		},
		Workload: WorkloadConfig{
			Port:         8443,
			TrustDomain:  "iterabase.local",
			ReadTimeout:  30 * time.Second,
			WriteTimeout: 300 * time.Second,
			IdleTimeout:  120 * time.Second,
		},
	}
}

// applyEnvOverrides lets critical values be set entirely via environment
// variables, which takes precedence over the YAML file.
func applyEnvOverrides(cfg *Config) {
	if v := os.Getenv("DATABASE_URL"); v != "" {
		cfg.Database.URL = v
	}
	if v := os.Getenv("REDIS_URL"); v != "" {
		cfg.Redis.URL = v
	}
	if v := os.Getenv("REDIS_TLS_CA_FILE"); v != "" {
		cfg.Redis.CAFile = v
	}
	if v := os.Getenv("ADMIN_API_KEY"); v != "" {
		cfg.Auth.AdminKey = v
	}
	if v := os.Getenv("PORT"); v != "" {
		var port int
		if _, err := fmt.Sscanf(v, "%d", &port); err == nil {
			cfg.Server.Port = port
		}
	}
	if v := os.Getenv("LOG_LEVEL"); v != "" {
		cfg.Logging.Level = v
	}
	if v := os.Getenv("LOG_FORMAT"); v != "" {
		cfg.Logging.Format = v
	}
	applyWorkloadEnvOverrides(cfg)
}

// applyWorkloadEnvOverrides applies the supervisor mTLS listener env overrides
// (HOR-398). Extracted from applyEnvOverrides to keep cyclomatic complexity
// under the lint ceiling.
func applyWorkloadEnvOverrides(cfg *Config) {
	if v := os.Getenv("WORKLOAD_ENABLED"); v != "" {
		cfg.Workload.Enabled = v == "true" || v == "1"
	}
	if v := os.Getenv("WORKLOAD_PORT"); v != "" {
		var p int
		if _, err := fmt.Sscanf(v, "%d", &p); err == nil {
			cfg.Workload.Port = p
		}
	}
	if v := os.Getenv("WORKLOAD_TRUST_DOMAIN"); v != "" {
		cfg.Workload.TrustDomain = v
	}
	if v := os.Getenv("WORKLOAD_SERVER_CERT_FILE"); v != "" {
		cfg.Workload.ServerCertFile = v
	}
	if v := os.Getenv("WORKLOAD_SERVER_KEY_FILE"); v != "" {
		cfg.Workload.ServerKeyFile = v
	}
	if v := os.Getenv("WORKLOAD_CLIENT_CA_FILE"); v != "" {
		cfg.Workload.ClientCAFile = v
	}
}

func validate(cfg *Config) error {
	if cfg.Database.URL == "" {
		return fmt.Errorf("database.url (or DATABASE_URL) is required")
	}
	if cfg.Redis.URL == "" {
		return fmt.Errorf("redis.url (or REDIS_URL) is required")
	}
	if cfg.Server.Port < 1 || cfg.Server.Port > 65535 {
		return fmt.Errorf("server.port must be between 1 and 65535")
	}
	if cfg.Workload.Enabled {
		if cfg.Workload.Port < 1 || cfg.Workload.Port > 65535 {
			return fmt.Errorf("workload.port must be between 1 and 65535")
		}
		if cfg.Workload.ServerCertFile == "" || cfg.Workload.ServerKeyFile == "" || cfg.Workload.ClientCAFile == "" {
			return fmt.Errorf("workload.server_cert_file, server_key_file, and client_ca_file are required when workload is enabled")
		}
		if cfg.Workload.Port == cfg.Server.Port {
			return fmt.Errorf("workload.port must differ from server.port")
		}
	}
	return nil
}
