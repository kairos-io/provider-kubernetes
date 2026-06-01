// Package credential handles control-plane-side minting of bounded-TTL join
// material and computation of the cluster CA SPKI pin. Per ADR-2 it never derives
// secrets from cluster_token (it uses kubeadm's own CSPRNG generators), and per
// ADR-10 it runs ONLY on a control-plane node that already holds local admin
// credentials; joining nodes never call it (there is no node-initiated mint RPC).
//
// Secret hygiene: token and certificate-key values are returned to the caller and
// never logged here. They are obtained from kubeadm stdout (not passed on argv),
// and the certificate key is intended to flow into a 0600 config file (ADR-2/OQ-7),
// not a command-line flag, so it never appears in the process table.
package credential

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kairos-io/provider-kubernetes/internal/kubeadm"
)

// JoinMaterial is transient, credential-bearing join input. It is never persisted
// by a joiner (OQ-7) and never logged (ADR-2).
type JoinMaterial struct {
	Token             string   // bootstrap token "id.secret"
	CACertHashes      []string // SPKI "sha256:..." pins
	CertificateKey    string   // control-plane joins only
	Endpoint          string   // controlPlaneEndpoint host:port
	DiscoveryFilePath string   // discovery kubeconfig path (externally-managed CP); alternative to token+hashes
}

// SPKIHashFromPEM computes the kubeadm-style Subject Public Key Info SHA-256 pin
// ("sha256:<hex>") of the (first) certificate in pemData. This is the value used
// for CACertHashes (ADR-2 CA pinning). Pure function.
func SPKIHashFromPEM(pemData []byte) (string, error) {
	block, _ := pem.Decode(pemData)
	if block == nil {
		return "", fmt.Errorf("no PEM block found")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return "", fmt.Errorf("parse certificate: %w", err)
	}
	spki, err := x509.MarshalPKIXPublicKey(cert.PublicKey)
	if err != nil {
		return "", fmt.Errorf("marshal public key: %w", err)
	}
	sum := sha256.Sum256(spki)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// Minter mints join material on a control-plane node, via the kubeadm binary
// through the shared Runner (ADR-1: argv, bounded by ctx). It requires local admin
// credentials, which exist only after init/control-plane-join.
type Minter struct {
	Runner   kubeadm.Runner
	RootPath string // cluster_root_path; locates admin.conf and the CA cert
}

func (m Minter) adminConf() string {
	return filepath.Join(m.RootPath, "etc", "kubernetes", "admin.conf")
}

// GenerateToken produces a fresh CSPRNG bootstrap token value offline
// (`kubeadm token generate`); it does not require a running cluster and is used to
// pre-seed a bounded-TTL token in the init configuration.
func (m Minter) GenerateToken(ctx context.Context) (string, error) {
	res, err := m.Runner.Run(ctx, "token", "generate")
	if err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	tok := strings.TrimSpace(res.Stdout)
	if tok == "" {
		return "", fmt.Errorf("kubeadm token generate produced no token")
	}
	return tok, nil
}

// CreateToken creates a fresh bounded-TTL bootstrap token on the cluster using the
// local admin credentials (control-plane-side mint). The token is returned via
// stdout, never passed on argv. ttl must be > 0 (ADR-2: never 0/non-expiring).
func (m Minter) CreateToken(ctx context.Context, ttl time.Duration) (token, ttlStr string, err error) {
	if ttl <= 0 {
		return "", "", fmt.Errorf("bootstrap token TTL must be greater than zero")
	}
	ttlStr = ttl.String()
	res, err := m.Runner.Run(ctx, "token", "create", "--ttl", ttlStr, "--kubeconfig", m.adminConf())
	if err != nil {
		return "", "", fmt.Errorf("create token: %w", err)
	}
	token = strings.TrimSpace(res.Stdout)
	if token == "" {
		return "", "", fmt.Errorf("kubeadm token create produced no token")
	}
	return token, ttlStr, nil
}

// GenerateCertificateKey produces a fresh control-plane certificate-encryption key
// (`kubeadm certs certificate-key`). The key is returned via stdout (never on
// argv); the caller places it into a 0600 config consumed by `kubeadm init
// --upload-certs`, keeping the upstream 2h kubeadm-certs expiry (ADR-2).
func (m Minter) GenerateCertificateKey(ctx context.Context) (string, error) {
	res, err := m.Runner.Run(ctx, "certs", "certificate-key")
	if err != nil {
		return "", fmt.Errorf("generate certificate key: %w", err)
	}
	key := strings.TrimSpace(res.Stdout)
	if key == "" {
		return "", fmt.Errorf("kubeadm certs certificate-key produced no key")
	}
	return key, nil
}

// CACertHashFromFile computes the SPKI pin of the cluster CA at the default path
// under RootPath (the Kairos-bootstrapped-CP case). For externally-managed control
// planes the hash/anchor is operator-supplied instead.
func (m Minter) CACertHashFromFile() (string, error) {
	path := filepath.Join(m.RootPath, "etc", "kubernetes", "pki", "ca.crt")
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read CA cert %s: %w", path, err)
	}
	return SPKIHashFromPEM(data)
}
