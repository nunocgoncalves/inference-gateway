package config

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the top-level gateway configuration.
type Config struct {
	Server      ServerConfig      `yaml:"server"`
	Database    DatabaseConfig    `yaml:"database"`
	Redis       RedisConfig       `yaml:"redis"`
	Auth        AuthConfig        `yaml:"auth"`
	RateLimits  RateLimitsConfig  `yaml:"rate_limits"`
	HealthCheck HealthCheckConfig `yaml:"health_check"`
	Registry    RegistryConfig    `yaml:"registry"`
	Logging     LoggingConfig     `yaml:"logging"`
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
	URL string `yaml:"url"`
}

type AuthConfig struct {
	AdminKey string `yaml:"admin_key"`
}

type RateLimitsConfig struct {
	DefaultRPM int `yaml:"default_rpm"`
	DefaultTPM int `yaml:"default_tpm"`
}

type HealthCheckConfig struct {
	Interval           time.Duration `yaml:"interval"`
	Timeout            time.Duration `yaml:"timeout"`
	HealthyThreshold   int           `yaml:"healthy_threshold"`
	UnhealthyThreshold int           `yaml:"unhealthy_threshold"`
}

type RegistryConfig struct {
	CacheRefreshInterval time.Duration `yaml:"cache_refresh_interval"`
}

type LoggingConfig struct {
	Level  string `yaml:"level"`
	Format string `yaml:"format"`
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
		RateLimits: RateLimitsConfig{
			DefaultRPM: 60,
			DefaultTPM: 100000,
		},
		HealthCheck: HealthCheckConfig{
			Interval:           10 * time.Second,
			Timeout:            5 * time.Second,
			HealthyThreshold:   3,
			UnhealthyThreshold: 1,
		},
		Registry: RegistryConfig{
			CacheRefreshInterval: 30 * time.Second,
		},
		Logging: LoggingConfig{
			Level:  "info",
			Format: "json",
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
	return nil
}
