// Package spiffe implements SPIFFE workload-identity verification for the
// inference gateway's supervisor mTLS path (HOR-398; ARCH-010/011).
//
// The gateway terminates native gRPC over HTTP/2 + mTLS for the workload
// listener. The client certificate's URI SAN is the authoritative caller
// identity; the gateway resolves scope from durable control-plane state
// rather than trusting agent-supplied scope (ARCH-004). Go's tls stack
// verifies the certificate chain against the configured ClientCAs
// (RequireAndVerifyClientCert); this package extracts and parses the SPIFFE
// URI SAN into a typed Identity.
//
// Trust domain is `iterabase.local` by default (configurable). The inference
// gateway accepts only supervisor callers (ARCH-010: model calls originate
// from the trusted supervisor):
//
//   - Supervisor (turn-scoped): spiffe://<td>/pools/<pool-uid>/workers/<pod-uid>
//
// This mirrors the control-plane's internal/spiffe package (HOR-392). The two
// repos cannot share an internal package, so the parsing is duplicated by
// design; the SPIFFE id format is the cross-service contract.
package spiffe

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// DefaultTrustDomain is the SPIFFE trust domain for the control plane.
const DefaultTrustDomain = "iterabase.local"

// Kind classifies a verified caller by its SPIFFE id path.
type Kind int

const (
	KindUnknown    Kind = iota
	KindSupervisor      // /pools/<pool>/workers/<worker>
)

func (k Kind) String() string {
	if k == KindSupervisor {
		return "supervisor"
	}
	return "unknown"
}

// Identity is a parsed, trust-domain-validated SPIFFE workload identity.
type Identity struct {
	SPIFFEID    string
	Kind        Kind
	TrustDomain string
	// Supervisor fields.
	PoolUID  string
	WorkerID string
}

// TrustConfig is the mTLS trust configuration for the workload TLS listener.
type TrustConfig struct {
	TrustDomain string
	ClientCAs   *x509.CertPool
}

// IdentityFromCerts extracts the SPIFFE identity from a peer certificate chain.
// The chain is assumed already verified by tls (RequireAndVerifyClientCert);
// this parses the URI SAN of the leaf cert and validates the trust domain +
// path structure. Returns an error if no URI SAN is present or the id is
// malformed — the caller fails closed (403).
func IdentityFromCerts(certs []*x509.Certificate, trustDomain string) (Identity, error) {
	if trustDomain == "" {
		trustDomain = DefaultTrustDomain
	}
	if len(certs) == 0 {
		return Identity{}, errors.New("spiffe: no peer certificate")
	}
	leaf := certs[0]
	if len(leaf.URIs) == 0 {
		return Identity{}, errors.New("spiffe: peer certificate has no URI SAN")
	}
	// A SPIFFE id is a single URI SAN; use the first.
	id, err := Parse(leaf.URIs[0].String(), trustDomain)
	if err != nil {
		return Identity{}, err
	}
	return id, nil
}

// IdentityFromConnState extracts the identity from a TLS connection state.
func IdentityFromConnState(cs *tls.ConnectionState, trustDomain string) (Identity, error) {
	if cs == nil {
		return Identity{}, errors.New("spiffe: no TLS connection state")
	}
	return IdentityFromCerts(cs.PeerCertificates, trustDomain)
}

// Parse validates a SPIFFE id string against the trust domain and parses its
// path into a typed Identity. Only supervisor ids are recognized; anything
// else returns KindUnknown (the inference gateway accepts only supervisor
// model callers — ARCH-010).
func Parse(spiffeID string, trustDomain string) (Identity, error) {
	if trustDomain == "" {
		trustDomain = DefaultTrustDomain
	}
	u, err := url.Parse(spiffeID)
	if err != nil {
		return Identity{}, fmt.Errorf("spiffe: parse id: %w", err)
	}
	if u.Scheme != "spiffe" {
		return Identity{}, fmt.Errorf("spiffe: id %q is not a spiffe:// URI", spiffeID)
	}
	if u.Host != trustDomain {
		return Identity{}, fmt.Errorf("spiffe: trust domain mismatch: id %q != %q", u.Host, trustDomain)
	}
	id := Identity{SPIFFEID: spiffeID, TrustDomain: u.Host}
	segments := splitPath(u.Path)
	if len(segments) == 4 && segments[0] == "pools" && segments[2] == "workers" {
		id.Kind = KindSupervisor
		id.PoolUID = segments[1]
		id.WorkerID = segments[3]
		return id, nil
	}
	return Identity{}, fmt.Errorf("spiffe: unrecognized id path %q", u.Path)
}

// splitPath splits a URL path into non-empty segments.
func splitPath(p string) []string {
	p = strings.Trim(p, "/")
	if p == "" {
		return nil
	}
	return strings.Split(p, "/")
}

// ServerTLSConfig builds the mTLS server TLS config for the workload listener:
// HTTP/2-only, require + verify client cert against ClientCAs. The gateway
// stamps the verified Identity from the request's TLS conn state in its
// workload-auth middleware.
func ServerTLSConfig(serverCert tls.Certificate, clientCAs *x509.CertPool) *tls.Config {
	return &tls.Config{
		Certificates: []tls.Certificate{serverCert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    clientCAs,
		MinVersion:   tls.VersionTLS12,
		NextProtos:   []string{"h2"}, // HTTP/2 only — no HTTP/1.1 fallback (ARCH-006)
	}
}
