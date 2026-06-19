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
	"github.com/sirupsen/logrus"
	"gopkg.in/yaml.v3"

	"github.com/kairos-io/provider-kubernetes/internal/kubeadm"
	"github.com/kairos-io/provider-kubernetes/internal/kubeadm/credential"
	"github.com/kairos-io/provider-kubernetes/internal/provider"
	"github.com/kairos-io/provider-kubernetes/internal/reset"
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
		case "version", "--version", "-v":
			fmt.Println(version.Version)
			return
		case "help", "--help", "-h":
			printUsage(os.Stdout)
			return
		}
	}

	logrus.Infof("starting agent-provider-kubernetes %s", version.Version)
	plugin := clusterplugin.ClusterPlugin{Provider: provider.Provider}
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

	if err := provider.Run(context.Background(), cluster, provider.Options{Runner: kubeadm.ExecRunner{}}); err != nil {
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

	minter := credential.Minter{Runner: kubeadm.ExecRunner{}, RootPath: *rootPath}
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
		Runner:    kubeadm.ExecRunner{},
		RootPath:  rootPath,
		CRISocket: criSocket,
	}); err != nil {
		logrus.Errorf("cluster reset: %v", err)
		return 1
	}
	logrus.Info("provider-kubernetes reset: done")
	return 0
}

func printUsage(w *os.File) {
	_, _ = fmt.Fprintf(w, `agent-provider-kubernetes %s

Usage:
  agent-provider-kubernetes                 run the Kairos clusterplugin event handler (default)
  agent-provider-kubernetes reconcile [...] run one bounded reconcile pass for a serialized Cluster
  agent-provider-kubernetes reset [...]     run a bounded cluster reset from a serialized Cluster
  agent-provider-kubernetes mint-join [...] mint join material on a CP and print a join cloud-config
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
