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

func TestLoad_RedisTLSEnvOverride(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://localhost:5432/test")
	t.Setenv("REDIS_URL", "rediss://redis:6379/0")
	t.Setenv("REDIS_TLS_CA_FILE", "/etc/iterabase/internal-ca/ca.crt")

	cfg, err := Load("")
	require.NoError(t, err)
	assert.Equal(t, "/etc/iterabase/internal-ca/ca.crt", cfg.Redis.CAFile)
	assert.Equal(t, "rediss://redis:6379/0", cfg.Redis.URL)
}

func TestDefaults_Workload(t *testing.T) {
	cfg := defaults()
	assert.False(t, cfg.Workload.Enabled) // disabled by default
	assert.Equal(t, 8443, cfg.Workload.Port)
	assert.Equal(t, "iterabase.local", cfg.Workload.TrustDomain)
}

func TestLoad_WorkloadEnabledRequiresCerts(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://localhost:5432/test")
	t.Setenv("REDIS_URL", "redis://localhost:6379/0")
	t.Setenv("WORKLOAD_ENABLED", "true")
	// No cert paths -> validation error.
	_, err := Load("")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "workload.server_cert_file")
}

func TestLoad_WorkloadEnvOverride(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://localhost:5432/test")
	t.Setenv("REDIS_URL", "redis://localhost:6379/0")
	t.Setenv("WORKLOAD_ENABLED", "true")
	t.Setenv("WORKLOAD_PORT", "9443")
	t.Setenv("WORKLOAD_TRUST_DOMAIN", "cluster.example")
	t.Setenv("WORKLOAD_SERVER_CERT_FILE", "/certs/srv.crt")
	t.Setenv("WORKLOAD_SERVER_KEY_FILE", "/certs/srv.key")
	t.Setenv("WORKLOAD_CLIENT_CA_FILE", "/certs/ca.crt")

	cfg, err := Load("")
	require.NoError(t, err)
	assert.True(t, cfg.Workload.Enabled)
	assert.Equal(t, 9443, cfg.Workload.Port)
	assert.Equal(t, "cluster.example", cfg.Workload.TrustDomain)
	assert.Equal(t, "/certs/ca.crt", cfg.Workload.ClientCAFile)
}

func TestLoad_WorkloadPortMustDiffer(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://localhost:5432/test")
	t.Setenv("REDIS_URL", "redis://localhost:6379/0")
	t.Setenv("WORKLOAD_ENABLED", "true")
	t.Setenv("WORKLOAD_PORT", "8080") // same as server.port
	t.Setenv("WORKLOAD_SERVER_CERT_FILE", "/certs/srv.crt")
	t.Setenv("WORKLOAD_SERVER_KEY_FILE", "/certs/srv.key")
	t.Setenv("WORKLOAD_CLIENT_CA_FILE", "/certs/ca.crt")

	_, err := Load("")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "must differ from server.port")
}
