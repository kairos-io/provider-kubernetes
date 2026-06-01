package provider

import (
	"fmt"
	"math"
	"strings"
	"unicode"
)

const (
	// minClusterTokenLen is the hard minimum length for cluster_token (OQ-8).
	minClusterTokenLen = 16
	// recommendedEntropyBits is the warn-only entropy floor (OQ-8).
	recommendedEntropyBits = 128
)

// ValidateClusterToken enforces the OQ-8 policy. cluster_token is a LOW-TRUST
// correlation seed (ADR-2): it is not key material and not the join authenticator.
//   - empty/whitespace-only: hard error
//   - length < 16: hard error
//   - below ~128 bits estimated entropy: returns a non-empty warning (not an error)
//
// It never logs or echoes the token value itself. It is a pure function (no I/O),
// validated once at config ingest.
func ValidateClusterToken(token string) (warning string, err error) {
	if strings.TrimSpace(token) == "" {
		return "", fmt.Errorf("cluster_token must not be empty")
	}
	if len([]rune(token)) < minClusterTokenLen {
		return "", fmt.Errorf("cluster_token must be at least %d characters", minClusterTokenLen)
	}
	if estimateEntropyBits(token) < recommendedEntropyBits {
		return fmt.Sprintf(
			"cluster_token is below the recommended %d bits of entropy; it is treated as a low-trust correlation value only and must not be reused as a secret",
			recommendedEntropyBits,
		), nil
	}
	return "", nil
}

// estimateEntropyBits is the standard conservative estimate
// len * log2(observed charset size). It is an estimate, not a security guarantee.
func estimateEntropyBits(s string) float64 {
	var lower, upper, digit, other bool
	for _, r := range s {
		switch {
		case unicode.IsLower(r):
			lower = true
		case unicode.IsUpper(r):
			upper = true
		case unicode.IsDigit(r):
			digit = true
		default:
			other = true
		}
	}
	size := 0
	if lower {
		size += 26
	}
	if upper {
		size += 26
	}
	if digit {
		size += 10
	}
	if other {
		size += 32 // conservative bucket for punctuation/symbols
	}
	if size == 0 {
		return 0
	}
	return float64(len([]rune(s))) * math.Log2(float64(size))
}
