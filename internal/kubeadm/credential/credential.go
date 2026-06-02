// Package credential handles control-plane-side minting of bounded-TTL join
// material and computation of the cluster CA SPKI pin. Per ADR-2 it never derives
// secrets from cluster_token (it uses kubeadm's own CSPRNG generators), and per
// ADR-10 it runs ONLY on a control-plane node that already holds local admin
// credentials; joining nodes never call it (there is no node-initiated mint RPC).
//
// Secret hygiene: token and certificate-key values are returned to the caller and
// never logged here. They are obtained from kubeadm stdout (not passed on argv),
// and the certificate key is written into a 0600 config file (ADR-2/OQ-7),
// never a command-line flag, so it never appears in the process table.
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
	"github.com/kairos-io/provider-kubernetes/internal/kubeadmconfig"
	"github.com/kairos-io/provider-kubernetes/internal/securefile"
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

// String implements fmt.Stringer with a redacted form, so an accidental
// log/printf of a JoinMaterial value never leaks the token or cert key.
// Callers that legitimately need the secret values must read the fields directly.
func (j JoinMaterial) String() string   { return "JoinMaterial{REDACTED}" }
func (j JoinMaterial) GoString() string { return "JoinMaterial{REDACTED}" }

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
	// RunDir is the directory for transient secret-bearing config files written
	// during UploadCerts (ADR-2/OQ-7: cert-key goes into a 0600 tmpfs file, never
	// on argv). Empty defaults to /run. MUST be a tmpfs mount in production.
	RunDir string
}

func (m Minter) adminConf() string {
	return filepath.Join(m.RootPath, "etc", "kubernetes", "admin.conf")
}

func (m Minter) runDir() string {
	if m.RunDir != "" {
		return m.RunDir
	}
	return "/run"
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

// UploadCerts re-uploads the cluster PKI to the kubeadm-certs Secret, encrypted
// under the freshly-minted certKey. It mirrors the init path (action.go:runInit):
// the cert-key is written into the InitConfiguration via --config (a 0600 tmpfs
// file) so it NEVER appears on argv (B1/B2). The transient config is shredded
// post-exec. The upstream 2h kubeadm-certs expiry is preserved: we pass no TTL-
// stripping flag (B3). The config also carries --kubeconfig so the call uses the
// local admin credentials (ADR-10).
//
// Command: kubeadm init phase upload-certs --upload-certs --config <0600-tmpfs>
//
// Note: kubeadm init phase upload-certs reads the certificateKey from the
// InitConfiguration in the --config file (confirmed: the existing runInit path
// at action.go:91-113 uses the same mechanism and the test asserts no --certificate-
// key flag appears on argv). No --certificate-key argv flag is used.
func (m Minter) UploadCerts(ctx context.Context, certKey string) error {
	if certKey == "" {
		return fmt.Errorf("upload-certs: certKey must not be empty")
	}

	// Build a minimal InitConfiguration carrying only the certificateKey. This is
	// the only field upload-certs reads from the config (it ignores all other
	// InitConfiguration fields when run as a standalone phase).
	initCfg := kubeadmconfig.NewInitConfiguration()
	initCfg.CertificateKey = certKey

	content, err := kubeadmconfig.Marshal(initCfg)
	if err != nil {
		return fmt.Errorf("upload-certs: marshal config: %w", err)
	}

	path, err := securefile.WriteTransient(m.runDir(), content)
	if err != nil {
		return fmt.Errorf("upload-certs: write transient config: %w", err)
	}
	defer securefile.Shred(path)

	// The cert-key is in the 0600 config file; it must never appear on argv (B1).
	// --upload-certs is the flag that triggers the actual upload (required).
	// --kubeconfig ensures we use the local admin credentials.
	if _, err := m.Runner.Run(ctx,
		"init", "phase", "upload-certs",
		"--upload-certs",
		"--config", path,
		"--kubeconfig", m.adminConf(),
	); err != nil {
		return fmt.Errorf("upload-certs: %w", err)
	}
	return nil
}

// MintJoinMaterial assembles fresh join material on a control-plane node (ADR-10
// N3 auto-propagation): a bounded-TTL bootstrap token, the cluster CA SPKI pin
// computed from the local ca.crt, and (for control-plane joins) a fresh
// certificate key. The operator/management plane surfaces this to a joining node
// out-of-band; this function only mints, it does not transmit. controlPlane=true
// includes the certificate key.
//
// KEYSTONE (ADR-11 #3): when controlPlane=true, UploadCerts is called atomically
// after GenerateCertificateKey so the freshly-minted cert-key is guaranteed to
// match what is stored in the kubeadm-certs Secret. A minted cert-key without a
// matching upload would cause CP-join to fail or use a stale key. The 2h kubeadm-
// certs expiry is preserved (B3; no TTL:0 / expiry-strip). The cert-key is never
// passed on argv (B1) and is not reused (B2: fresh per CP-join call).
func (m Minter) MintJoinMaterial(ctx context.Context, controlPlane bool, ttl time.Duration) (*JoinMaterial, error) {
	token, _, err := m.CreateToken(ctx, ttl)
	if err != nil {
		return nil, err
	}
	hash, err := m.CACertHashFromFile()
	if err != nil {
		return nil, err
	}
	jm := &JoinMaterial{Token: token, CACertHashes: []string{hash}}
	if controlPlane {
		key, err := m.GenerateCertificateKey(ctx)
		if err != nil {
			return nil, err
		}
		// KEYSTONE: upload-certs paired atomically with the freshly-minted cert-key
		// (ADR-11 #3 / B2/B3). The cert-key flows through UploadCerts via a 0600
		// config file, never on argv (B1).
		if err := m.UploadCerts(ctx, key); err != nil {
			return nil, fmt.Errorf("mint CP join material: %w", err)
		}
		jm.CertificateKey = key
	}
	return jm, nil
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
