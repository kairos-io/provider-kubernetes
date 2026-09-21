package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kairos-io/kairos-sdk/clusterplugin"
	"gopkg.in/yaml.v3"

	"github.com/kairos-io/provider-kubernetes/internal/provider"
)

// samples_test.go is the guard behind every claim the sample cloud-configs in
// samples/ make: that a shipped example is one the provider actually accepts.
// It runs the real ingest path -- the same NewContext / ParseUserConfig /
// BuildInput / BuildJoinMaterial functions the boot-time reconcile calls -- over
// every sample, so an example cannot drift away from the config the code parses
// without a test failure. It asserts on the typed result (role, endpoint, trust
// anchor), never on rendered strings (design principle 6).

// cloudConfig is the sliver of a Kairos cloud-config the provider reads. The
// rest of the document (install, users, stages) belongs to kairos-agent and is
// deliberately not modeled here.
type cloudConfig struct {
	Cluster *clusterplugin.Cluster `yaml:"cluster"`
}

func collectSamples(t *testing.T) []string {
	t.Helper()
	var files []string
	err := filepath.WalkDir("samples", func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || filepath.Ext(path) != ".yaml" {
			return nil
		}
		files = append(files, path)
		return nil
	})
	if err != nil {
		t.Fatalf("walk samples: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("no sample YAML files found under samples/")
	}
	return files
}

// TestSamplesParse asserts every sample cloud-config that carries a cluster
// block is accepted by the provider's own ingest path, and that each join
// sample carries a CA trust anchor (CA pinning is mandatory, ADR-2).
func TestSamplesParse(t *testing.T) {
	withCluster := 0
	for _, path := range collectSamples(t) {
		t.Run(path, func(t *testing.T) {
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			if !strings.HasPrefix(string(raw), "#cloud-config") {
				// Not a cloud-config (e.g. the Calico Installation CR, which is
				// a multi-document manifest). Every document still has to be
				// valid YAML, so decode the whole stream rather than just the
				// first document.
				dec := yaml.NewDecoder(bytes.NewReader(raw))
				for i := 0; ; i++ {
					var doc interface{}
					err := dec.Decode(&doc)
					if errors.Is(err, io.EOF) {
						if i == 0 {
							t.Fatal("file contains no YAML document")
						}
						return
					}
					if err != nil {
						t.Fatalf("document %d is not valid YAML: %v", i, err)
					}
				}
			}

			var cc cloudConfig
			if err := yaml.Unmarshal(raw, &cc); err != nil {
				t.Fatalf("parse cloud-config: %v", err)
			}
			if cc.Cluster == nil {
				t.Fatal("cloud-config sample has no cluster block")
			}
			withCluster++

			pctx, err := provider.NewContext(*cc.Cluster)
			if err != nil {
				t.Fatalf("NewContext: %v", err)
			}
			switch pctx.Role {
			case "init", "controlplane", "worker":
			default:
				t.Fatalf("role %q is not init/controlplane/worker", pctx.Role)
			}

			uc, err := provider.ParseUserConfig(pctx.UserOptions)
			if err != nil {
				t.Fatalf("ParseUserConfig: %v", err)
			}

			// resolvedVersion "" means "use the pinned kubernetesVersion", which
			// is what a sample declares; the runtime value comes from the bundled
			// kubeadm binary, which no test can detect.
			in, _, err := provider.BuildInput(pctx, uc, "")
			if err != nil {
				t.Fatalf("BuildInput: %v", err)
			}
			if in.ControlPlaneEndpoint == "" {
				t.Fatal("BuildInput produced an empty controlPlaneEndpoint")
			}

			if pctx.Role == "init" {
				return
			}

			// Joining roles must carry a trust anchor, and a control-plane join
			// must carry a certificate key.
			jm, err := provider.BuildJoinMaterial(pctx, uc)
			if err != nil {
				t.Fatalf("BuildJoinMaterial: %v", err)
			}
			if jm == nil {
				t.Fatal("join sample carries no join material")
			}
			if jm.DiscoveryFilePath == "" && len(jm.CACertHashes) == 0 {
				t.Fatal("join sample has a token but no CA trust anchor")
			}
			if pctx.Role == "controlplane" && jm.CertificateKey == "" {
				t.Fatal("control-plane join sample carries no certificateKey")
			}
		})
	}
	if withCluster == 0 {
		t.Fatal("no sample carried a cluster block")
	}
}
