package provider

import (
	"fmt"
	"sort"

	"github.com/kairos-io/provider-kubernetes/internal/kubeadm/credential"
)

// BuildJoinMaterial assembles operator-delivered join material for a joining node
// (ADR-10: the node consumes material delivered out-of-band; it never mints). It
// returns nil when no join material is supplied, so the caller fails loud rather
// than joining without a trust anchor (ADR-2: never UnsafeSkipCAVerification).
func BuildJoinMaterial(ctx Context, uc UserConfig) (*credential.JoinMaterial, error) {
	disc := uc.JoinConfiguration.Discovery

	// File discovery: CA is pinned by the (CA-embedded) discovery kubeconfig.
	if disc.File.KubeConfigPath != "" {
		return &credential.JoinMaterial{
			DiscoveryFilePath: disc.File.KubeConfigPath,
			CertificateKey:    uc.JoinConfiguration.ControlPlane.CertificateKey,
		}, nil
	}

	// Token discovery: must be CA-pinned. Resolve hashes from CACerts and/or the
	// explicit caCertHashes (OQ-9: cross-validate, fail on mismatch).
	if disc.BootstrapToken.Token != "" {
		hashes, err := resolveCACertHashes(ctx.CACerts, disc.BootstrapToken.CACertHashes)
		if err != nil {
			return nil, err
		}
		if len(hashes) == 0 {
			return nil, fmt.Errorf("join material has a token but no CA trust anchor (supply caCertHashes or ca_certs)")
		}
		return &credential.JoinMaterial{
			Token:          disc.BootstrapToken.Token,
			CACertHashes:   hashes,
			CertificateKey: uc.JoinConfiguration.ControlPlane.CertificateKey,
			Endpoint:       ctx.ControlPlaneHost,
		}, nil
	}

	// No anchor delivered.
	return nil, nil
}

// resolveCACertHashes implements OQ-9: if CA certs are supplied, derive their SPKI
// pins; if explicit hashes are also supplied, the two sets must agree exactly or it
// is a hard error. If only one source is present, that source is used.
func resolveCACertHashes(caCerts, explicit []string) ([]string, error) {
	var derived []string
	for i, pemStr := range caCerts {
		h, err := credential.SPKIHashFromPEM([]byte(pemStr))
		if err != nil {
			return nil, fmt.Errorf("derive CA hash from ca_certs[%d]: %w", i, err)
		}
		derived = append(derived, h)
	}

	switch {
	case len(derived) > 0 && len(explicit) > 0:
		if !hashSetsEqual(derived, explicit) {
			return nil, fmt.Errorf("CA trust mismatch: explicit caCertHashes do not match the hashes derived from ca_certs")
		}
		return derived, nil
	case len(derived) > 0:
		return derived, nil
	default:
		return explicit, nil
	}
}

func hashSetsEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	as := append([]string{}, a...)
	bs := append([]string{}, b...)
	sort.Strings(as)
	sort.Strings(bs)
	for i := range as {
		if as[i] != bs[i] {
			return false
		}
	}
	return true
}
