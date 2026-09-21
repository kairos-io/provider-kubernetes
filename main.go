// Command agent-provider-kubernetes is a Kairos cluster provider that bootstraps
// upstream Kubernetes clusters with kubeadm. It is shipped inside a Kairos image;
// kairos-agent discovers it (agent-provider-* prefix) and invokes it over the
// clusterplugin event bus.
//
// Modes:
//   - default (no argv): run as a clusterplugin event handler (Provider +
//     EventClusterReset). This is what kairos-agent invokes at boot.
//   - "reconcile": run one bounded reconcile pass for a Cluster read from
//     --cluster-file. This is what the boot-time yip stage invokes.
//   - "mint-join": on a control-plane node, mint bounded-TTL join material and
//     print a ready-to-paste worker/controlplane cloud-config.
//   - "version": print the build version.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kairos-io/kairos-sdk/clusterplugin"
	"github.com/mudler/go-pluggable"
	yip "github.com/mudler/yip/pkg/schema"
	"github.com/sirupsen/logrus"
	"gopkg.in/yaml.v3"

	"github.com/kairos-io/provider-kubernetes/internal/clusterconfigdir"
	"github.com/kairos-io/provider-kubernetes/internal/imageimport"
	"github.com/kairos-io/provider-kubernetes/internal/kubeadm"
	"github.com/kairos-io/provider-kubernetes/internal/kubeadm/credential"
	"github.com/kairos-io/provider-kubernetes/internal/provider"
	"github.com/kairos-io/provider-kubernetes/internal/reset"
	"github.com/kairos-io/provider-kubernetes/internal/unitmigrate"
	"github.com/kairos-io/provider-kubernetes/version"
)

// eventInitProviderInfo is the event emitted by kairos-init (v0.14+) to discover
// the provider name and version during the image build phase.
// Defined locally because kairos-sdk v0.5.0 does not yet expose this constant;
// the value must match the string used by kairos-init / kairos-sdk >=v0.17.
const eventInitProviderInfo pluggable.EventType = "init.provider.info"

// providerInfoPayload mirrors the bus.ProviderInstalledVersionPayload from
// kairos-sdk >=v0.17. kairos-init unmarshals resp.Data into this type.
// Fields must remain JSON-tagged exactly as the upstream struct.
type providerInfoPayload struct {
	Provider string `json:"provider"`
	Version  string `json:"version"`
}

// handleProviderInfo responds to the init.provider.info probe emitted by kairos-init.
// kairos-init invokes our binary as:
//
//	agent-provider-kubernetes init.provider.info
//
// with a JSON Event on stdin ({"name":"init.provider.info","data":"null","file":""}).
// It expects EventResponse.Data to contain a JSON-serialized ProviderInstalledVersionPayload;
// an empty Data string causes "unexpected end of JSON input" in kairos-init's unmarshaller.
func handleProviderInfo(_ *pluggable.Event) pluggable.EventResponse {
	payload := providerInfoPayload{
		Provider: "kubernetes",
		Version:  version.Version,
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return pluggable.EventResponse{
			Error: fmt.Sprintf("marshal provider info: %v", err),
		}
	}
	return pluggable.EventResponse{
		State: "success",
		Data:  string(data),
	}
}

// wrapProvider is the D-3 / F-UKIBOOT fix's seam (S-D3-1, security review
// 2026-09-18). It composes ensure (production: clusterconfigdir.Ensure) with
// next (production: provider.Provider) into the single
// clusterplugin.ClusterProvider kairos-sdk@v0.5.0's ClusterPlugin.Run calls.
// That call happens inside clusterplugin.ClusterPlugin.onBoot
// (clusterplugin/plugin.go:50), strictly AFTER the boot payload is parsed and
// config.Cluster is confirmed non-nil, and strictly BEFORE the SDK's own
// OpenFile at plugin.go:59 that writes cluster.kairos.yaml with
// O_CREATE|O_WRONLY|O_TRUNC, 0600 and no MkdirAll -- the ENOENT that is this
// whole defect. ClusterPlugin.Run registers onBoot as the ONLY caller of the
// Provider field (on bus.EventBoot); EventClusterReset and
// init.provider.info are separate FactoryPlugins wired below with their own
// handlers, so this wrapper never runs during an image-build probe and never
// runs on a reset.
//
// next itself is untouched and stays side-effect-free (internal/provider.Provider's
// documented invariant): ensure runs first and, only when it reports
// rep.Withhold (an unsafe ancestor, an unsafe existing cloud-config
// directory, or an unsafe token-file target -- S-D3-2/S-D3-3/S-D3-4, see
// clusterconfigdir's package doc for the exact withhold set), the wrapper
// substitutes the inert YipConfig for next's real one so the SDK's
// unavoidable write carries no cluster_token and no Commands (S-D3-5). Every
// other outcome (including a merely-missing ancestor or "nothing to
// report") calls through to next unchanged.
//
// ensure is a parameter (rather than calling clusterconfigdir.Ensure
// directly) so wrapProvider's composition logic is unit-testable with a fake
// that returns a canned Report -- the real Ensure does Linux syscalls against
// "/usr/local" and must never run against the real host filesystem from a
// test (design principle 6, hardware-free testability).
func wrapProvider(next clusterplugin.ClusterProvider, ensure func(clusterplugin.Cluster) clusterconfigdir.Report) clusterplugin.ClusterProvider {
	return func(cluster clusterplugin.Cluster) yip.YipConfig {
		rep := ensure(cluster)
		if rep.Withhold {
			return provider.InertConfig(string(rep.Reason))
		}
		return next(cluster)
	}
}

func main() {
	args := os.Args[1:]
	if len(args) > 0 {
		switch args[0] {
		case "reconcile":
			os.Exit(runReconcile(args[1:]))
		case "reset":
			os.Exit(runReset(args[1:]))
		case "mint-join":
			os.Exit(runMintJoin(args[1:]))
		case "import-images":
			os.Exit(runImportImages(args[1:]))
		case "migrate-units":
			os.Exit(runMigrateUnits(args[1:]))
		case "version", "--version", "-v":
			fmt.Println(version.Version)
			return
		case "help", "--help", "-h":
			printUsage(os.Stdout)
			return
		}
	}

	logrus.Infof("starting agent-provider-kubernetes %s", version.Version)
	plugin := clusterplugin.ClusterPlugin{Provider: wrapProvider(provider.Provider, clusterconfigdir.Ensure)}
	if err := plugin.Run(
		pluggable.FactoryPlugin{
			EventType:     clusterplugin.EventClusterReset,
			PluginHandler: provider.HandleClusterReset,
		},
		pluggable.FactoryPlugin{
			EventType:     eventInitProviderInfo,
			PluginHandler: handleProviderInfo,
		},
	); err != nil {
		logrus.Fatal(err)
	}
}

// runReconcile executes one bounded reconcile pass. It is invoked from the
// boot-time yip stage Provider() emits; the stage runs at network.after so the
// network is up and CP reachability checks make sense.
func runReconcile(args []string) int {
	fs := flag.NewFlagSet("reconcile", flag.ContinueOnError)
	clusterFile := fs.String("cluster-file", provider.ClusterStatePath, "path to the serialized Cluster YAML (written 0600 on tmpfs by Provider)")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}

	logrus.Infof("provider-kubernetes reconcile %s: reading %s", version.Version, *clusterFile)

	data, err := os.ReadFile(*clusterFile)
	if err != nil {
		logrus.Errorf("read cluster file: %v", err)
		return 1
	}
	var cluster clusterplugin.Cluster
	if err := yaml.Unmarshal(data, &cluster); err != nil {
		logrus.Errorf("parse cluster file: %v", err)
		return 1
	}

	if err := provider.Run(context.Background(), cluster, provider.Options{Runner: kubeadm.DefaultRunner()}); err != nil {
		logrus.Errorf("reconcile: %v", err)
		return 1
	}
	logrus.Info("provider-kubernetes reconcile: done")
	return 0
}

// runMintJoin mints bounded-TTL join material on a control-plane node and prints a
// ready-to-paste cloud-config for a joining node (ADR-10: the CP mints, the
// operator delivers out-of-band; joining nodes never mint). It must run where the
// local admin credentials and cluster CA live (under --root-path). The credential
// values are printed to stdout by design and are never logged.
func runMintJoin(args []string) int {
	fs := flag.NewFlagSet("mint-join", flag.ContinueOnError)
	role := fs.String("role", "worker", "join role: worker or controlplane")
	ttl := fs.Duration("ttl", time.Hour, "bootstrap token TTL (must be > 0)")
	endpoint := fs.String("endpoint", "", "apiserver endpoint host:port (default: derived from admin.conf)")
	rootPath := fs.String("root-path", "/", "cluster_root_path (locates admin.conf and ca.crt)")
	clusterToken := fs.String("cluster-token", "", "cluster_token correlation value to embed (must match the control plane)")
	// HA-2: the joining CP's own advertise address. The minting CP cannot know the
	// joiner's IP; the operator passes it here or fills in the placeholder in the
	// rendered cloud-config before delivering it to the joining node.
	advertiseAddress := fs.String("advertise-address", "", "joining CP node's own API server advertise address (controlplane role only; HA-2)")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}

	roleNorm := strings.ToLower(strings.TrimSpace(*role))
	if roleNorm != "worker" && roleNorm != "controlplane" {
		fmt.Fprintf(os.Stderr, "role must be worker or controlplane, got %q\n", *role)
		return 2
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	minter := credential.Minter{Runner: kubeadm.DefaultRunner(), RootPath: *rootPath}
	jm, err := minter.MintJoinMaterial(ctx, roleNorm == "controlplane", *ttl)
	if err != nil {
		fmt.Fprintf(os.Stderr, "mint join material: %v\n", err)
		return 1
	}

	ep := strings.TrimSpace(*endpoint)
	if ep == "" {
		adminConf := filepath.Join(*rootPath, "etc", "kubernetes", "admin.conf")
		data, readErr := os.ReadFile(adminConf)
		if readErr != nil {
			fmt.Fprintf(os.Stderr, "derive endpoint: read %s: %v (pass --endpoint)\n", adminConf, readErr)
			return 1
		}
		ep, err = provider.EndpointFromKubeconfig(data)
		if err != nil {
			fmt.Fprintf(os.Stderr, "derive endpoint: %v (pass --endpoint)\n", err)
			return 1
		}
	}

	out, err := provider.RenderJoinCloudConfig(provider.JoinSnippet{
		Role:             roleNorm,
		Endpoint:         ep,
		Token:            jm.Token,
		CACertHashes:     jm.CACertHashes,
		CertificateKey:   jm.CertificateKey,
		ClusterToken:     *clusterToken,
		TTL:              ttl.String(),
		AdvertiseAddress: *advertiseAddress,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "render join cloud-config: %v\n", err)
		return 1
	}
	fmt.Print(out)
	return 0
}

// runReset performs a bounded cluster reset from a serialized Cluster YAML, exactly
// as HandleClusterReset does for the pluggable event. This subcommand is the e2e
// harness's entry point for the reset scenario (ADR-13 Tier-1 scenario 3); it is
// also a useful operator escape hatch. It reads the cluster_root_path from
// ProviderOptions and the optional CRI socket from the user config, then calls
// reset.Run -- no shell, no interpolation, bounded (design principle 1 / #4099-1).
func runReset(args []string) int {
	fs := flag.NewFlagSet("reset", flag.ContinueOnError)
	clusterFile := fs.String("cluster-file", provider.ClusterStatePath, "path to the serialized Cluster YAML")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}

	logrus.Infof("provider-kubernetes reset %s: reading %s", version.Version, *clusterFile)

	data, err := os.ReadFile(*clusterFile)
	if err != nil {
		logrus.Errorf("read cluster file: %v", err)
		return 1
	}
	var cluster clusterplugin.Cluster
	if err := yaml.Unmarshal(data, &cluster); err != nil {
		logrus.Errorf("parse cluster file: %v", err)
		return 1
	}

	rootPath := "/"
	if v := cluster.ProviderOptions["cluster_root_path"]; v != "" {
		rootPath = v
	}

	var criSocket string
	if uc, ucErr := provider.ParseUserConfig(cluster.Options); ucErr == nil {
		criSocket = uc.InitConfiguration.NodeRegistration.CRISocket
	}

	if err := reset.Run(context.Background(), reset.Options{
		Runner:    kubeadm.DefaultRunner(),
		RootPath:  rootPath,
		CRISocket: criSocket,
	}); err != nil {
		logrus.Errorf("cluster reset: %v", err)
		return 1
	}
	logrus.Info("provider-kubernetes reset: done")
	return 0
}

// parseImportImagesArgs validates import-images' argv per ADR-16-A2 decisions
// 2 and 10: no arguments, or exactly "--verify-only". It is a pure function so
// the CLI's usage contract is unit-testable without running Import. Any other
// argv (including the removed "--dir" flag) is a usage error.
func parseImportImagesArgs(args []string) (verifyOnly bool, err error) {
	switch len(args) {
	case 0:
		return false, nil
	case 1:
		if args[0] == "--verify-only" {
			return true, nil
		}
	}
	return false, fmt.Errorf("usage: agent-provider-kubernetes import-images [--verify-only]")
}

// runImportImages imports the pre-bundled control-plane image tarballs listed
// in images.lock into containerd's k8s.io namespace (ADR-16, revised by
// ADR-16-A2), so kubeadm init finds them locally and a first boot converges
// with no registry access. Invoked at boot by the
// provider-kubernetes-image-import.service oneshot, ordered before kubelet.
func runImportImages(args []string) int {
	verifyOnly, err := parseImportImagesArgs(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		printUsage(os.Stderr)
		return 2
	}
	// Bounded so the boot path can never hang (#4099-1); importing local tarballs
	// is fast, this is generous headroom for many/large images on slow disks.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	logrus.Infof("provider-kubernetes import-images %s: verifyOnly=%t", version.Version, verifyOnly)
	// Import's summary line carries the outcome and must stay the last line this
	// command logs (CI, the release gate and the e2e tests read it as such).
	res := imageimport.Import(ctx, kubeadm.CtrRunner(), verifyOnly)
	return imageimport.ExitCode(res.Outcome)
}

// parseMigrateUnitsArgs validates migrate-units' argv per ADR-19 U2 / S19-8:
// no arguments at all (the migrate unit must not read cloud-config, env, the
// bus or the network before or through this command). It is a pure function
// so the CLI's usage contract is unit-testable without running Migrate. Any
// argument is a usage error.
func parseMigrateUnitsArgs(args []string) error {
	if len(args) != 0 {
		return fmt.Errorf("usage: agent-provider-kubernetes migrate-units")
	}
	return nil
}

// runMigrateUnits removes the stale /etc/systemd/system copies of
// containerd.service, kubelet.service, provider-kubernetes-image-import.service
// and the kubelet 10-kubeadm.conf drop-in (ADR-19 U2 decision 3), but only
// when each is byte-identical to a blob this project has ever shipped at that
// path, and reloads systemd if a fragment or drop-in was removed. Invoked at
// boot by the image-only provider-kubernetes-unit-migrate.service oneshot,
// ordered before containerd, the image-import unit and kubelet.
func runMigrateUnits(args []string) int {
	if err := parseMigrateUnitsArgs(args); err != nil {
		fmt.Fprintln(os.Stderr, err)
		printUsage(os.Stderr)
		return 2
	}
	// Bounded so the boot path can never hang (#4099-1); ADR-19 S19-7's overall
	// deadline is 20s.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	logrus.Infof("provider-kubernetes migrate-units %s", version.Version)
	// Migrate's summary line carries the outcome and must stay the last line
	// this command logs (CI and the e2e tests read it as such).
	res := unitmigrate.Migrate(ctx, kubeadm.SystemctlRunner())
	return unitmigrate.ExitCode(res.Outcome)
}

func printUsage(w *os.File) {
	_, _ = fmt.Fprintf(w, `agent-provider-kubernetes %s

Usage:
  agent-provider-kubernetes                 run the Kairos clusterplugin event handler (default)
  agent-provider-kubernetes reconcile [...] run one bounded reconcile pass for a serialized Cluster
  agent-provider-kubernetes reset [...]     run a bounded cluster reset from a serialized Cluster
  agent-provider-kubernetes mint-join [...] mint join material on a CP and print a join cloud-config
  agent-provider-kubernetes import-images [--verify-only]
                                           import (or verify) the bundled control-plane image tarballs
  agent-provider-kubernetes migrate-units   remove stale /etc/systemd/system unit copies (ADR-19 U2)
  agent-provider-kubernetes version         print the build version

mint-join flags:
  --role worker|controlplane   join role (default worker)
  --ttl 1h                     bootstrap token TTL (must be > 0)
  --endpoint host:port         apiserver endpoint (default: derived from admin.conf)
  --root-path /                cluster_root_path locating admin.conf and ca.crt
  --cluster-token VALUE        cluster_token to embed (must match the control plane)
  --advertise-address IP       joining CP node's own advertise address (controlplane only; operator fills in if not known at mint time)

`, version.Version)
}
