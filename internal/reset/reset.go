// Package reset implements bounded, idempotent cluster reset (ADR-4): it runs
// `kubeadm reset` via argv (no shell) and removes the authoritative kubeadm
// artifacts so the next boot re-converges from clean state. Removing
// /etc/kubernetes also shreds the cluster PKI and any bootstrap material on the
// node (ADR-2 reset shredding). Every step is bounded so reset can never hang
// (issue #4099-1).
//
// SECURITY: `RootPath` is operator-supplied via cluster_root_path. Because this
// package deletes paths derived from it, RootPath is validated (absolute, no
// traversal segments) inside Run, and a symlinked artifact is removed, never
// followed. The artifact directories are emptied and kept, and nothing below
// them that is a mount point is ever entered or removed (clear_linux.go): a
// volume kubeadm reset could not unmount keeps its data, and the reset reports
// it. Operators must NOT point RootPath at a directory whose only copy of their
// externally-managed PKI lives under it.
//
// HA-5: stacked-etcd CP detection + etcd orphan cleanup advisory (ADR-11 #5).
// On a stacked-etcd CP the advisory always appears: the full, actionable
// warning when this node's apiserver did not answer or kubeadm reset failed,
// otherwise a line asking the operator to verify the member is gone, because
// kubeadm reports a failed member removal only as a warning. The provider
// never runs etcdctl against a quorum; that is operator-owned.
package reset

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/kairos-io/provider-kubernetes/internal/kubeadm"
	"github.com/kairos-io/provider-kubernetes/internal/securefile"
)

const (
	defaultRootPath = "/"
	defaultTimeout  = 5 * time.Minute
	defaultRunDir   = "/run"
)

// Options configures a reset pass.
type Options struct {
	Runner    kubeadm.Runner
	RootPath  string
	CRISocket string        // optional; passed to `kubeadm reset --cri-socket` when set
	Timeout   time.Duration // bounded; zero defaults to defaultTimeout
	// NodeName is this node's name, surfaced in the etcd orphan warning (HA-5).
	// When empty the hostname is used in the advisory message.
	NodeName string
	// RunDir is the ephemeral directory swept for leftover kubeadm-*.yaml transient
	// configs from an interrupted join (HA-5). Empty defaults to /run.
	RunDir string
	// ControlPlaneReachable is an injectable bounded probe for HA-5. When nil the
	// cluster is assumed unreachable (conservative: always emit the warning).
	ControlPlaneReachable func(ctx context.Context) bool
}

// authoritativeArtifacts are the paths whose presence the actualstate prober
// treats as cluster membership. Removing them makes the next boot read
// "uninitialized" so the reconcile re-converges cleanly. Pre-condition: root is
// already validated (absolute, no traversal) by validateRoot.
func authoritativeArtifacts(root string) []string {
	return []string{
		filepath.Join(root, "etc", "kubernetes"), // confs + pki (shreds CA key)
		filepath.Join(root, "var", "lib", "kubelet"),
		filepath.Join(root, "var", "lib", "etcd"),
	}
}

// validateRoot enforces that RootPath is absolute, free of traversal segments,
// and cleaned. Empty -> "/" default (defense-in-depth: do not rely on callers).
func validateRoot(root string) (string, error) {
	if root == "" {
		root = defaultRootPath
	}
	if strings.Contains(root, "..") {
		return "", fmt.Errorf("reset: RootPath must not contain traversal segments: %q", root)
	}
	clean := filepath.Clean(root)
	if !filepath.IsAbs(clean) {
		return "", fmt.Errorf("reset: RootPath must be absolute, got %q", root)
	}
	return clean, nil
}

// ErrMountsKept is wrapped by Run's error when mount points were found under
// the artifact directories and left in place. The reset is then incomplete:
// whatever is mounted there is still in use, and the operator unmounts it and
// resets again.
var ErrMountsKept = errors.New("mount points left in place")

// maxReportedMounts bounds how many kept paths the error text names; the log
// names every one of them.
const maxReportedMounts = 5

// MountsKeptError reports the mount points a reset left in place. Its Error
// text names them, for the log and the subcommand's stderr. StatusSummary is
// what the status document carries: pod UIDs and volume names are long
// tokens that the status sanitizer redacts, so the summary gives the count
// and the remedy, and points at the log for the paths.
type MountsKeptError struct {
	Paths []string
}

func (e *MountsKeptError) Error() string {
	named := e.Paths
	more := ""
	if len(named) > maxReportedMounts {
		named = named[:maxReportedMounts]
		more = fmt.Sprintf(" and %d more", len(e.Paths)-maxReportedMounts)
	}
	return fmt.Sprintf("%v: %s%s; unmount them and run the reset again", ErrMountsKept, strings.Join(named, ", "), more)
}

// Unwrap makes errors.Is(err, ErrMountsKept) hold.
func (e *MountsKeptError) Unwrap() error { return ErrMountsKept }

// StatusSummary is the status document's message for this error.
func (e *MountsKeptError) StatusSummary() string {
	noun := "mount points"
	if len(e.Paths) == 1 {
		noun = "mount point"
	}
	return fmt.Sprintf("%d %s under the artifact directories left in place (the reset log names them); unmount them and run the reset again", len(e.Paths), noun)
}

// isStackedEtcdCP reports whether this node is a stacked-etcd control plane by
// checking for the presence of the etcd static-pod manifest or the etcd data dir
// under root. Pure file check, no I/O beyond stat (HA-5).
func isStackedEtcdCP(root string) bool {
	indicators := []string{
		filepath.Join(root, "etc", "kubernetes", "manifests", "etcd.yaml"),
		filepath.Join(root, "var", "lib", "etcd", "member"),
	}
	for _, p := range indicators {
		if _, err := os.Stat(p); err == nil {
			return true
		}
	}
	return false
}

// Run performs a bounded, idempotent reset. `kubeadm reset` failing (e.g. on an
// already-clean node) is non-fatal: artifact cleanup still runs so reset is
// idempotent. It returns the first artifact-removal error and, when a mount
// point under an artifact directory was left in place, a *MountsKeptError
// (which wraps ErrMountsKept), joined when both happened.
//
// HA-5: on a stacked-etcd CP the etcd member advisory follows kubeadm reset
// (etcdMemberAdvisory): a loud actionable warning naming what made the removal
// doubtful, or a line asking the operator to verify. No etcdctl is run. A sweep
// of RunDir for leftover transient kubeadm-*.yaml files also runs.
func Run(ctx context.Context, opts Options) error {
	root, err := validateRoot(opts.RootPath)
	if err != nil {
		return err
	}
	opts.RootPath = root

	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	runDir := opts.RunDir
	if runDir == "" {
		runDir = defaultRunDir
	}

	// HA-5: detect a stacked-etcd CP and probe it before anything is removed;
	// the advisory itself waits for kubeadm reset's result below.
	cpIsStacked := isStackedEtcdCP(root)
	clusterReachable := false
	if opts.ControlPlaneReachable != nil {
		clusterReachable = opts.ControlPlaneReachable(ctx)
	}

	args := []string{"reset", "-f", "--cleanup-tmp-dir"}
	if opts.CRISocket != "" {
		args = append(args, "--cri-socket", opts.CRISocket)
	}
	_, kubeadmErr := opts.Runner.Run(ctx, args...)
	if kubeadmErr != nil {
		// Non-fatal: proceed to artifact cleanup so reset is idempotent. The
		// error is already secret-sanitized by the Runner.
		logrus.Warnf("provider-kubernetes: kubeadm reset returned an error (continuing cleanup): %v", kubeadmErr)
	}
	if cpIsStacked {
		why := ""
		switch {
		case !clusterReachable:
			why = "this node's apiserver did not answer"
		case kubeadmErr != nil:
			why = "kubeadm reset failed"
		}
		etcdMemberAdvisory(opts.NodeName, why)
	}

	var firstErr error
	var kept []string
	for _, p := range authoritativeArtifacts(opts.RootPath) {
		k, err := clearArtifact(p)
		kept = append(kept, k...)
		if err != nil {
			logrus.Warnf("provider-kubernetes: failed to clear %s during reset: %v", p, err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}

	// HA-5: sweep RunDir for leftover transient kubeadm-*.yaml configs from an
	// interrupted join. Non-fatal; Shred logs failures at debug level.
	securefile.SweepRunDir(runDir)

	var errs []error
	if firstErr != nil {
		errs = append(errs, firstErr)
	}
	if len(kept) > 0 {
		errs = append(errs, &MountsKeptError{Paths: kept})
	}
	return errors.Join(errs...)
}

// etcdMemberAdvisory tells the operator what to do about this stacked-etcd
// member. kubeadm reset removes it through admin.conf, which names the
// controlPlaneEndpoint, and reports both "unable to fetch kubeadm-config" and
// "failed to remove etcd member" as warnings while still exiting 0. So a
// healthy local apiserver and a clean exit make the removal likely but not
// certain: the advisory never goes silent, it only changes from "act" to
// "verify". why names what made the removal doubtful; empty means nothing
// did. The detection reads the etcd manifest and data directory, which the
// first reset removes, so only the first run on a node can give it.
func etcdMemberAdvisory(nodeName, why string) {
	if nodeName == "" {
		if h, err := os.Hostname(); err == nil {
			nodeName = h
		} else {
			nodeName = "<node-name>"
		}
	}
	if why == "" {
		logrus.Warnf("provider-kubernetes: this was a stacked-etcd control-plane node. kubeadm reset removes its etcd member when it "+
			"can reach the cluster, but reports a failed removal only as a warning: from a surviving control-plane node, "+
			"verify with `etcdctl member list` that %s is gone, and `etcdctl member remove <id>` it if not.", nodeName)
		return
	}
	logrus.Warnf("provider-kubernetes: ATTENTION: this appears to be a stacked-etcd control-plane node and its etcd member may not have been removed (%s). "+
		"The etcd member for this node may remain registered in the etcd quorum after reset, which can cause quorum loss. "+
		"Operator action required from a surviving control-plane node: "+
		"(1) kubectl delete node %s  "+
		"(2) etcdctl member list  (identify this node's member ID)  "+
		"(3) etcdctl member remove <id>  "+
		"This provider does NOT run etcdctl. Proceeding with local cleanup.", why, nodeName)
}
