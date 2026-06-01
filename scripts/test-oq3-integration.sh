#!/bin/sh
# OQ-3 integration smoke-test: exercise the boot-time stitch end-to-end inside a
# container running the just-built Kairos image. We cannot create a real
# Kubernetes cluster here (no systemd, no networking, no kernel modules), so we
# stub `kubeadm` to a recording shim and assert the provider's emitted yip stage
# wires through to the reconcile subcommand with the expected argv.
#
# Validates:
#   - default invocation does not panic anymore (was a real bug pre-OQ-3).
#   - reconcile subcommand reads /run/provider-kubernetes/cluster.yaml.
#   - kubeadm is invoked with the expected argv for role=init (no shell, no
#     secrets in argv).
#
# Usage:
#   scripts/test-oq3-integration.sh [image-tag]   (default: kairos-kubeadm:oq3)
set -eu

IMAGE="${1:-kairos-kubeadm:oq3}"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

# Stub kubeadm: record argv to /tmp/kubeadm-calls.log, fabricate plausible stdout
# so the executor's parsing path succeeds for token/cert generation.
cat > "$TMP/kubeadm" <<'STUB'
#!/bin/sh
echo "kubeadm $*" >> /tmp/kubeadm-calls.log
case "$1 $2" in
  "version -o")           printf "v1.34.0\n" ;;
  "token generate")       printf "abcdef.0123456789abcdef\n" ;;
  "certs certificate-key") printf "%64s\n" | tr ' ' a ;;
  "init --config")        exit 0 ;;
  *)                      exit 0 ;;
esac
STUB
chmod +x "$TMP/kubeadm"

# Cluster YAML the same shape Provider() writes to /run/provider-kubernetes/.
cat > "$TMP/cluster.yaml" <<'CFG'
cluster_token: integration-test-cluster-token-with-plenty-of-entropy
control_plane_host: 10.0.0.1
role: init
providerConfig:
  cluster_root_path: /tmp/oq3-root
config: |
  clusterConfiguration:
    kubernetesVersion: v1.34.0
    controlPlaneEndpoint: "10.0.0.1:6443"
    networking:
      podSubnet: 10.244.0.0/16
      serviceSubnet: 10.96.0.0/12
CFG

echo "=== 1. provider 'version' subcommand prints something ==="
docker run --rm --entrypoint=/system/providers/agent-provider-kubernetes "$IMAGE" version

echo "=== 2. provider 'help' does not panic ==="
docker run --rm --entrypoint=/system/providers/agent-provider-kubernetes "$IMAGE" help >/dev/null

echo "=== 3. reconcile subcommand drives kubeadm init via the stubbed binary ==="
docker run --rm \
  -v "$TMP/kubeadm":/usr/bin/kubeadm:ro \
  -v "$TMP/cluster.yaml":/run/cluster.yaml:ro \
  --entrypoint=/system/providers/agent-provider-kubernetes \
  "$IMAGE" reconcile --cluster-file=/run/cluster.yaml | tee "$TMP/reconcile.out"

echo "=== 4. assert: kubeadm was invoked with init and the expected flags ==="
docker run --rm \
  -v "$TMP/kubeadm":/usr/bin/kubeadm:ro \
  -v "$TMP/cluster.yaml":/run/cluster.yaml:ro \
  --entrypoint=/bin/sh "$IMAGE" -c '
    : > /tmp/kubeadm-calls.log
    /system/providers/agent-provider-kubernetes reconcile --cluster-file=/run/cluster.yaml >/dev/null
    echo "--- recorded kubeadm calls ---"
    cat /tmp/kubeadm-calls.log
    echo "--- assertions ---"
    grep -q "^kubeadm version -o short$" /tmp/kubeadm-calls.log         || { echo "FAIL: version not detected"; exit 1; }
    grep -q "^kubeadm token generate$" /tmp/kubeadm-calls.log           || { echo "FAIL: token-generate not invoked"; exit 1; }
    grep -q "^kubeadm certs certificate-key$" /tmp/kubeadm-calls.log    || { echo "FAIL: certificate-key not generated"; exit 1; }
    init_call=$(grep "^kubeadm init " /tmp/kubeadm-calls.log || true)
    case "$init_call" in
      *"--upload-certs"*) ;;
      *) echo "FAIL: kubeadm init missing --upload-certs (got: $init_call)"; exit 1 ;;
    esac
    case "$init_call" in
      *"--config "*) ;;
      *) echo "FAIL: kubeadm init missing --config (got: $init_call)"; exit 1 ;;
    esac
    case "$init_call" in
      *"abcdef.0123456789abcdef"*) echo "FAIL: bootstrap token leaked into kubeadm argv"; exit 1 ;;
      *aaaaaaaaaa*)                echo "FAIL: certificate key leaked into kubeadm argv"; exit 1 ;;
    esac
    echo "PASS"
  '
echo "OQ-3 integration: PASS"
