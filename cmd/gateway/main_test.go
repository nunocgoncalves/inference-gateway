package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nunocgoncalves/inference-gateway/internal/config"
)

// TestRedisOptions_Plaintext: a redis:// URL with no CA file leaves TLSConfig
// nil — go-redis stays plaintext (kind/E2E backward-compat).
func TestRedisOptions_Plaintext(t *testing.T) {
	opts, err := redisOptions(config.RedisConfig{URL: "redis://localhost:6379/0"})
	require.NoError(t, err)
	assert.Nil(t, opts.TLSConfig, "plaintext redis:// must not enable TLS")
}

// TestRedisOptions_TLS_Verifies: a rediss:// URL + a CA file produces a
// TLSConfig whose RootCAs actually trust the server cert — proven by a real
// TLS handshake against a local TLS listener presenting that cert.
func TestRedisOptions_TLS_Verifies(t *testing.T) {
	certPEM, keyPEM, certPath := writeSelfSignedCert(t)

	addr, stop := startTLSListener(t, certPEM, keyPEM)
	defer stop()

	opts, err := redisOptions(config.RedisConfig{URL: "rediss://localhost:6379/0", CAFile: certPath})
	require.NoError(t, err)
	require.NotNil(t, opts.TLSConfig, "rediss:// + CA must enable TLS")

	// Clone + set ServerName (go-redis sets it from the URL host at dial time).
	dialCfg := opts.TLSConfig.Clone()
	dialCfg.ServerName = "localhost"

	conn, err := tls.Dial("tcp", addr, dialCfg)
	require.NoError(t, err, "TLS handshake should succeed with the internal CA")
	_ = conn.Close()
}

// TestRedisOptions_TLS_NoCARejects: a rediss:// URL with NO CA file yields
// go-redis's default empty TLSConfig (system roots), which does NOT trust the
// self-signed cert — the handshake fails. This proves the CA file is what makes
// verification work (not go-redis's default).
func TestRedisOptions_TLS_NoCARejects(t *testing.T) {
	certPEM, keyPEM, _ := writeSelfSignedCert(t)
	addr, stop := startTLSListener(t, certPEM, keyPEM)
	defer stop()

	opts, err := redisOptions(config.RedisConfig{URL: "rediss://localhost:6379/0"})
	require.NoError(t, err)
	require.NotNil(t, opts.TLSConfig, "rediss:// sets an (empty) TLSConfig")

	dialCfg := opts.TLSConfig.Clone()
	dialCfg.ServerName = "localhost"
	_, err = tls.Dial("tcp", addr, dialCfg)
	require.Error(t, err, "without the internal CA, the self-signed cert must not verify")
}

// TestRedisOptions_BadCA: a CA file with no cert is rejected loudly.
func TestRedisOptions_BadCA(t *testing.T) {
	dir := t.TempDir()
	badPath := dir + "/ca.pem"
	require.NoError(t, os.WriteFile(badPath, []byte("not a cert"), 0o600))

	_, err := redisOptions(config.RedisConfig{URL: "rediss://localhost:6379/0", CAFile: badPath})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no certificate")
}

// TestRedisOptions_MissingCAFile: a nonexistent CA file path is rejected.
func TestRedisOptions_MissingCAFile(t *testing.T) {
	_, err := redisOptions(config.RedisConfig{URL: "rediss://localhost:6379/0", CAFile: "/no/such/ca.pem"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "read redis TLS CA file")
}

// TestRedisOptions_BadURL: a malformed URL is rejected by ParseURL.
func TestRedisOptions_BadURL(t *testing.T) {
	_, err := redisOptions(config.RedisConfig{URL: "://bad"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parse redis URL")
}

// --- helpers ---

// writeSelfSignedCert generates a self-signed cert (SAN: localhost,
// 127.0.0.1) + key, writes the cert to a temp file, and returns
// (certPEM, keyPEM, certPath).
func writeSelfSignedCert(t *testing.T) (certPEM, keyPEM []byte, certPath string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "redis-test"},
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1)},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)

	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})

	certPath = t.TempDir() + "/ca.crt"
	require.NoError(t, os.WriteFile(certPath, certPEM, 0o600))
	return certPEM, keyPEM, certPath
}

// startTLSListener starts a TLS listener on 127.0.0.1:0 presenting the given
// cert, accepts a single connection, completes its server-side handshake, and
// returns the listener address + a stop func. The conn is held open until stop
// so the client's tls.Dial can complete cleanly.
func startTLSListener(t *testing.T, certPEM, keyPEM []byte) (addr string, stop func()) {
	t.Helper()
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	require.NoError(t, err)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	stopCh := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		// Force the server-side handshake to complete so the client's tls.Dial
		// can finish; ignore the error (the NoCA test expects it to fail).
		tlsConn := tls.Server(conn, &tls.Config{
			Certificates: []tls.Certificate{cert},
			MinVersion:   tls.VersionTLS12,
		})
		_ = tlsConn.Handshake()
		<-stopCh // hold the conn open until stop
	}()

	return ln.Addr().String(), func() {
		close(stopCh)
		_ = ln.Close()
		<-done
	}
}

// Compile-time guard: ensure redisOptions returns *redis.Options (catches
// signature drift).
var _ = func() *redis.Options { o, _ := redisOptions(config.RedisConfig{URL: "redis://x:1/0"}); return o }
