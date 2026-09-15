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
# Image names (F-IMPORTREF): `crane pull ref@digest` writes the docker-save RepoTags
# as "<repo>:i-was-a-digest", and `ctr images import` names images from RepoTags, so
# the exact refs kubeadm and containerd's sandbox_image look up would be absent after
# the boot import. Each tarball is therefore pulled into a scratch directory,
# validated fail-closed (validate_image_tar), its manifest.json RepoTags rewritten by
# exact string replacement to the exact ref, re-tarred with identical members, and
# re-validated from the NEW tarball before it is moved into OUT_DIR. containerd's
# importer does not bind member bytes to member names and Go's JSON decoder matches
# keys case-insensitively, so every check is on exact member names and exact bytes.
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

# lock_string_ok <value>: succeeds only for a non-empty value made of printable ASCII
# (0x20-0x7e) other than '"' and '\'. images.lock is written with printf below, so
# this is what keeps each top-level value a plain JSON string, and it is the rule the
# boot importer applies when it parses the lock (ADR-16-A2 decision 4). Bytes are
# checked one by one with od (-v: no '*' line folding), so a newline, a control byte
# or a non-ASCII byte anywhere in the value is caught, independent of the locale.
lock_string_ok() {
  [ -n "$1" ] || return 1
  for ls_byte in $(printf '%s' "$1" | od -An -v -tu1); do
    if [ "${ls_byte}" -lt 32 ] || [ "${ls_byte}" -gt 126 ] || [ "${ls_byte}" -eq 34 ] || [ "${ls_byte}" -eq 92 ]; then
      return 1
    fi
  done
}

# require_lock_string <name> <value>: FATAL unless lock_string_ok. The value is not
# echoed: it may hold control bytes.
require_lock_string() {
  if ! lock_string_ok "$2"; then
    printf '%s\n' "FATAL: $1 must be non-empty printable ASCII without '\"' or '\\' (it is written into images.lock)" >&2
    exit 1
  fi
}

# The top-level images.lock values are validated here, before any tool or registry
# call, so a bad value fails the BUILD instead of making every boot refuse the
# bundle (the boot importer applies the same rules and refuses a lock that breaks
# them).
require_lock_string KUBERNETES_VERSION "${KUBERNETES_VERSION}"
require_lock_string IMAGE_REPOSITORY "${IMAGE_REPOSITORY}"
require_lock_string COSIGN_IDENTITY "${COSIGN_IDENTITY}"
require_lock_string COSIGN_ISSUER "${COSIGN_ISSUER}"
if ! printf '%s\n' "${KUBERNETES_VERSION}" | grep -Eqx 'v[0-9]+\.[0-9]+\.[0-9]+'; then
  echo "FATAL: KUBERNETES_VERSION '${KUBERNETES_VERSION}' is not v<major>.<minor>.<patch>" >&2
  exit 1
fi

# Every ref must be fully qualified with a registry host: a bare "name/path" would be
# resolved against Docker Hub by crane but named differently by containerd.
case "${IMAGE_REPOSITORY%%/*}" in
  *.* | *:* | localhost) : ;;
  *) echo "FATAL: IMAGE_REPOSITORY '${IMAGE_REPOSITORY}' must start with a registry host (containing '.' or ':', or 'localhost')" >&2; exit 1 ;;
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

# validate_image_tar <tarball> <expected RepoTag> <work dir> <label>
# Fail-closed validator for a crane docker-save tarball, used on BOTH the raw pull
# and the rewritten output so both are held to one exact shape:
#   (a) every member is a regular file with a unique name; the names are exactly one
#       "sha256:<64 hex>" config, one or more "<64 hex>.tar.gz" layers, and exactly
#       one manifest.json, which is the LAST member (so no "/", "./" or "..");
#   (b) extracted into an empty directory, every blob is a singly-linked regular file
#       whose sha256 equals the hex in its name;
#   (c) manifest.json is byte-for-byte
#       [{"Config":"<config>","RepoTags":["<expected>"],"Layers":["<layer>",...]}]
#       with no other keys, whitespace or trailing bytes, and set(Layers) equals the
#       set of layer members (a layer may repeat inside Layers).
# Leaves <work>/<label>.names (member order), <label>.x/ (extracted members),
# <label>.blobs ("name size sha256" per non-manifest member, in member order) and
# <label>.layers.text (the exact Layers list text, order and repeats included).
validate_image_tar() {
  vt_tar="$1"
  vt_expect="$2"
  vt_work="$3"
  vt_label="$4"
  vt_names="${vt_work}/${vt_label}.names"
  vt_listing="${vt_work}/${vt_label}.listing"
  vt_x="${vt_work}/${vt_label}.x"
  vt_blobs="${vt_work}/${vt_label}.blobs"
  vt_fail="FATAL: ${vt_tar} (expected RepoTag ${vt_expect}):"

  # (a) member names and types. busybox tar silently rewrites unsafe names when it
  # lists or extracts (it strips a leading "/" and "../" prefixes, warning once on
  # stderr), so ANY stderr output from tar is FATAL: the names seen below must be the
  # names actually stored.
  tar -tf "${vt_tar}" > "${vt_names}" 2> "${vt_work}/${vt_label}.tar-stderr"
  tar -tvf "${vt_tar}" > "${vt_listing}" 2>> "${vt_work}/${vt_label}.tar-stderr"
  if [ -s "${vt_work}/${vt_label}.tar-stderr" ]; then
    echo "${vt_fail} tar reported:" >&2
    cat "${vt_work}/${vt_label}.tar-stderr" >&2
    exit 1
  fi
  vt_count="$(wc -l < "${vt_names}")"
  if [ "${vt_count}" -lt 3 ]; then
    echo "${vt_fail} ${vt_count} members; want a config, >= 1 layer and manifest.json" >&2
    exit 1
  fi
  if grep -Evx 'manifest\.json|sha256:[0-9a-f]{64}|[0-9a-f]{64}\.tar\.gz' "${vt_names}" >&2 \
    || grep -E '/|^\.|\.\.' "${vt_names}" >&2; then
    echo "${vt_fail} unexpected member name(s) above" >&2
    exit 1
  fi
  if [ -n "$(sort "${vt_names}" | uniq -d)" ]; then
    echo "${vt_fail} duplicate member names: $(sort "${vt_names}" | uniq -d | tr '\n' ' ')" >&2
    exit 1
  fi
  if [ "$(grep -cx 'manifest\.json' "${vt_names}")" != 1 ] || [ "$(tail -n 1 "${vt_names}")" != manifest.json ]; then
    echo "${vt_fail} manifest.json must appear exactly once, as the last member" >&2
    exit 1
  fi
  if [ "$(grep -cEx 'sha256:[0-9a-f]{64}' "${vt_names}")" != 1 ]; then
    echo "${vt_fail} exactly one sha256:<hex> config member is required" >&2
    exit 1
  fi
  # busybox tar -tv: "<mode> <owner> <size> <date> <time> <name>"; a symlink or
  # hardlink appends " -> <target>" (a hardlink keeps a '-' mode), so require exactly
  # six fields, a '-' type, and the name sequence to equal tar -tf's.
  if awk 'NF != 6 || substr($1, 1, 1) != "-"' "${vt_listing}" | grep . >&2 \
    || ! awk '{ print $6 }' "${vt_listing}" | cmp -s - "${vt_names}"; then
    echo "${vt_fail} every member must be a regular file (non-regular or mismatched listing above)" >&2
    exit 1
  fi

  # (b) extract into a fresh, empty directory; bind each blob to its name.
  mkdir "${vt_x}"
  tar -xf "${vt_tar}" -C "${vt_x}" 2> "${vt_work}/${vt_label}.tar-stderr"
  if [ -s "${vt_work}/${vt_label}.tar-stderr" ]; then
    echo "${vt_fail} tar extraction reported:" >&2
    cat "${vt_work}/${vt_label}.tar-stderr" >&2
    exit 1
  fi
  if [ "$(find "${vt_x}" -mindepth 1 | wc -l)" != "${vt_count}" ] \
    || [ -n "$(find "${vt_x}" -mindepth 1 ! -type f)" ]; then
    echo "${vt_fail} extraction produced entries other than the ${vt_count} listed regular files" >&2
    exit 1
  fi
  : > "${vt_blobs}"
  while IFS= read -r vt_name; do
    vt_path="${vt_x}/${vt_name}"
    if [ ! -f "${vt_path}" ] || [ -L "${vt_path}" ] || [ "$(stat -c %h "${vt_path}")" != 1 ]; then
      echo "${vt_fail} ${vt_name} is not a singly-linked regular file after extraction" >&2
      exit 1
    fi
    vt_size="$(stat -c %s "${vt_path}")"
    vt_listed="$(awk -v n="${vt_name}" '$6 == n { print $3 }' "${vt_listing}")"
    if [ "${vt_size}" != "${vt_listed}" ]; then
      echo "${vt_fail} ${vt_name} size ${vt_size} != listed ${vt_listed}" >&2
      exit 1
    fi
    [ "${vt_name}" != manifest.json ] || continue
    vt_hex="${vt_name#sha256:}"
    vt_hex="${vt_hex%.tar.gz}"
    vt_sha="$(sha256sum "${vt_path}" | cut -d' ' -f1)"
    if [ "${vt_sha}" != "${vt_hex}" ]; then
      echo "${vt_fail} blob ${vt_name} has sha256 ${vt_sha}" >&2
      exit 1
    fi
    printf '%s %s %s\n' "${vt_name}" "${vt_size}" "${vt_sha}" >> "${vt_blobs}"
  done < "${vt_names}"

  # (c) manifest.json exact bytes. The prefix and suffix are compared literally (quoted
  # patterns), the Layers list is matched by a strict ERE on a single line, and the
  # file size must equal the validated pieces, so no byte (newline, NUL, a second JSON
  # value) can hide outside them.
  vt_mfile="${vt_x}/manifest.json"
  vt_msize="$(stat -c %s "${vt_mfile}")"
  if [ "${vt_msize}" -gt 1048576 ]; then
    echo "${vt_fail} manifest.json is ${vt_msize} bytes" >&2
    exit 1
  fi
  vt_cfg="$(grep -Ex 'sha256:[0-9a-f]{64}' "${vt_names}")"
  vt_pre="[{\"Config\":\"${vt_cfg}\",\"RepoTags\":[\"${vt_expect}\"],\"Layers\":["
  vt_suf=']}]'
  vt_m="$(cat "${vt_mfile}")"
  case "${vt_m}" in
    "${vt_pre}"*"${vt_suf}") : ;;
    *)
      echo "${vt_fail} manifest.json is not exactly ${vt_pre}...${vt_suf}; got:" >&2
      printf '%s\n' "${vt_m}" >&2
      exit 1
      ;;
  esac
  vt_mid="${vt_m#"${vt_pre}"}"
  vt_mid="${vt_mid%"${vt_suf}"}"
  if [ "$(printf '%s\n' "${vt_mid}" | wc -l)" != 1 ] \
    || ! printf '%s\n' "${vt_mid}" | grep -Eqx '"[0-9a-f]{64}\.tar\.gz"(,"[0-9a-f]{64}\.tar\.gz")*'; then
    echo "${vt_fail} manifest.json Layers is not a plain list of <hex>.tar.gz names" >&2
    exit 1
  fi
  # Byte count via wc -c (busybox ${#var} counts characters, not bytes).
  vt_want="$(printf '%s' "${vt_pre}${vt_mid}${vt_suf}" | wc -c | tr -d ' ')"
  if [ "${vt_msize}" != "${vt_want}" ]; then
    echo "${vt_fail} manifest.json has ${vt_msize} bytes, want ${vt_want} (trailing or hidden bytes)" >&2
    exit 1
  fi
  if [ "$(grep -o '"RepoTags":\[' "${vt_mfile}" | wc -l)" != 1 ]; then
    echo "${vt_fail} \"RepoTags\":[ must occur exactly once in manifest.json" >&2
    exit 1
  fi
  printf '%s' "${vt_mid}" > "${vt_work}/${vt_label}.layers.text"
  printf '%s\n' "${vt_mid}" | tr ',' '\n' | tr -d '"' | sort -u > "${vt_work}/${vt_label}.layers.manifest"
  grep -Ex '[0-9a-f]{64}\.tar\.gz' "${vt_names}" | sort -u > "${vt_work}/${vt_label}.layers.members"
  if ! cmp -s "${vt_work}/${vt_label}.layers.manifest" "${vt_work}/${vt_label}.layers.members"; then
    echo "${vt_fail} manifest.json Layers do not match the layer members" >&2
    exit 1
  fi
}

# bundle_image <ref> <digest> <dest tarball>
# Pull ref@digest for TARGETARCH into a scratch directory outside OUT_DIR, rewrite
# its RepoTags from "<repo>:i-was-a-digest" to the exact ref, and move the result to
# <dest> ONLY after the new tarball re-validates and its members (names, sizes, blob
# bytes) equal the raw pull's. Sets bundled_sha to the sha256 of <dest> after the move.
bundle_image() {
  bi_ref="$1"
  bi_digest="$2"
  bi_dest="$3"
  bi_placeholder="${bi_ref%:*}:i-was-a-digest"

  bi_work="$(mktemp -d)"
  trap 'rm -rf "${bi_work}"' EXIT
  case "${bi_work%/}/" in
    "${OUT_DIR%/}/"*) echo "FATAL: scratch directory ${bi_work} is under OUT_DIR ${OUT_DIR}" >&2; exit 1 ;;
  esac
  if [ -e "${bi_dest}" ]; then
    echo "FATAL: ${bi_dest} already exists (two refs map to one tarball name?)" >&2
    exit 1
  fi

  crane pull --platform "linux/${TARGETARCH}" "${bi_ref}@${bi_digest}" "${bi_work}/raw.tar"
  bi_raw_sha="$(sha256sum "${bi_work}/raw.tar" | cut -d' ' -f1)"
  echo "pulled ${bi_ref}@${bi_digest} (linux/${TARGETARCH}) raw=${bi_raw_sha}"
  validate_image_tar "${bi_work}/raw.tar" "${bi_placeholder}" "${bi_work}" raw

  # Exact literal replacement (quoted patterns; the raw validation proved the old
  # string occurs exactly once), then re-tar the SAME members in the raw order.
  # touch -r gives the new manifest.json the config member's mtime so its header does
  # not carry the build time. That pins ONLY that mtime: busybox tar also records
  # each member's mode and owner as they are on disk in this stage (which depends on
  # the user and umask the extraction ran with) in its own header layout, so the
  # final tarball bytes are stable only for the same build environment (this
  # stage's pinned alpine/busybox, run as root), not reproducible in general.
  # Nothing relies on more: the lock records the registry digest, and the etcd tool
  # extraction and the logged final= hash bind to the bytes produced in this run.
  bi_old="\"RepoTags\":[\"${bi_placeholder}\"]"
  bi_new="\"RepoTags\":[\"${bi_ref}\"]"
  bi_m="$(cat "${bi_work}/raw.x/manifest.json")"
  bi_head="${bi_m%%"${bi_old}"*}"
  bi_tail="${bi_m#*"${bi_old}"}"
  if [ "${bi_head}" = "${bi_m}" ] || [ "${bi_tail}" = "${bi_m}" ]; then
    echo "FATAL: ${bi_old} not found in the raw manifest.json of ${bi_ref}" >&2
    exit 1
  fi
  printf '%s' "${bi_head}${bi_new}${bi_tail}" > "${bi_work}/raw.x/manifest.json"
  touch -r "${bi_work}/raw.x/$(grep -Ex 'sha256:[0-9a-f]{64}' "${bi_work}/raw.names")" \
    "${bi_work}/raw.x/manifest.json"
  tar -cf "${bi_work}/final.tar" -C "${bi_work}/raw.x" -T "${bi_work}/raw.names"

  # Re-read the NEW tarball: same validator with the exact ref, member for member
  # (name, size, blob sha256) identical to the raw pull, and the same Layers list
  # (order and repeats: the layer stack, not just its set).
  bi_checked_sha="$(sha256sum "${bi_work}/final.tar" | cut -d' ' -f1)"
  validate_image_tar "${bi_work}/final.tar" "${bi_ref}" "${bi_work}" final
  if ! cmp -s "${bi_work}/raw.blobs" "${bi_work}/final.blobs"; then
    echo "FATAL: rewritten ${bi_ref} tarball members differ from the raw pull:" >&2
    diff "${bi_work}/raw.blobs" "${bi_work}/final.blobs" >&2 || true
    exit 1
  fi
  if ! cmp -s "${bi_work}/raw.layers.text" "${bi_work}/final.layers.text"; then
    echo "FATAL: rewritten ${bi_ref} manifest.json Layers differ from the raw pull (order or repeats)" >&2
    exit 1
  fi

  mv "${bi_work}/final.tar" "${bi_dest}"
  bundled_sha="$(sha256sum "${bi_dest}" | cut -d' ' -f1)"
  if [ "${bundled_sha}" != "${bi_checked_sha}" ]; then
    echo "FATAL: ${bi_dest} sha256 ${bundled_sha} != validated ${bi_checked_sha}" >&2
    exit 1
  fi
  echo "rewrote ${bi_placeholder} -> ${bi_ref} raw=${bi_raw_sha} final=${bundled_sha}"
  rm -rf "${bi_work}"
  trap - EXIT
}

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

  # The tarball must be byte-identical to what bundle_image validated and moved into
  # OUT_DIR for the verified digest (no rewrite between then and here).
  xe_got_sha="$(sha256sum "${xe_tar}" | cut -d' ' -f1)"
  if [ "${xe_got_sha}" != "${xe_tar_sha}" ]; then
    echo "FATAL: ${xe_tar} changed since it was bundled (sha256 ${xe_got_sha} != ${xe_tar_sha})" >&2
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

# 0. Allowlist EVERY ref before any registry call (crane digest, cosign verify):
#    under IMAGE_REPOSITORY, a plain host[:port]/path:tag (no digest, no uppercase
#    path, no JSON metacharacters). One bad ref anywhere in the list stops the build
#    before anything is fetched.
while IFS= read -r ref; do
  [ -n "${ref}" ] || continue
  case "${ref}" in
    "${IMAGE_REPOSITORY}/"*) : ;;
    *) echo "FATAL: ref '${ref}' is not under IMAGE_REPOSITORY '${IMAGE_REPOSITORY}/'" >&2; exit 1 ;;
  esac
  if ! printf '%s\n' "${ref}" \
    | grep -Eqx '[a-z0-9.-]+(:[0-9]+)?/[a-z0-9]+([._/-][a-z0-9]+)*:[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}'; then
    echo "FATAL: ref '${ref}' is not an allowed <host>[:port]/<path>:<tag> reference" >&2
    exit 1
  fi
done < /tmp/imglist

while IFS= read -r ref; do
  [ -n "${ref}" ] || continue

  # 1. resolve tag -> digest once; everything after is content-addressed.
  # Exactly sha256:<64 lowercase hex> on one line: it is written into images.lock,
  # whose digest rule the boot importer enforces.
  digest="$(crane digest "${ref}")"
  if [ "$(printf '%s\n' "${digest}" | wc -l)" != 1 ] \
    || ! printf '%s\n' "${digest}" | grep -Eqx 'sha256:[0-9a-f]{64}'; then
    echo "FATAL: bad digest for ${ref}: '${digest}'" >&2
    exit 1
  fi

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
  #    content-addressed index instead of crane's implicit linux/amd64. bundle_image
  #    names the tarball's image with the exact ref before it enters OUT_DIR.
  f="${OUT_DIR}/$(echo "${ref}" | tr '/:' '__').tar"
  bundle_image "${ref}" "${digest}" "${f}"
  tarball="$(basename "${f}")"

  # Record the etcd entry for the etcdctl/etcdutl extraction below, binding the
  # bytes of the final (validated, renamed) tarball as moved into OUT_DIR.
  case "${ref}" in
    */etcd:*)
      etcd_count=$((etcd_count + 1))
      etcd_ref="${ref}"
      etcd_digest="${digest}"
      etcd_tar="${f}"
      etcd_tar_sha="${bundled_sha}"
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

# The images.lock BYTE LAYOUT below is a RUNTIME contract (ADR-16-A2 decision 4):
# at every boot internal/imageimport parses the lock strictly, re-renders it with
# its one Go renderer and refuses the whole bundle (reason lock-invalid) unless the
# re-rendered bytes EQUAL the file: key names and order, the spacing, one image per
# line, LF line ends and the single trailing newline. Do not change a byte of this
# layout, a key or a value rule without changing that renderer and its golden locks
# (internal/imageimport/testdata/images.lock.<version>) in the same change. The /v1
# release attestation predicate is these bytes too.
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
