package kubeadmconfig

import (
	"fmt"
	"strings"

	"sigs.k8s.io/yaml"
)

// Marshal renders the given kubeadm/kubelet documents into a single multi-document
// YAML string (documents separated by "---"), in the order provided. Each document
// must already carry its apiVersion/kind (use the New* constructors). The result is
// what we write to the kubeadm --config file; the kubeadm binary validates it.
func Marshal(docs ...any) (string, error) {
	var b strings.Builder
	for i, d := range docs {
		out, err := yaml.Marshal(d)
		if err != nil {
			return "", fmt.Errorf("marshal kubeadm document %d: %w", i, err)
		}
		if i > 0 {
			b.WriteString("---\n")
		}
		b.Write(out)
	}
	return b.String(), nil
}
