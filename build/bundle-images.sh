#!/bin/sh
# Pre-bundle the kubeadm control-plane images for an air-gap first boot, with
# best-effort upstream signature verification (ADR-16 P5). These tarballs are baked
# into the IMMUTABLE OS and `ctr import`ed at boot, bypassing any runtime CRI image
# policy, so provenance is established here at BUILD time.
#
# For each image kubeadm requires we:
#   1. resolve the floating tag to an immutable digest ONCE (crane digest),
#   2. cosign-verify THAT digest against the Kubernetes release identity, and
#   3. pull the SAME digest (crane pull ref@digest).
# Binding verify and pull to one digest closes the TOCTOU window.
#
# Verification policy (security-reviewed; registry.k8s.io signature coverage is
# empirically incomplete -- the kube-apiserver/controller-manager/proxy images for
# v1.34.0 carry NO discoverable cosign signature at either registry.k8s.io or the
# canonical us-central1 backing registry, while scheduler/coredns/pause/etcd do):
#   - signature verifies            -> verified=true.
#   - "no signatures found"         -> verified=false, reason "no-upstream-signature"
#                                      (digest-pinned from kubeadm's own list; the
#                                      image is still content-addressed, just not
#                                      signature-attested). Recorded in images.lock.
#   - any OTHER cosign failure      -> HARD FAIL (a present-but-invalid signature is
#     (present-but-invalid, network)  a tamper/trust signal; never ignored).
# Floor: pause, etcd, coredns MUST verify, AND at least one kube-* component MUST
# verify, or the build aborts (a fully-unsigned control-plane core is anomalous and
# must not pass silently).
#
# The images.lock records ref->digest + per-image verified flag/reason, so each
# released artifact attests exactly what it baked and at what trust level
# (feeds the CycloneDX SBOM attestation, ADR-15).
#
# Requires: kubeadm, crane, cosign on PATH (installed + pinned in the Dockerfile).
set -eu

: "${KUBERNETES_VERSION:?KUBERNETES_VERSION is required}"
IMAGE_REPOSITORY="${IMAGE_REPOSITORY:-registry.k8s.io}"
# Kubernetes release images are cosign keyless-signed by the krel promoter identity
# (kubernetes.io/docs/tasks/administer-cluster/verify-signed-artifacts).
COSIGN_IDENTITY="${COSIGN_IDENTITY:-krel-trust@k8s-releng-prod.iam.gserviceaccount.com}"
COSIGN_ISSUER="${COSIGN_ISSUER:-https://accounts.google.com}"
OUT_DIR="${OUT_DIR:-/images}"
MIN_IMAGES="${MIN_IMAGES:-5}"

mkdir -p "${OUT_DIR}"

kubeadm config images list \
  --kubernetes-version "${KUBERNETES_VERSION}" \
  --image-repository "${IMAGE_REPOSITORY}" > /tmp/imglist
test -s /tmp/imglist
grep -q '/pause:' /tmp/imglist

entries=""
n=0
verified_count=0
pause_ok=0
etcd_ok=0
coredns_ok=0
kube_any_ok=0

while IFS= read -r ref; do
  [ -n "${ref}" ] || continue

  # 1. resolve tag -> digest once; everything after is content-addressed.
  digest="$(crane digest "${ref}")"
  case "${digest}" in
    sha256:*) : ;;
    *) echo "FATAL: bad digest for ${ref}: '${digest}'" >&2; exit 1 ;;
  esac

  # 2. verify the DIGEST (not the tag) against the Kubernetes release identity.
  set +e
  out="$(cosign verify "${ref}@${digest}" \
    --certificate-identity "${COSIGN_IDENTITY}" \
    --certificate-oidc-issuer "${COSIGN_ISSUER}" 2>&1)"
  rc=$?
  set -e
  # reason is a CLOSED enum ("" or "no-upstream-signature"); ref/digest are
  # registry refs / sha256 hex. None contain JSON metacharacters, so the manual
  # printf JSON below is well-formed by construction (keep this invariant: any new
  # reason value must remain quote/backslash-free).
  if [ "${rc}" -eq 0 ]; then
    verified=true
    reason=""
    verified_count=$((verified_count + 1))
    echo "verified ${ref}@${digest}"
  elif printf '%s' "${out}" | grep -q "no signatures found"; then
    # Proven-unsigned at the registry: acceptable, recorded. NOT a verify failure.
    verified=false
    reason="no-upstream-signature"
    echo "WARN: no upstream signature for ${ref}@${digest}; digest-pinned only"
  else
    # Present-but-invalid signature, or a real error: never ignore it.
    echo "FATAL: cosign verify failed for ${ref}@${digest} (not 'no signatures found'):" >&2
    printf '%s\n' "${out}" >&2
    exit 1
  fi

  # Track the floor with anchored matches (a floor flag is set ONLY on an
  # affirmative verified==true, never on a skipped/timed-out image -- see B1c/R2).
  case "${ref}" in
    */pause:*) [ "${verified}" = true ] && pause_ok=1 ;;
    */etcd:*) [ "${verified}" = true ] && etcd_ok=1 ;;
    */coredns/coredns:*) [ "${verified}" = true ] && coredns_ok=1 ;;
    */kube-apiserver:* | */kube-controller-manager:* | */kube-scheduler:* | */kube-proxy:*) \
      [ "${verified}" = true ] && kube_any_ok=1 ;;
  esac

  # 3. pull the SAME digest we just resolved+checked.
  f="${OUT_DIR}/$(echo "${ref}" | tr '/:' '__').tar"
  crane pull "${ref}@${digest}" "${f}"
  tarball="$(basename "${f}")"

  entry="$(printf '    {"ref": "%s", "digest": "%s", "tarball": "%s", "verified": %s, "verifyReason": "%s"}' \
    "${ref}" "${digest}" "${tarball}" "${verified}" "${reason}")"
  if [ -z "${entries}" ]; then
    entries="${entry}"
  else
    entries="$(printf '%s,\n%s' "${entries}" "${entry}")"
  fi
  n=$((n + 1))
  echo "bundled ${ref}@${digest} -> ${tarball} (verified=${verified})"
done < /tmp/imglist

# --- Floor / sanity assertions (ADR-16 P5 B1c, B6) ---
if [ "${n}" -lt "${MIN_IMAGES}" ]; then
  echo "FATAL: expected >= ${MIN_IMAGES} control-plane images, bundled ${n}" >&2
  exit 1
fi
if [ "${pause_ok}" -ne 1 ] || [ "${etcd_ok}" -ne 1 ] || [ "${coredns_ok}" -ne 1 ]; then
  echo "FATAL: signature floor not met -- pause/etcd/coredns MUST be signature-verified (pause=${pause_ok} etcd=${etcd_ok} coredns=${coredns_ok})" >&2
  exit 1
fi
if [ "${kube_any_ok}" -ne 1 ]; then
  echo "FATAL: no kube-* control-plane image was signature-verified; a fully-unsigned control-plane core is anomalous -- escalate, do not pass" >&2
  exit 1
fi

lock="${OUT_DIR}/images.lock"
{
  printf '{\n'
  printf '  "kubernetesVersion": "%s",\n' "${KUBERNETES_VERSION}"
  printf '  "imageRepository": "%s",\n' "${IMAGE_REPOSITORY}"
  printf '  "verifiedBy": {"identity": "%s", "issuer": "%s"},\n' \
    "${COSIGN_IDENTITY}" "${COSIGN_ISSUER}"
  printf '  "images": [\n%s\n  ]\n' "${entries}"
  printf '}\n'
} > "${lock}"

echo "wrote ${lock}: ${n} images, ${verified_count} signature-verified"
cat "${lock}"
