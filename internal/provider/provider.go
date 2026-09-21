// Package provider implements the Kairos clusterplugin entrypoint for
// provider-kubernetes: it translates a Cluster definition into a yip
// configuration.
//
// STATUS (foundation slice): inputs are parsed and validated, but bootstrap
// stages are not yet emitted. That lands with the kubeadm-flow and credential
// layers. Provider must always return promptly and never hang or panic (a
// provider that blocks stalls every later Kairos boot stage, issue #4099-1).
package provider

import (
	"github.com/kairos-io/kairos-sdk/clusterplugin"
	yip "github.com/mudler/yip/pkg/schema"
	"github.com/sirupsen/logrus"
	"gopkg.in/yaml.v3"

	"github.com/kairos-io/provider-kubernetes/internal/hostexec"
)

// ProviderBinaryPath is where the Kairos image installs the provider binary
// (Kairos discovery convention: agent-provider-* under /system/providers/).
// It is an alias of hostexec.ProviderBinaryPath (ADR-16-A2): hostexec is the
// single source of truth for this path, since it also anchors the bundle
// walk's device check.
const ProviderBinaryPath = hostexec.ProviderBinaryPath

// reconcileStageKey is the yip stage we emit into. We pick network.after so
// the bounded reconcile runs once the network is up (kubeadm join needs CP
// reachability) and never blocks earlier boot stages. OQ-3: revisit against the
// target kairos-agent if a more specific post-network stage becomes preferred.
const reconcileStageKey = "network.after"

// Provider is the clusterplugin.ClusterProvider implementation. It is the
// boot-time half of the contract: emit a yip config that, when executed,
// reconciles the node to the desired role. Provider itself is side-effect-free
// and returns promptly (#4099-1); the actual reconciliation runs later, inside
// the reconcile subcommand the emitted stage invokes.
// InertConfig returns the same inert, no-Stages configuration-error shape
// Provider itself falls back to on validation failure (see the
// "configuration error" / "serialization error" returns below), extended
// with a reason string. It carries no Stages and therefore no Commands and
// no cluster_token (S-D3-5, the D-3 / F-UKIBOOT security review, 2026-09-18):
// the main.go wrapper composed around Provider returns this instead of the
// real config when internal/clusterconfigdir finds the
// /usr/local/cloud-config write target unsafe, so the kairos-sdk clusterplugin's
// unavoidable O_CREATE|O_TRUNC write at that path carries nothing worth
// stealing. reason must be one of clusterconfigdir's closed Reason tokens --
// never raw cluster config, a token, or a path taken from an operator
// override.
func InertConfig(reason string) yip.YipConfig {
	return yip.YipConfig{Name: "provider-kubernetes (configuration error): " + reason}
}

func Provider(cluster clusterplugin.Cluster) yip.YipConfig {
	// Validate input early so an invalid cluster never reaches the boot stage.
	pctx, err := NewContext(cluster)
	if err != nil {
		logrus.Errorf("provider-kubernetes: invalid cluster configuration: %v", err)
		return yip.YipConfig{Name: "provider-kubernetes (configuration error)"}
	}
	if pctx.TokenWarning != "" {
		logrus.Warnf("provider-kubernetes: %s", pctx.TokenWarning)
	}

	// Serialize the Cluster into a tmpfs path the reconcile subcommand reads.
	// /run is tmpfs on every supported distro, so this file is wiped on reboot
	// (OQ-7: no bootstrap secrets persisted across reboots). YAML (not JSON)
	// because clusterplugin.Role's JSON unmarshal is broken in kairos-sdk v0.5.0
	// (it preserves literal quotes); the SDK's primary tags are yaml anyway.
	clusterYAML, err := yaml.Marshal(cluster)
	if err != nil {
		logrus.Errorf("provider-kubernetes: serialize cluster: %v", err)
		return yip.YipConfig{Name: "provider-kubernetes (serialization error)"}
	}

	logrus.Infof("provider-kubernetes: emitting %s stage for role %q", reconcileStageKey, pctx.Role)

	return yip.YipConfig{
		Name: "provider-kubernetes",
		Stages: map[string][]yip.Stage{
			reconcileStageKey: {{
				Name: "provider-kubernetes: write cluster state",
				Files: []yip.File{{
					Path:        ClusterStatePath,
					Permissions: 0o600,
					Owner:       0,
					Group:       0,
					Content:     string(clusterYAML),
				}},
			}, {
				Name: "provider-kubernetes: import pre-bundled images",
				Commands: []string{
					// ADR-16: import the pre-bundled control-plane images into
					// containerd BEFORE reconcile runs kubeadm init/join, so init
					// finds them locally (air-gap) and never pulls. yip runs stage
					// steps in array order, so this deterministically precedes the
					// reconcile step below -- the systemd oneshot races this stage
					// and cannot be relied on for ordering. No-op when nothing is
					// bundled; failures are logged but do not abort the boot (yip
					// continues to reconcile, which surfaces any missing image).
					ProviderBinaryPath + " import-images >>" + ImportLogPath + " 2>&1 || true",
				},
			}, {
				Name: "provider-kubernetes: reconcile",
				Commands: []string{
					// argv would be cleaner than a shell, but yip Commands run via
					// /bin/sh -c. Each token is a literal path/flag (no operator
					// data interpolated), so there is no shell-injection surface.
					// stdout/stderr is sanitized by the kubeadm Runner before it
					// reaches logs (ADR-2 secret hygiene).
					ProviderBinaryPath + " reconcile --cluster-file=" + ClusterStatePath +
						" >>" + ReconcileLogPath + " 2>&1",
				},
			}},
		},
	}
}
