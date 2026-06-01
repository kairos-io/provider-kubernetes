package provider

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"strings"
	"testing"

	"github.com/kairos-io/provider-kubernetes/internal/kubeadm/credential"
)

func caPEM(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "ca"}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

func TestBuildJoinMaterialFileDiscovery(t *testing.T) {
	var uc UserConfig
	uc.JoinConfiguration.Discovery.File.KubeConfigPath = "/run/discovery.conf"
	jm, err := BuildJoinMaterial(Context{}, uc)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if jm == nil || jm.DiscoveryFilePath != "/run/discovery.conf" {
		t.Fatalf("expected file discovery material, got %+v", jm)
	}
}

func TestBuildJoinMaterialTokenExplicitHashes(t *testing.T) {
	var uc UserConfig
	uc.JoinConfiguration.Discovery.BootstrapToken.Token = "abcdef.0123456789abcdef"
	uc.JoinConfiguration.Discovery.BootstrapToken.CACertHashes = []string{"sha256:deadbeef"}
	jm, err := BuildJoinMaterial(Context{ControlPlaneHost: "10.0.0.1"}, uc)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if jm == nil || jm.Token == "" || len(jm.CACertHashes) != 1 {
		t.Fatalf("expected token material, got %+v", jm)
	}
}

func TestBuildJoinMaterialOQ9CrossValidate(t *testing.T) {
	pemStr := caPEM(t)
	derived, err := credential.SPKIHashFromPEM([]byte(pemStr))
	if err != nil {
		t.Fatal(err)
	}

	// Matching: explicit hash equals the hash derived from CACerts -> ok.
	var ok UserConfig
	ok.JoinConfiguration.Discovery.BootstrapToken.Token = "abcdef.0123456789abcdef"
	ok.JoinConfiguration.Discovery.BootstrapToken.CACertHashes = []string{derived}
	jm, err := BuildJoinMaterial(Context{CACerts: []string{pemStr}}, ok)
	if err != nil {
		t.Fatalf("expected match to succeed, got: %v", err)
	}
	if len(jm.CACertHashes) != 1 || jm.CACertHashes[0] != derived {
		t.Fatalf("expected derived hash, got %+v", jm.CACertHashes)
	}

	// Mismatch: explicit hash disagrees with CACerts -> hard error (OQ-9).
	var bad UserConfig
	bad.JoinConfiguration.Discovery.BootstrapToken.Token = "abcdef.0123456789abcdef"
	bad.JoinConfiguration.Discovery.BootstrapToken.CACertHashes = []string{"sha256:deadbeef"}
	if _, err := BuildJoinMaterial(Context{CACerts: []string{pemStr}}, bad); err == nil {
		t.Fatal("expected OQ-9 cross-validation mismatch to be a hard error")
	}
}

func TestBuildJoinMaterialOQ9NormalizesCase(t *testing.T) {
	pemStr := caPEM(t)
	derived, err := credential.SPKIHashFromPEM([]byte(pemStr))
	if err != nil {
		t.Fatal(err)
	}
	// Operator supplies the same hash but upper-cased / padded; must still match.
	var uc UserConfig
	uc.JoinConfiguration.Discovery.BootstrapToken.Token = "abcdef.0123456789abcdef"
	uc.JoinConfiguration.Discovery.BootstrapToken.CACertHashes = []string{"  " + strings.ToUpper(derived) + "  "}
	if _, err := BuildJoinMaterial(Context{CACerts: []string{pemStr}}, uc); err != nil {
		t.Fatalf("case/whitespace-only difference must still match, got: %v", err)
	}
}

func TestBuildJoinMaterialNoneReturnsNil(t *testing.T) {
	jm, err := BuildJoinMaterial(Context{}, UserConfig{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if jm != nil {
		t.Fatalf("expected nil material when nothing delivered, got %+v", jm)
	}
}
