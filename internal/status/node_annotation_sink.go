// Package status -- NodeAnnotationSink (ADR-4-S, Layer 2 / Slice S3).
//
// SECURITY REVIEW BLOCK (required by ADR-4-S before S3 lands):
//
//	RBAC identity used: kubelet.conf (node identity, system:node:<name>) is
//	preferred; admin.conf (cluster-admin) is used only as a fallback when
//	kubelet.conf is absent. Both files are provisioned by kubeadm at init/join
//	time and are the SAME identity the rest of the provider already uses.
//	No new ServiceAccount, ClusterRole, ClusterRoleBinding, or RBAC object is
//	created or shipped by this provider. This sink piggybacks on an existing,
//	already-trusted credential.
//
//	Why no new privilege is introduced: the annotations written are OWN-Node
//	metadata under the prefix "provider-kubernetes.kairos.io/". The Node
//	authorizer grants a kubelet identity (system:node:<name>) the right to
//	patch its OWN Node object at the resource level; annotation writes are
//	covered by that grant. NOTE: the NodeRestriction admission plugin does NOT
//	inspect metadata.annotations -- it restricts only labels, taints,
//	spec.configSource, and ownerReferences on a node's own Node object. It does
//	not enforce any annotation allowlist or reserved-prefix rules on annotations.
//	The own-Node annotation write is authorized purely by the Node authorizer at
//	the resource level, and is left unconstrained on annotations by NodeRestriction.
//	Verified against release-1.34/1.35/1.36 NodeRestriction source. The admin.conf
//	identity (system:masters equivalent) trivially has the same permission.
//
//	No secret on argv: the only arguments passed to kubectl are the kubeconfig
//	path (a file path, not inline secret bytes), the node name, annotation
//	key=value pairs (all closed-enum, no tokens/keys/PEM), --overwrite, and
//	--kubeconfig. The annotation values are drawn from the closed Status schema
//	(phase, outcome, reason, terminal, last-action, updated-at, version) --
//	none of these fields can carry a secret by the schema's construction (see
//	status.go and the S2 sanitize guard). The free-text Message field is
//	intentionally excluded from the annotation set.
package status

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/kairos-io/provider-kubernetes/internal/kubeadm"
)

// AnnotationPrefix is the vendor-prefixed namespace for all provider status
// annotations written to the Node object. Keys are stable; removing or
// repurposing a key requires an ADR (same rule as the Status schema fields).
const AnnotationPrefix = "provider-kubernetes.kairos.io/"

// Annotation key suffixes under AnnotationPrefix. The full annotation key is
// AnnotationPrefix + <suffix>, e.g. "provider-kubernetes.kairos.io/phase".
const (
	AnnotationPhase      = AnnotationPrefix + "phase"
	AnnotationOutcome    = AnnotationPrefix + "outcome"
	AnnotationReason     = AnnotationPrefix + "reason"
	AnnotationTerminal   = AnnotationPrefix + "terminal"
	AnnotationLastAction = AnnotationPrefix + "last-action"
	AnnotationUpdatedAt  = AnnotationPrefix + "updated-at"
	AnnotationVersion    = AnnotationPrefix + "version"
)

// KubectlRunner is the minimal command-runner interface used by NodeAnnotationSink.
// It matches kubeadm.Runner exactly so production code can pass the same ExecRunner
// (or a kubectl-path variant) and tests can inject a fake. Defined here to avoid
// coupling the status package to the kubeadm package's concrete ExecRunner.
// The interface is intentionally narrow: a single Run method that takes argv
// and honors the context deadline.
type KubectlRunner interface {
	Run(ctx context.Context, args ...string) (kubeadm.Result, error)
}

// NodeResolver is a function that returns the node name for the annotation target.
// Production callers supply resolveNodeName (os.Hostname + toLower); tests inject
// a func that returns a known name or an error for the no-node-name no-op path.
type NodeResolver func() (string, error)

// KubeconfigResolver is a function that returns the path to the kubeconfig to
// use, or "" when none is available (pre-membership no-op path). Production
// callers supply resolveKubeconfig wrapping kubeconfigFor; tests inject a func.
type KubeconfigResolver func() string

// NodeAnnotationSink writes a small set of closed-enum Status fields as Node
// annotations under the prefix "provider-kubernetes.kairos.io/". It is the
// Layer-2 (post-membership) sink defined by ADR-4-S / Slice S3.
//
// Pre-membership safety: if the kubeconfig resolver returns "", or the node name
// cannot be determined, or the kubectl invocation fails, this sink is a NO-OP
// that logs at debug/warn level and returns without error. It NEVER blocks,
// NEVER errors out to the caller, and NEVER masks the real reconcile outcome
// (design principle 4 / #4099-1). It is strictly best-effort and additive.
//
// Annotation key set (stable, closed):
//
//	provider-kubernetes.kairos.io/phase
//	provider-kubernetes.kairos.io/outcome
//	provider-kubernetes.kairos.io/reason
//	provider-kubernetes.kairos.io/terminal
//	provider-kubernetes.kairos.io/last-action
//	provider-kubernetes.kairos.io/updated-at
//	provider-kubernetes.kairos.io/version
//
// Message is intentionally excluded: it is free-text with a sanitize guard
// but the closed-enum fields above carry all machine-readable signal. An
// operator who needs the full message reads the Layer-1 file.
type NodeAnnotationSink struct {
	// Runner executes kubectl. In production this is a kubeadm.ExecRunner whose
	// Path is set to "kubectl". Tests inject a fake that records calls.
	Runner KubectlRunner
	// ResolveNode returns the name of this node as the Kubernetes API knows it.
	// Returning an error causes the sink to no-op for this Record call.
	ResolveNode NodeResolver
	// ResolveKubeconfig returns the --kubeconfig path to use, or "" when no
	// usable kubeconfig exists (pre-membership). An empty return causes a no-op.
	ResolveKubeconfig KubeconfigResolver
	// AnnotateDeadline caps the single kubectl invocation. Defaults to 5s when zero.
	AnnotateDeadline time.Duration
}

// NewNodeAnnotationSink returns a NodeAnnotationSink wired for production use.
// rootPath is the cluster_root_path (the same value used throughout the provider;
// default "/"). nodeName is the Kubernetes node name from NodeRegistration (prefer
// this over os.Hostname for correctness -- Finding D); pass "" to fall back to
// os.Hostname() lowercased. The returned sink is safe to use immediately: if no
// kubeconfig exists at Record time the sink no-ops.
func NewNodeAnnotationSink(rootPath, nodeName string) *NodeAnnotationSink {
	return &NodeAnnotationSink{
		Runner:            &kubeadm.ExecRunner{Path: "kubectl"},
		ResolveNode:       MakeNodeResolver(nodeName),
		ResolveKubeconfig: func() string { return resolveKubeconfig(rootPath) },
	}
}

// MakeNodeResolver returns a NodeResolver that uses knownName when non-empty,
// falling back to os.Hostname() lowercased. This prefers the Kubernetes node
// name from NodeRegistration (kubeadm's authoritative name) over the OS hostname
// which may differ in some configurations (security Finding D / correctness).
// Exported so run.go can wire a late-bound closure over &nodeName without
// duplicating the fallback logic.
func MakeNodeResolver(knownName string) NodeResolver {
	return func() (string, error) {
		if knownName != "" {
			return knownName, nil
		}
		return resolveNodeName()
	}
}

// deadline returns the effective per-annotation deadline.
func (n *NodeAnnotationSink) deadline() time.Duration {
	if n.AnnotateDeadline > 0 {
		return n.AnnotateDeadline
	}
	return 5 * time.Second
}

// Record writes the Status fields as Node annotations. It is best-effort:
// any prerequisite absence or kubectl error is logged and swallowed.
func (n *NodeAnnotationSink) Record(ctx context.Context, s Status) {
	kc := n.ResolveKubeconfig()
	if kc == "" {
		logrus.Debugf("provider-kubernetes: node-annotation sink: no kubeconfig available; skipping (pre-membership no-op)")
		return
	}

	nodeName, err := n.ResolveNode()
	if err != nil || nodeName == "" {
		logrus.Debugf("provider-kubernetes: node-annotation sink: cannot determine node name (%v); skipping", err)
		return
	}

	// Defense-in-depth (security Finding A): guard that the node name and every
	// annotation token does not begin with '-'. A leading '-' could be parsed as
	// a flag by cobra/kubectl even though today's closed-enum values cannot start
	// with '-'. Skip the write and warn rather than risk a malformed argv.
	if strings.HasPrefix(nodeName, "-") {
		logrus.Warnf("provider-kubernetes: node-annotation sink: node name %q begins with '-'; skipping to avoid flag injection", nodeName)
		return
	}

	// Build annotation key=value pairs from the closed-enum fields only.
	// Message is intentionally excluded (see type-level doc).
	annotations := []string{
		fmt.Sprintf("%s=%s", AnnotationPhase, string(s.Phase)),
		fmt.Sprintf("%s=%s", AnnotationOutcome, string(s.Outcome)),
		fmt.Sprintf("%s=%s", AnnotationReason, string(s.Reason)),
		fmt.Sprintf("%s=%s", AnnotationTerminal, boolStr(s.Terminal)),
		fmt.Sprintf("%s=%s", AnnotationLastAction, s.LastAction),
		fmt.Sprintf("%s=%s", AnnotationUpdatedAt, s.UpdatedAt),
		fmt.Sprintf("%s=%s", AnnotationVersion, s.Version),
	}

	// Guard each annotation token: a key=value token beginning with '-' could be
	// misread as a flag. In practice the annotation keys all start with our prefix
	// and the values are closed-enum, but we enforce the invariant explicitly.
	for _, tok := range annotations {
		if strings.HasPrefix(tok, "-") {
			logrus.Warnf("provider-kubernetes: node-annotation sink: annotation token %q begins with '-'; skipping entire write to avoid flag injection", tok)
			return
		}
	}

	// Assemble the single kubectl annotate argv. All key=value pairs are passed
	// in one call so the annotation set is written atomically from the API
	// server's perspective (one PATCH). No secret appears on argv.
	//
	// Example argv (for documentation / test assertion reference):
	//   kubectl annotate node mynode \
	//     provider-kubernetes.kairos.io/phase=Converged \
	//     provider-kubernetes.kairos.io/outcome=success \
	//     ... \
	//     --overwrite \
	//     --kubeconfig /etc/kubernetes/kubelet.conf
	args := make([]string, 0, 3+len(annotations)+2)
	args = append(args, "annotate", "node", nodeName)
	args = append(args, annotations...)
	args = append(args, "--overwrite", "--kubeconfig", kc)

	rctx, cancel := context.WithTimeout(ctx, n.deadline())
	defer cancel()

	if _, err := n.Runner.Run(rctx, args...); err != nil {
		logrus.Warnf("provider-kubernetes: node-annotation sink: kubectl annotate: %v (swallowed)", err)
	}
}

// boolStr converts a bool to its string representation for annotation values.
func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// resolveNodeName returns this node's name as Kubernetes knows it: the hostname,
// lower-cased. This matches the convention kubeadm uses when naming nodes and is
// consistent with how runningKubeletVersionViaKubectl resolves the node name.
// Prefer makeNodeResolver with a known node name (from NodeRegistration) over
// this fallback when the kubeadm node name is available (security Finding D).
func resolveNodeName() (string, error) {
	host, err := os.Hostname()
	if err != nil {
		return "", fmt.Errorf("os.Hostname: %w", err)
	}
	if host == "" {
		return "", fmt.Errorf("hostname is empty")
	}
	return strings.ToLower(host), nil
}

// resolveKubeconfig returns the kubeconfig path to use for own-Node annotation
// under rootPath: kubelet.conf is preferred (node identity, least privilege);
// admin.conf is used only as a fallback when kubelet.conf is absent. "" is
// returned when neither exists (pre-membership). This is the same heuristic used
// by the upgrade probes (kubeconfigFor in provider/upgrade.go); duplicated here
// to keep the status package free of a provider package import.
//
// Precedence rationale (security Q2): kubelet.conf carries the node identity
// (system:node:<name>), which is sufficient for own-Node annotation writes and
// follows the principle of least privilege. admin.conf (system:masters equivalent)
// is an unnecessary blast-radius over-grant for this best-effort metadata write.
func resolveKubeconfig(rootPath string) string {
	if rootPath == "" {
		rootPath = "/"
	}
	for _, name := range []string{"kubelet.conf", "admin.conf"} {
		p := filepath.Join(rootPath, "etc", "kubernetes", name)
		if fileStatOK(p) {
			return p
		}
	}
	return ""
}

// fileStatOK reports whether path exists and is a regular file (not a directory).
// This is the same check used by actualstate.fileExists; duplicated here to keep
// the status package self-contained.
func fileStatOK(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}
