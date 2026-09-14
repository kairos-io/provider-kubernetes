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
# empirically incomplete and varies by PATCH release -- e.g. the kube-apiserver/
# controller-manager/proxy images for v1.34.0 and v1.35.0 carry NO discoverable
# cosign signature at either registry.k8s.io or the canonical us-central1 backing
# registry, while all seven images verify for v1.35.8, v1.36.4 and v1.37.0 as of
# 2026-09-13):
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
# etcdctl + etcdutl (ADR-12-A1): after the floor passes and images.lock is written,
# the static etcdctl/etcdutl are EXTRACTED from the single etcd image tarball
# bundled above (signature-verified: etcd is on the floor) into TOOLS_DIR. No new
# download origin, no new pin: the tools are byte-for-byte the ones inside the
# verified etcd image, so they are version-matched to the etcd kubeadm deploys.
# The extraction is offline (crane export reads the on-disk tarball from stdin).
#
# Requires: kubeadm, crane, cosign on PATH (installed + pinned in the Dockerfile).
set -eu

: "${KUBERNETES_VERSION:?KUBERNETES_VERSION is required}"
# REQUIRED, never defaulted: crane resolves a multi-arch index to linux/amd64 unless
# told otherwise, so a silent default would bundle amd64 images (and amd64 etcd
# tools) into an arm64 image. elf_machine is the ELF e_machine the extracted etcd
# tools must carry for this architecture.
: "${TARGETARCH:?TARGETARCH is required (amd64 or arm64)}"
case "${TARGETARCH}" in
  amd64) elf_machine=62 ;;
  arm64) elf_machine=183 ;;
  *) echo "FATAL: unsupported TARGETARCH '${TARGETARCH}' (amd64 or arm64)" >&2; exit 1 ;;
esac
IMAGE_REPOSITORY="${IMAGE_REPOSITORY:-registry.k8s.io}"
# Kubernetes release images are cosign keyless-signed by the krel promoter identity
# (kubernetes.io/docs/tasks/administer-cluster/verify-signed-artifacts).
COSIGN_IDENTITY="${COSIGN_IDENTITY:-krel-trust@k8s-releng-prod.iam.gserviceaccount.com}"
COSIGN_ISSUER="${COSIGN_ISSUER:-https://accounts.google.com}"
OUT_DIR="${OUT_DIR:-/images}"
TOOLS_DIR="${TOOLS_DIR:-/tools}"
MIN_IMAGES="${MIN_IMAGES:-5}"

# The tools must never land in the embedded image directory (it is imported into
# containerd at boot and attested as images.lock's directory).
case "${TOOLS_DIR%/}/" in
  "${OUT_DIR%/}/"*) echo "FATAL: TOOLS_DIR '${TOOLS_DIR}' must not be OUT_DIR '${OUT_DIR}' or under it" >&2; exit 1 ;;
esac

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
etcd_count=0
etcd_ref=""
etcd_digest=""
etcd_tar=""
etcd_tar_sha=""
etcd_verified=false

# extract_etcd_tools <ref> <digest> <tarball> <tarball-sha256>
# Install etcdctl + etcdutl into TOOLS_DIR from the bundled etcd image tarball.
# Every deviation from the expected shape is FATAL: the tarball must be unchanged
# since its pull, each member must appear exactly once as a regular file, and each
# installed binary must be a root-owned 0755 (no setuid/setgid) ELF for
# TARGETARCH that EXECUTES here (musl-runnable) and reports the etcd image tag's
# version (3.7.0-0 -> "etcdctl version: 3.7.0").
extract_etcd_tools() {
  xe_ref="$1"
  xe_digest="$2"
  xe_tar="$3"
  xe_tar_sha="$4"

  xe_tag="${xe_ref##*:}"
  if ! printf '%s\n' "${xe_tag}" | grep -Eqx '[0-9]+\.[0-9]+\.[0-9]+-[0-9]+'; then
    echo "FATAL: etcd image tag '${xe_tag}' (${xe_ref}) is not <major>.<minor>.<patch>-<N>" >&2
    exit 1
  fi
  xe_want="${xe_tag%-*}"

  # The tarball must be byte-identical to what crane pull wrote for the verified
  # digest (no rewrite between the pull and here).
  xe_got_sha="$(sha256sum "${xe_tar}" | cut -d' ' -f1)"
  if [ "${xe_got_sha}" != "${xe_tar_sha}" ]; then
    echo "FATAL: ${xe_tar} changed since pull (sha256 ${xe_got_sha} != ${xe_tar_sha})" >&2
    exit 1
  fi

  xe_scratch="$(mktemp -d)"
  trap 'rm -rf "${xe_scratch}"' EXIT
  # Flatten the image filesystem offline (the image is read from stdin, not a registry).
  crane export - "${xe_scratch}/fs.tar" < "${xe_tar}"
  tar -tf "${xe_scratch}/fs.tar" > "${xe_scratch}/names"
  tar -tvf "${xe_scratch}/fs.tar" > "${xe_scratch}/listing"
  mkdir -p "${TOOLS_DIR}"

  for xe_tool in etcdctl etcdutl; do
    xe_member="usr/local/bin/${xe_tool}"

    # Exactly once, counting every spelling that extracts to the same path
    # ("./usr/...", "/usr/..."), and present under the exact canonical name.
    xe_n="$(sed -E 's#^(\./|/)+##' "${xe_scratch}/names" | grep -cxF "${xe_member}" || true)"
    xe_exact="$(grep -cxF "${xe_member}" "${xe_scratch}/names" || true)"
    if [ "${xe_n}" != 1 ] || [ "${xe_exact}" != 1 ]; then
      echo "FATAL: ${xe_member} must appear exactly once in ${xe_ref}@${xe_digest} (found ${xe_n}, canonical ${xe_exact})" >&2
      exit 1
    fi

    # Regular file per the listing. busybox tar -tv prints
    # "<mode> <owner> <size> <date> <time> <name>[ -> <target>]" and shows a
    # hardlink with a '-' mode plus " -> <target>", so require the name field to be
    # EXACTLY the member (no link target) and the mode type to be '-'.
    xe_types="$(awk -v m="${xe_member}" '{
      name = $0
      sub(/^[^ ]+ +[^ ]+ +[^ ]+ +[^ ]+ +[^ ]+ /, "", name)
      if (name == m) printf "%s", substr($1, 1, 1)
    }' "${xe_scratch}/listing")"
    if [ "${xe_types}" != "-" ]; then
      echo "FATAL: ${xe_member} in ${xe_ref}@${xe_digest} is not a single regular file (listing types '${xe_types}'; symlink/hardlink rejected)" >&2
      grep -F "${xe_member}" "${xe_scratch}/listing" >&2 || true
      exit 1
    fi

    # Extract ONLY that member into a fresh, empty directory, then re-check the type
    # on disk (a lone hardlink member fails to extract; a symlink would be caught).
    xe_x="${xe_scratch}/x-${xe_tool}"
    mkdir "${xe_x}"
    tar -xf "${xe_scratch}/fs.tar" -C "${xe_x}" "${xe_member}"
    xe_src="${xe_x}/${xe_member}"
    if [ ! -f "${xe_src}" ] || [ -L "${xe_src}" ] || [ "$(stat -c %h "${xe_src}")" != 1 ]; then
      echo "FATAL: extracted ${xe_member} is not a regular, singly-linked file" >&2
      exit 1
    fi

    xe_dst="${TOOLS_DIR}/${xe_tool}"
    install -m 0755 -o root -g root "${xe_src}" "${xe_dst}"
    xe_mode="$(stat -c '%F|%a|%u|%g' "${xe_dst}")"
    if [ "${xe_mode}" != "regular file|755|0|0" ] || [ -u "${xe_dst}" ] || [ -g "${xe_dst}" ]; then
      echo "FATAL: ${xe_dst} must be a root:root 0755 regular file without setuid/setgid (got ${xe_mode})" >&2
      exit 1
    fi

    xe_magic="$(head -c 4 "${xe_dst}" | od -An -tx1 | tr -d ' \n')"
    xe_machine="$(od -An -tu2 -j18 -N2 "${xe_dst}" | tr -d ' \n')"
    if [ "${xe_magic}" != 7f454c46 ] || [ "${xe_machine}" != "${elf_machine}" ]; then
      echo "FATAL: ${xe_dst} is not an ELF binary for ${TARGETARCH} (magic ${xe_magic}, e_machine ${xe_machine}, want ${elf_machine})" >&2
      exit 1
    fi

    # Execute it here, on alpine/musl like Hadron: a glibc-linked binary cannot run,
    # so success proves it is musl-runnable (upstream etcd tools are static Go).
    # Empty environment, as the provider runs it.
    if ! xe_out="$(env -i "${xe_dst}" version 2>&1)"; then
      echo "FATAL: ${xe_dst} does not execute in the musl build stage:" >&2
      printf '%s\n' "${xe_out}" >&2
      exit 1
    fi
    xe_line="$(printf '%s\n' "${xe_out}" | head -n 1)"
    if [ "${xe_line}" != "${xe_tool} version: ${xe_want}" ]; then
      echo "FATAL: ${xe_dst} reports '${xe_line}', want '${xe_tool} version: ${xe_want}' (etcd image ${xe_ref})" >&2
      exit 1
    fi

    xe_sha="$(sha256sum "${xe_dst}" | cut -d' ' -f1)"
    echo "etcd-tools: ${xe_dst} ${xe_line} sha256=${xe_sha} from ${xe_ref}@${xe_digest} (${xe_member})"
  done

  rm -rf "${xe_scratch}"
  trap - EXIT
}

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

  # 3. pull the SAME digest we just resolved+checked. The digest is the INDEX digest
  #    (what cosign verified); --platform selects the TARGETARCH manifest from that
  #    content-addressed index instead of crane's implicit linux/amd64.
  f="${OUT_DIR}/$(echo "${ref}" | tr '/:' '__').tar"
  crane pull --platform "linux/${TARGETARCH}" "${ref}@${digest}" "${f}"
  tarball="$(basename "${f}")"

  # Record the etcd entry for the etcdctl/etcdutl extraction below, binding the
  # tarball bytes as pulled.
  case "${ref}" in
    */etcd:*)
      etcd_count=$((etcd_count + 1))
      etcd_ref="${ref}"
      etcd_digest="${digest}"
      etcd_tar="${f}"
      etcd_tar_sha="$(sha256sum "${f}" | cut -d' ' -f1)"
      etcd_verified="${verified}"
      ;;
  esac

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

# --- etcdctl / etcdutl from the verified etcd image (ADR-12-A1) ---
if [ "${etcd_count}" -ne 1 ]; then
  echo "FATAL: expected exactly one */etcd:* image in the bundle, got ${etcd_count}" >&2
  exit 1
fi
if [ "${etcd_verified}" != true ]; then
  echo "FATAL: etcd image ${etcd_ref}@${etcd_digest} is not signature-verified; refusing to extract etcdctl/etcdutl" >&2
  exit 1
fi
extract_etcd_tools "${etcd_ref}" "${etcd_digest}" "${etcd_tar}" "${etcd_tar_sha}"
