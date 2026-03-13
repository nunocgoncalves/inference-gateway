package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDefaults(t *testing.T) {
	cfg := defaults()
	assert.Equal(t, 8080, cfg.Server.Port)
	assert.Equal(t, 60, cfg.RateLimits.DefaultRPM)
	assert.Equal(t, 100000, cfg.RateLimits.DefaultTPM)
	assert.Equal(t, "info", cfg.Logging.Level)
	assert.Equal(t, "json", cfg.Logging.Format)
}

func TestLoad_EnvOnly(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://localhost:5432/test")
	t.Setenv("REDIS_URL", "redis://localhost:6379/0")
	t.Setenv("ADMIN_API_KEY", "admin-secret")

	cfg, err := Load("")
	require.NoError(t, err)
	assert.Equal(t, "postgres://localhost:5432/test", cfg.Database.URL)
	assert.Equal(t, "redis://localhost:6379/0", cfg.Redis.URL)
	assert.Equal(t, "admin-secret", cfg.Auth.AdminKey)
}

func TestLoad_YAMLFile(t *testing.T) {
	content := `
server:
  port: 9090
database:
  url: "${DATABASE_URL}"
  max_open_conns: 50
redis:
  url: "${REDIS_URL}"
logging:
  level: debug
  format: text
`
	dir := t.TempDir()
	path := filepath.Join(dir, "gateway.yaml")
	require.NoError(t, os.WriteFile(path, []byte(content), 0644))

	t.Setenv("DATABASE_URL", "postgres://localhost:5432/test")
	t.Setenv("REDIS_URL", "redis://localhost:6379/0")

	cfg, err := Load(path)
	require.NoError(t, err)
	assert.Equal(t, 9090, cfg.Server.Port)
	assert.Equal(t, 50, cfg.Database.MaxOpenConns)
	assert.Equal(t, "debug", cfg.Logging.Level)
	assert.Equal(t, "text", cfg.Logging.Format)
}

func TestLoad_EnvOverridesYAML(t *testing.T) {
	content := `
database:
  url: "postgres://yaml-host:5432/test"
redis:
  url: "redis://yaml-host:6379/0"
`
	dir := t.TempDir()
	path := filepath.Join(dir, "gateway.yaml")
	require.NoError(t, os.WriteFile(path, []byte(content), 0644))

	// Env vars should override YAML values.
	t.Setenv("DATABASE_URL", "postgres://env-host:5432/test")
	t.Setenv("REDIS_URL", "redis://env-host:6379/0")

	cfg, err := Load(path)
	require.NoError(t, err)
	assert.Equal(t, "postgres://env-host:5432/test", cfg.Database.URL)
	assert.Equal(t, "redis://env-host:6379/0", cfg.Redis.URL)
}

func TestLoad_MissingRequired(t *testing.T) {
	// No DATABASE_URL or REDIS_URL set.
	t.Setenv("DATABASE_URL", "")
	t.Setenv("REDIS_URL", "")

	_, err := Load("")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "database.url")
}

func TestLoad_PortEnvOverride(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://localhost:5432/test")
	t.Setenv("REDIS_URL", "redis://localhost:6379/0")
	t.Setenv("PORT", "3000")

	cfg, err := Load("")
	require.NoError(t, err)
	assert.Equal(t, 3000, cfg.Server.Port)
}
