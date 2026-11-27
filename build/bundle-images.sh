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
# Archive format (ADR-16-A3, F-IMPORTBLOB): each image is bundled as a canonical
# single-platform OCI IMAGE LAYOUT, never docker-save. docker-save carries no
# manifest document at all, so containerd RE-SERIALIZES one at import; that
# synthesized manifest digest exists in no registry, and the first later CRI
# PullImage of the same ref re-points the image record at the real registry index
# and orphans it, after which GC deletes it and CreateContainer fails with
# "content digest sha256:...: not found". An OCI layout ships the registry's OWN
# manifest bytes, so the imported record points at a digest that exists.
#
# Image names (F-IMPORTREF): containerd names an imported OCI layout from
# index.json's org.opencontainers.image.ref.name annotation, which crane does NOT
# write (without it the import creates no named image record at all), so each
# layout's index.json is RE-RENDERED here, canonically, with exactly that one
# annotation set to the exact ref kubeadm and containerd's sandbox_image look up.
# Nothing the registry served is rewritten: the manifest, config and layer blobs
# are its own bytes, named by their own digests.
#
# Every tarball is pulled into a scratch directory, archived to one canonical
# profile (fixed member order, no directory members, uid/gid 0, mode 0644, mtime
# epoch 0) and then validated fail-closed from its OWN bytes (validate_image_tar)
# before it is moved into OUT_DIR. containerd's importer does not bind member bytes
# to member names, keys members on path.Clean(name) where a later duplicate
# silently overwrites an earlier one, takes the OCI branch (and returns early) when
# oci-layout is present, and lets Go's JSON decoder match keys case-insensitively,
# so every check is on exact member names and exact bytes, and the validator is
# never looser than that importer.
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
# The images.lock records ref -> index digest + per-platform manifestDigest +
# per-image verified flag/reason, so each released artifact attests exactly what it
# baked and at what trust level (feeds the CycloneDX SBOM attestation, ADR-15).
# manifestDigest is what the boot importer PINS each archive to, so the lock is
# ARCH-SPECIFIC (see the layout comment above the printf block below).
#
# etcdctl + etcdutl (ADR-12-A1): after the floor passes and images.lock is written,
# the static etcdctl/etcdutl are EXTRACTED from the etcd image the bundle carries
# (signature-verified: etcd is on the floor) into TOOLS_DIR. No new download
# origin, no new pin: the extraction is pinned to the per-platform MANIFEST digest
# the bundled archive names, and the flattened image's config digest must equal the
# one that archive's manifest names, so the tools are provably the ones inside the
# verified, bundled etcd image and are version-matched to the etcd kubeadm deploys.
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

# BEGIN retry (extracted by scripts/test-bundle-images-retry.sh)
# retry <cmd> [args...]: run a read-only registry fetch up to 5 times, sleeping
# 5, 15, 30, 60 s between tries (times BUNDLE_RETRY_DELAY_SCALE, default 1), so a
# registry quota error does not fail the build. Notices go to stderr, stdout is
# the command's own. Returns the last exit status, so it stays fail-closed.
retry() {
  rt_scale="${BUNDLE_RETRY_DELAY_SCALE:-1}"
  case "${rt_scale}" in
    '' | *[!0-9]*) echo "FATAL: BUNDLE_RETRY_DELAY_SCALE '${rt_scale}' is not a whole number" >&2; return 2 ;;
  esac
  rt_n=1
  while :; do
    rt_rc=0
    "$@" || rt_rc=$?
    if [ "${rt_rc}" -eq 0 ] || [ "${rt_n}" -ge 5 ]; then
      return "${rt_rc}"
    fi
    case "${rt_n}" in
      1) rt_delay=5 ;;
      2) rt_delay=15 ;;
      3) rt_delay=30 ;;
      *) rt_delay=60 ;;
    esac
    rt_delay=$((rt_delay * rt_scale))
    echo "retry: '$*' failed (exit ${rt_rc}), attempt ${rt_n}/5, next in ${rt_delay}s" >&2
    sleep "${rt_delay}"
    rt_n=$((rt_n + 1))
  done
}
# END retry

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
etcd_manifest_digest=""
etcd_config_digest=""
etcd_tar=""
etcd_tar_sha=""
etcd_verified=false

# --- the canonical OCI image-layout archive (ADR-16-A3) ----------------------
# These bounds, media types and byte-exact renderings MUST equal
# internal/imageimport/tarcheck.go's (indexMaxSize, manifestMaxSize,
# configMaxSize, min/maxTarMembers, ociLayoutBytes, renderIndex): the boot
# importer refuses anything else, and the governing rule is that a validator
# must never be LOOSER than containerd's importer.
MAX_INDEX_BYTES=8192
MAX_MANIFEST_BYTES=65536
MAX_CONFIG_BYTES=1048576
MIN_MEMBERS=3
MAX_MEMBERS=256
MEDIA_TYPE_OCI_INDEX='application/vnd.oci.image.index.v1+json'
MEDIA_TYPE_DOCKER_INDEX='application/vnd.docker.distribution.manifest.list.v2+json'
MEDIA_TYPE_OCI_MANIFEST='application/vnd.oci.image.manifest.v1+json'
MEDIA_TYPE_DOCKER_MANIFEST='application/vnd.docker.distribution.manifest.v2+json'
ANNOTATION_REF_NAME='org.opencontainers.image.ref.name'
# containerd's imageName() reads io.containerd.image.name FIRST and only falls
# back to org.opencontainers.image.ref.name, so an archive carrying it would
# choose the imported record's NAME itself while a ref.name-only validator
# reported success -- F-IMPORTREF re-opened as a TRUST bug. Refused outright.
ANNOTATION_CONTAINERD_NAME='io.containerd.image.name'

# render_oci_layout: the EXACT oci-layout member bytes -- go-containerregistry's
# PRETTY-printed form, 37 bytes, NO trailing newline (crane's layout/write.go).
# NOT the compact {"imageLayoutVersion":"1.0.0"}: internal/imageimport compares
# these bytes literally and never parses them, so the compact form would make
# every boot refuse every tarball. Rendered here, and written OVER crane's copy,
# so the shipped member is this one string and a future crane that formats its
# own oci-layout differently cannot change what boots. Changing these bytes means
# changing internal/imageimport's ociLayoutBytes in the same commit.
render_oci_layout() {
  printf '{\n    "imageLayoutVersion": "1.0.0"\n}'
}

# render_index <mediaType> <size> <manifest digest> <ref>: the ONE canonical
# index.json rendering -- compact, NO trailing newline, byte for byte what
# internal/imageimport's renderIndex() re-renders and compares. EXACTLY ONE
# manifest descriptor, platform exactly linux/<TARGETARCH>, and EXACTLY ONE
# annotation (see ANNOTATION_CONTAINERD_NAME). crane does NOT write that
# annotation, and without it containerd's importer creates NO named image
# record at all, so this rendering -- not crane's index.json -- is what makes
# the bundle importable under the exact ref kubeadm looks up.
# Every interpolated value is allowlisted before it reaches here (media type
# from a closed set, size digits only, digest sha256 hex, ref by the step-0 ref
# allowlist), so the printf JSON is well-formed by construction -- the same
# invariant the images.lock printf below relies on.
render_index() {
  printf '{"schemaVersion":2,"mediaType":"%s","manifests":[{"mediaType":"%s","size":%s,"digest":"%s","platform":{"architecture":"%s","os":"linux"},"annotations":{"%s":"%s"}}]}' \
    "${MEDIA_TYPE_OCI_INDEX}" "$1" "$2" "$3" "${TARGETARCH}" "${ANNOTATION_REF_NAME}" "$4"
}

# count_matches <string> <BRE>: how many times the pattern occurs in the string.
count_matches() {
  printf '%s' "$1" | grep -o -e "$2" 2>/dev/null | grep -c . || true
}

# derive_descriptor <index.json path>
# Read the ONE manifest descriptor out of an OCI layout's index.json and set
# desc_mt / desc_size / desc_digest. Accepts crane's PRETTY-printed index.json
# and our compact one by stripping ASCII whitespace first (no value in either
# form contains whitespace), then holding the result to an anchored prefix and
# suffix and to EXACTLY ONE mediaType, size and digest inside the single
# descriptor -- a second descriptor adds a second of each and is refused.
derive_descriptor() {
  dd_file="$1"
  dd_fail="FATAL: ${dd_file}:"

  dd_bytes="$(wc -c < "${dd_file}")"
  if [ "${dd_bytes}" -lt 2 ] || [ "${dd_bytes}" -gt "${MAX_INDEX_BYTES}" ]; then
    echo "${dd_fail} index.json is ${dd_bytes} bytes, want 2..${MAX_INDEX_BYTES}" >&2
    exit 1
  fi
  dd_flat="$(tr -d ' \t\n\r' < "${dd_file}")"
  case "${dd_flat}" in
    *"${ANNOTATION_CONTAINERD_NAME}"*)
      echo "${dd_fail} index.json carries ${ANNOTATION_CONTAINERD_NAME}, which would let the archive choose the imported image's NAME" >&2
      exit 1
      ;;
  esac
  dd_pre="{\"schemaVersion\":2,\"mediaType\":\"${MEDIA_TYPE_OCI_INDEX}\",\"manifests\":[{"
  dd_suf='}]}'
  case "${dd_flat}" in
    "${dd_pre}"*"${dd_suf}") : ;;
    *)
      echo "${dd_fail} index.json is not schemaVersion 2 / ${MEDIA_TYPE_OCI_INDEX} / one manifests entry; got:" >&2
      cat "${dd_file}" >&2
      echo >&2
      exit 1
      ;;
  esac
  dd_body="${dd_flat#"${dd_pre}"}"
  dd_body="${dd_body%"${dd_suf}"}"

  # Media type: allowlisted. An index/manifest-list media type is REFUSED --
  # a nested index would name a multi-arch index whose other platforms' blobs
  # are absent from this archive.
  if [ "$(count_matches "${dd_body}" '"mediaType":"[^"]*"')" != 1 ]; then
    echo "${dd_fail} index.json must hold exactly one manifest descriptor with exactly one mediaType" >&2
    exit 1
  fi
  dd_mt="$(printf '%s' "${dd_body}" | grep -o '"mediaType":"[^"]*"')"
  dd_mt="${dd_mt#\"mediaType\":\"}"
  dd_mt="${dd_mt%\"}"
  case "${dd_mt}" in
    "${MEDIA_TYPE_OCI_MANIFEST}" | "${MEDIA_TYPE_DOCKER_MANIFEST}") : ;;
    "${MEDIA_TYPE_OCI_INDEX}" | "${MEDIA_TYPE_DOCKER_INDEX}")
      echo "${dd_fail} index.json names an INDEX (${dd_mt}); the archive must name one platform manifest" >&2
      exit 1
      ;;
    *)
      echo "${dd_fail} index.json descriptor mediaType '${dd_mt}' is not allowed" >&2
      exit 1
      ;;
  esac

  if [ "$(count_matches "${dd_body}" '"digest":')" != 1 ] \
    || [ "$(count_matches "${dd_body}" '"digest":"sha256:[0-9a-f]\{64\}"')" != 1 ]; then
    echo "${dd_fail} index.json must name exactly one manifest digest, as sha256:<64 lowercase hex>" >&2
    exit 1
  fi
  dd_digest="$(printf '%s' "${dd_body}" | grep -o '"digest":"sha256:[0-9a-f]\{64\}"')"
  dd_digest="${dd_digest#\"digest\":\"}"
  dd_digest="${dd_digest%\"}"

  if [ "$(count_matches "${dd_body}" '"size":')" != 1 ] \
    || [ "$(count_matches "${dd_body}" '"size":[0-9][0-9]*')" != 1 ]; then
    echo "${dd_fail} index.json must declare exactly one manifest size, as a plain number" >&2
    exit 1
  fi
  dd_size="$(printf '%s' "${dd_body}" | grep -o '"size":[0-9][0-9]*')"
  dd_size="${dd_size#\"size\":}"

  # platform, when present, must be EXACTLY linux/<TARGETARCH>:
  # images.FilterPlatforms includes a child only when its platform is absent or
  # matches, and containerd's transfer store wraps SetChildrenMappedLabels
  # INSIDE that filter, so a non-matching platform still gets an image record
  # but NO containerd.io/gc.ref.content.* labels -- config and layers are
  # unrooted the moment the lease releases. F-IMPORTBLOB through another door.
  dd_arch="$(printf '%s' "${dd_body}" | grep -o '"architecture":"[^"]*"' || true)"
  dd_os="$(printf '%s' "${dd_body}" | grep -o '"os":"[^"]*"' || true)"
  if [ -n "${dd_arch}" ] || [ -n "${dd_os}" ]; then
    if [ "${dd_arch}" != "\"architecture\":\"${TARGETARCH}\"" ] || [ "${dd_os}" != "\"os\":\"linux\"" ]; then
      echo "${dd_fail} index.json platform must be exactly linux/${TARGETARCH} (got ${dd_arch:-<none>} ${dd_os:-<none>})" >&2
      exit 1
    fi
  fi

  desc_mt="${dd_mt}"
  desc_size="${dd_size}"
  desc_digest="${dd_digest}"
}

# derive_blob_order <manifest blob> <descriptor mediaType> <manifest hex> <out>
# Parse an image manifest and write the blob members it owes AFTER itself, in
# canonical order, one "<hex> <size>" per line: the config FIRST, then each
# DISTINCT layer in first-seen manifest order (a repeated layer digest is
# stored once, at its first position -- exactly what the boot checker expects).
# Sets config_digest.
#
# Registry manifests are PRETTY-printed and their key order varies, so the
# document is whitespace-stripped (no value in it contains whitespace), the
# config object and the layers array are cut out BY NAME, and digests are read
# only from those slices: a digest anywhere else (an annotation, an OCI
# subject descriptor) can never be mistaken for a layer. The manifest's bytes
# are already bound to its name by sha256, and the member list derived here is
# compared against the archive's actual members by the caller, so a parse
# differential fails closed instead of selecting different content.
derive_blob_order() {
  db_man="$1"
  db_mt="$2"
  db_hex="$3"
  db_out="$4"
  db_fail="FATAL: ${db_man}:"

  db_bytes="$(wc -c < "${db_man}")"
  if [ "${db_bytes}" -lt 2 ] || [ "${db_bytes}" -gt "${MAX_MANIFEST_BYTES}" ]; then
    echo "${db_fail} manifest blob is ${db_bytes} bytes, want 2..${MAX_MANIFEST_BYTES}" >&2
    exit 1
  fi
  db_flat="$(tr -d ' \t\n\r' < "${db_man}")"
  if [ "$(count_matches "${db_flat}" '"config":{')" != 1 ]; then
    echo "${db_fail} manifest must hold exactly one config object" >&2
    exit 1
  fi
  if [ "$(count_matches "${db_flat}" '"layers":\[')" != 1 ]; then
    echo "${db_fail} manifest must hold exactly one layers array" >&2
    exit 1
  fi

  # What is left once the config object and the layers array are cut out is the
  # manifest's own top level: schemaVersion 2, and a mediaType that a docker
  # schema2 manifest MUST declare (an OCI manifest may omit it).
  db_top="$(printf '%s' "${db_flat}" | sed -e 's#"config":{[^}]*}##' -e 's#"layers":\[[^]]*\]##')"
  case "${db_top}" in
    *'"schemaVersion":2'*) : ;;
    *) echo "${db_fail} manifest schemaVersion is not 2" >&2; exit 1 ;;
  esac
  db_tmt="$(printf '%s' "${db_top}" | grep -o '"mediaType":"[^"]*"' || true)"
  if [ -n "${db_tmt}" ]; then
    if [ "${db_tmt}" != "\"mediaType\":\"${db_mt}\"" ]; then
      echo "${db_fail} manifest mediaType ${db_tmt} does not match index.json's '${db_mt}'" >&2
      exit 1
    fi
  elif [ "${db_mt}" != "${MEDIA_TYPE_OCI_MANIFEST}" ]; then
    echo "${db_fail} manifest omits mediaType but index.json declares '${db_mt}'" >&2
    exit 1
  fi

  # The config descriptor.
  db_cfg="$(printf '%s' "${db_flat}" | sed -n 's#.*"config":{\([^}]*\)}.*#\1#p')"
  if [ "$(count_matches "${db_cfg}" '"digest":')" != 1 ] \
    || [ "$(count_matches "${db_cfg}" '"digest":"sha256:[0-9a-f]\{64\}"')" != 1 ] \
    || [ "$(count_matches "${db_cfg}" '"size":')" != 1 ] \
    || [ "$(count_matches "${db_cfg}" '"size":[0-9][0-9]*')" != 1 ]; then
    echo "${db_fail} the manifest config needs exactly one sha256 digest and one numeric size" >&2
    exit 1
  fi
  db_cfg_digest="$(printf '%s' "${db_cfg}" | grep -o '"digest":"sha256:[0-9a-f]\{64\}"')"
  db_cfg_digest="${db_cfg_digest#\"digest\":\"}"
  db_cfg_digest="${db_cfg_digest%\"}"
  db_cfg_size="$(printf '%s' "${db_cfg}" | grep -o '"size":[0-9][0-9]*')"
  db_cfg_size="${db_cfg_size#\"size\":}"
  if [ "${db_cfg_size}" -lt 1 ] || [ "${db_cfg_size}" -gt "${MAX_CONFIG_BYTES}" ]; then
    echo "${db_fail} manifest config size ${db_cfg_size}, want 1..${MAX_CONFIG_BYTES}" >&2
    exit 1
  fi
  db_cfg_hex="${db_cfg_digest#sha256:}"
  if [ "${db_cfg_hex}" = "${db_hex}" ]; then
    echo "${db_fail} the manifest config is the manifest blob itself" >&2
    exit 1
  fi

  # The layers, in order. Each element is cut onto its own line, then its
  # digest and size are read independently of the key order inside it.
  printf '%s %s\n' "${db_cfg_hex}" "${db_cfg_size}" > "${db_out}"
  printf '%s' "${db_flat}" | sed -n 's#.*"layers":\[\([^]]*\)\].*#\1#p' \
    | awk '{ gsub(/\},\{/, "\n"); print }' > "${db_out}.layers"
  db_n=0
  while IFS= read -r db_line; do
    [ -n "${db_line}" ] || continue
    if [ "$(count_matches "${db_line}" '"digest":')" != 1 ] \
      || [ "$(count_matches "${db_line}" '"digest":"sha256:[0-9a-f]\{64\}"')" != 1 ] \
      || [ "$(count_matches "${db_line}" '"size":')" != 1 ] \
      || [ "$(count_matches "${db_line}" '"size":[0-9][0-9]*')" != 1 ]; then
      echo "${db_fail} layer ${db_n} needs exactly one sha256 digest and one numeric size" >&2
      exit 1
    fi
    db_l_hex="$(printf '%s' "${db_line}" | grep -o '"digest":"sha256:[0-9a-f]\{64\}"')"
    db_l_hex="${db_l_hex#\"digest\":\"sha256:}"
    db_l_hex="${db_l_hex%\"}"
    db_l_size="$(printf '%s' "${db_line}" | grep -o '"size":[0-9][0-9]*')"
    db_l_size="${db_l_size#\"size\":}"
    db_n=$((db_n + 1))
    if [ "${db_l_size}" -lt 1 ]; then
      echo "${db_fail} layer ${db_n} size ${db_l_size} is not positive" >&2
      exit 1
    fi
    # Manifest, config and layer digests are pairwise distinct; only a layer
    # repeating ANOTHER LAYER is allowed, and then with the same size.
    if [ "${db_l_hex}" = "${db_hex}" ] || [ "${db_l_hex}" = "${db_cfg_hex}" ]; then
      echo "${db_fail} layer ${db_n} reuses the manifest or config digest" >&2
      exit 1
    fi
    if grep -q "^${db_l_hex} " "${db_out}"; then
      if ! grep -qx "${db_l_hex} ${db_l_size}" "${db_out}"; then
        echo "${db_fail} layer ${db_n} repeats a digest with a different size" >&2
        exit 1
      fi
      continue
    fi
    printf '%s %s\n' "${db_l_hex}" "${db_l_size}" >> "${db_out}"
  done < "${db_out}.layers"
  if [ "${db_n}" -lt 1 ]; then
    echo "${db_fail} manifest names no layers" >&2
    exit 1
  fi
  if [ "${db_n}" -gt "${MAX_MEMBERS}" ]; then
    echo "${db_fail} manifest names ${db_n} layers, more than ${MAX_MEMBERS}" >&2
    exit 1
  fi

  config_digest="${db_cfg_digest}"
}

# derive_canonical <layout dir> <work dir> <label> <ref>
# Derive the canonical member list of an OCI image layout DIRECTORY, from its
# own bytes, and prove the layout is closed over it. Leaves:
#   <work>/<label>.order  the member names in canonical order (tar -T input)
#   <work>/<label>.pairs  "<name> <size>" per member, same order
# and sets desc_mt/desc_size/desc_digest, config_digest and manifest_hex. The ORDER is load-bearing (the boot check is a single
# streaming pass) and so is the CLOSURE: the blobs the manifest names are
# exactly the blobs present -- the check docker-save made impossible.
# The oci-layout and index.json sizes recorded in .pairs are the CANONICAL
# rendering's lengths, not whatever is on disk, so a stale or crane-written
# index.json cannot pass the caller's comparison.
derive_canonical() {
  dc_dir="$1"
  dc_work="$2"
  dc_label="$3"
  dc_ref="$4"
  dc_order="${dc_work}/${dc_label}.order"
  dc_pairs="${dc_work}/${dc_label}.pairs"
  dc_fail="FATAL: ${dc_dir}:"

  derive_descriptor "${dc_dir}/index.json"
  manifest_hex="${desc_digest#sha256:}"
  dc_man="${dc_dir}/blobs/sha256/${manifest_hex}"
  if [ ! -f "${dc_man}" ] || [ -L "${dc_man}" ]; then
    echo "${dc_fail} index.json names manifest ${desc_digest}, which is not a blob of this layout" >&2
    exit 1
  fi
  dc_man_size="$(wc -c < "${dc_man}")"
  if [ "${dc_man_size}" != "${desc_size}" ]; then
    echo "${dc_fail} manifest blob is ${dc_man_size} bytes, index.json declares ${desc_size}" >&2
    exit 1
  fi
  dc_man_sha="$(sha256sum "${dc_man}" | cut -d' ' -f1)"
  if [ "${dc_man_sha}" != "${manifest_hex}" ]; then
    echo "${dc_fail} manifest blob has sha256 ${dc_man_sha}, named ${manifest_hex}" >&2
    exit 1
  fi
  derive_blob_order "${dc_man}" "${desc_mt}" "${manifest_hex}" "${dc_work}/${dc_label}.blobs"

  # The config declares the platform it was BUILT for; assert it here, bound to
  # content, rather than trusting index.json's platform field alone.
  dc_cfg="${dc_dir}/blobs/sha256/${config_digest#sha256:}"
  if [ ! -f "${dc_cfg}" ] || [ -L "${dc_cfg}" ]; then
    echo "${dc_fail} the manifest names config ${config_digest}, which is not a blob of this layout" >&2
    exit 1
  fi
  if [ "$(grep -Eo '"architecture":[[:space:]]*"[^"]*"' "${dc_cfg}" | grep -c .)" != 1 ] \
    || [ "$(grep -Eo "\"architecture\":[[:space:]]*\"${TARGETARCH}\"" "${dc_cfg}" | grep -c .)" != 1 ] \
    || [ "$(grep -Eo '"os":[[:space:]]*"[^"]*"' "${dc_cfg}" | grep -c .)" != 1 ] \
    || [ "$(grep -Eo '"os":[[:space:]]*"linux"' "${dc_cfg}" | grep -c .)" != 1 ]; then
    echo "${dc_fail} the image config does not declare exactly linux/${TARGETARCH}" >&2
    grep -Eo '"(architecture|os)":[[:space:]]*"[^"]*"' "${dc_cfg}" >&2 || true
    exit 1
  fi

  # Canonical order and the per-member sizes.
  {
    printf 'oci-layout\nindex.json\nblobs/sha256/%s\n' "${manifest_hex}"
    awk '{ print "blobs/sha256/" $1 }' "${dc_work}/${dc_label}.blobs"
  } > "${dc_order}"
  {
    printf 'oci-layout %s\n' "$(render_oci_layout | wc -c | tr -d ' ')"
    printf 'index.json %s\n' \
      "$(render_index "${desc_mt}" "${desc_size}" "${desc_digest}" "${dc_ref}" | wc -c | tr -d ' ')"
    printf 'blobs/sha256/%s %s\n' "${manifest_hex}" "${desc_size}"
    awk '{ print "blobs/sha256/" $1 " " $2 }' "${dc_work}/${dc_label}.blobs"
  } > "${dc_pairs}"

  dc_count="$(wc -l < "${dc_order}")"
  if [ "${dc_count}" -lt "${MIN_MEMBERS}" ] || [ "${dc_count}" -gt "${MAX_MEMBERS}" ]; then
    echo "${dc_fail} the layout implies ${dc_count} members, want ${MIN_MEMBERS}..${MAX_MEMBERS}" >&2
    exit 1
  fi

  # Closure: the blobs the manifest names are EXACTLY the blobs present.
  grep '^blobs/sha256/' "${dc_order}" | sort > "${dc_work}/${dc_label}.want-blobs"
  (cd "${dc_dir}" && find blobs -mindepth 1 ! -type d | sort) > "${dc_work}/${dc_label}.have-blobs"
  if ! cmp -s "${dc_work}/${dc_label}.want-blobs" "${dc_work}/${dc_label}.have-blobs"; then
    echo "${dc_fail} the blobs present are not exactly the manifest, config and layers it names (< wanted, > present):" >&2
    diff "${dc_work}/${dc_label}.want-blobs" "${dc_work}/${dc_label}.have-blobs" >&2 || true
    exit 1
  fi
}

# validate_image_tar <tarball> <expected ref> <work dir> <label>
# Fail-closed validator for ONE canonical OCI image-layout archive
# (ADR-16-A3), run on the tarball that is about to be shipped:
#   (a) 3..256 members, every one a regular file whose name is byte-exactly
#       "oci-layout", "index.json" or "blobs/sha256/<64 lowercase hex>", with
#       no "manifest.json" under any spelling, no absolute/"./"/".."/"//"/
#       trailing-slash/backslash/NUL name, and unique both exactly and after
#       path.Clean (containerd keys members on the CLEANED name and a later
#       duplicate silently OVERWRITES an earlier one);
#   (b) the canonical tar profile: uid/gid 0, mode 0644, mtime epoch 0, in the
#       fixed order oci-layout, index.json, manifest, config, layers;
#   (c) extracted into an empty directory, every blob is a singly-linked
#       regular file whose sha256 equals the hex in its name;
#   (d) oci-layout is byte-exactly render_oci_layout, and index.json is
#       byte-exactly render_index re-rendered from the ARCHIVE's own manifest
#       member, carrying EXACTLY ONE annotation: ref.name = <expected ref>;
#   (e) the index -> manifest -> config/layers closure holds: the blobs the
#       manifest names, in manifest order with each distinct digest once, are
#       exactly the blob members, and every member's size equals the size its
#       descriptor declares.
# Sets desc_digest (the per-platform manifest digest images.lock records) and
# config_digest. Leaves <work>/<label>.names, .listing and .x/ behind.
validate_image_tar() {
  vt_tar="$1"
  vt_expect="$2"
  vt_work="$3"
  vt_label="$4"
  vt_names="${vt_work}/${vt_label}.names"
  vt_listing="${vt_work}/${vt_label}.listing"
  vt_x="${vt_work}/${vt_label}.x"
  vt_fail="FATAL: ${vt_tar} (expected ref ${vt_expect}):"

  # (a) member names and types. busybox tar silently rewrites unsafe names when
  # it lists or extracts (it strips a leading "/" and "../" prefixes, warning
  # once on stderr), so ANY stderr output from tar is FATAL: the names seen
  # below must be the names actually stored.
  tar -tf "${vt_tar}" > "${vt_names}" 2> "${vt_work}/${vt_label}.tar-stderr"
  tar -tvf "${vt_tar}" > "${vt_listing}" 2>> "${vt_work}/${vt_label}.tar-stderr"
  if [ -s "${vt_work}/${vt_label}.tar-stderr" ]; then
    echo "${vt_fail} tar reported:" >&2
    cat "${vt_work}/${vt_label}.tar-stderr" >&2
    exit 1
  fi
  vt_count="$(wc -l < "${vt_names}")"
  if [ "${vt_count}" -lt "${MIN_MEMBERS}" ] || [ "${vt_count}" -gt "${MAX_MEMBERS}" ]; then
    echo "${vt_fail} ${vt_count} members, want ${MIN_MEMBERS}..${MAX_MEMBERS}" >&2
    exit 1
  fi
  # A NUL inside a member name would truncate it for every line-based check.
  if [ "$(tr -d '\000' < "${vt_names}" | wc -c)" != "$(wc -c < "${vt_names}")" ]; then
    echo "${vt_fail} a member name contains a NUL byte" >&2
    exit 1
  fi
  # Format confusion: containerd takes the OCI branch and returns EARLY when
  # oci-layout is present, so an archive carrying BOTH oci-layout and
  # manifest.json validates as docker-save and imports as OCI. Refused first,
  # for a precise message; the allowlist below refuses it again.
  if sed -e 's#^\(\./\)*##' -e 's#//*#/#g' -e 's#/$##' "${vt_names}" | grep -qx 'manifest\.json'; then
    echo "${vt_fail} a manifest.json member is present; an OCI layout must never carry one" >&2
    exit 1
  fi
  # Traversal and aliasing hazards, named explicitly for diagnosis. The
  # anchored allowlist that follows is what actually enforces them; it replaces
  # the pre-A3 "no member name contains a slash" rule, which every
  # blobs/sha256/<hex> name breaks.
  if grep -E '^/|^\./|//|/$|\\|(^|/)\.\.?($|/)' "${vt_names}" >&2; then
    echo "${vt_fail} the member name(s) above are absolute, aliasing, or a directory (trailing '/')" >&2
    exit 1
  fi
  if grep -Evx 'oci-layout|index\.json|blobs/sha256/[0-9a-f]{64}' "${vt_names}" >&2; then
    echo "${vt_fail} unexpected member name(s) above" >&2
    exit 1
  fi
  if [ -n "$(sort "${vt_names}" | uniq -d)" ]; then
    echo "${vt_fail} duplicate member names: $(sort "${vt_names}" | uniq -d | tr '\n' ' ')" >&2
    exit 1
  fi
  # ... and duplicates after cleaning: "index.json" and "./index.json" are ONE
  # key to containerd and two distinct strings to a naive seen-set. Every name
  # that survived the refusals above is already its own clean form, so this is
  # defence in depth against a future loosening of them ("..", refused
  # outright, is the one element a sed cannot resolve).
  sed -e 's#^\(\./\)*##' -e 's#//*#/#g' -e 's#/$##' "${vt_names}" > "${vt_work}/${vt_label}.clean"
  if [ -n "$(sort "${vt_work}/${vt_label}.clean" | uniq -d)" ]; then
    echo "${vt_fail} member names that collide after path.Clean: $(sort "${vt_work}/${vt_label}.clean" | uniq -d | tr '\n' ' ')" >&2
    exit 1
  fi
  # busybox tar -tv: "<mode> <owner> <size> <date> <time> <name>"; a symlink or
  # hardlink appends " -> <target>" (a hardlink keeps a '-' mode), so require
  # exactly six fields, a '-' type, and the name sequence to equal tar -tf's.
  if awk 'NF != 6 || substr($1, 1, 1) != "-"' "${vt_listing}" | grep . >&2 \
    || ! awk '{ print $6 }' "${vt_listing}" | cmp -s - "${vt_names}"; then
    echo "${vt_fail} every member must be a regular file (non-regular or mismatched listing above)" >&2
    exit 1
  fi
  # (b) the canonical tar profile: mode 0644, root-owned, mtime epoch 0, so the
  # archive carries no build time and no local umask. busybox tar prints the
  # header's uname/gname when the archive carries them ("root/root", what busybox
  # itself writes for uid/gid 0 here) and the numbers otherwise ("0/0"), so both
  # spellings of root are accepted; the uid/gid are asserted NUMERICALLY on disk
  # before the archive is created, and scripts/verify-bundle-refs.sh re-asserts
  # 0/0 numerically from the shipped archive with GNU tar --numeric-owner.
  if awk '$1 != "-rw-r--r--" || ($2 != "root/root" && $2 != "0/0") || $4 != "1970-01-01" || $5 != "00:00:00"' \
    "${vt_listing}" | grep . >&2; then
    echo "${vt_fail} the member(s) above are not uid/gid 0, mode 0644, mtime epoch 0" >&2
    exit 1
  fi

  # (c) extract into a fresh, empty directory; bind each blob to its name.
  # blobs/ and blobs/sha256/ are created by the extraction as implied parents:
  # the archive itself carries NO directory member (asserted above, where every
  # member's listed type must be '-').
  mkdir "${vt_x}"
  tar -xf "${vt_tar}" -C "${vt_x}" 2> "${vt_work}/${vt_label}.tar-stderr"
  if [ -s "${vt_work}/${vt_label}.tar-stderr" ]; then
    echo "${vt_fail} tar extraction reported:" >&2
    cat "${vt_work}/${vt_label}.tar-stderr" >&2
    exit 1
  fi
  if [ "$(find "${vt_x}" -mindepth 1 -type f | wc -l)" != "${vt_count}" ] \
    || [ -n "$(find "${vt_x}" -mindepth 1 ! -type f ! -type d)" ] \
    || [ "$(cd "${vt_x}" && find . -mindepth 1 -type d | sort | tr '\n' ' ')" != "./blobs ./blobs/sha256 " ]; then
    echo "${vt_fail} extraction produced entries other than the ${vt_count} listed regular files under blobs/sha256" >&2
    find "${vt_x}" -mindepth 1 >&2
    exit 1
  fi
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
    case "${vt_name}" in
      blobs/sha256/*) : ;;
      *) continue ;;
    esac
    vt_hex="${vt_name#blobs/sha256/}"
    vt_sha="$(sha256sum "${vt_path}" | cut -d' ' -f1)"
    if [ "${vt_sha}" != "${vt_hex}" ]; then
      echo "${vt_fail} blob ${vt_name} has sha256 ${vt_sha}" >&2
      exit 1
    fi
  done < "${vt_names}"

  # (d) oci-layout exact bytes.
  render_oci_layout > "${vt_work}/${vt_label}.oci-layout.want"
  if ! cmp -s "${vt_work}/${vt_label}.oci-layout.want" "${vt_x}/oci-layout"; then
    echo "${vt_fail} oci-layout is not the exact 37 canonical bytes:" >&2
    od -c "${vt_x}/oci-layout" >&2
    exit 1
  fi

  # (e) index.json, the manifest it names, and the whole member list: derived
  # from the ARCHIVE's own bytes and compared, which is what binds names,
  # order, sizes and the closure in one comparison.
  derive_canonical "${vt_x}" "${vt_work}" "${vt_label}" "${vt_expect}"
  vt_idx="$(cat "${vt_x}/index.json")"
  vt_ann="\"annotations\":{\"${ANNOTATION_REF_NAME}\":\"${vt_expect}\"}}]}"
  case "${vt_idx}" in
    *"${vt_ann}")
      if [ "$(count_matches "${vt_idx}" '"annotations":{')" != 1 ] \
        || [ "$(count_matches "${vt_idx}" '"'"${ANNOTATION_REF_NAME}"'":')" != 1 ]; then
        echo "${vt_fail} index.json must carry EXACTLY ONE annotation, ${ANNOTATION_REF_NAME}" >&2
        exit 1
      fi
      ;;
    *)
      echo "${vt_fail} index.json does not end with exactly the one annotation ${ANNOTATION_REF_NAME}=${vt_expect}; got:" >&2
      printf '%s\n' "${vt_idx}" >&2
      exit 1
      ;;
  esac
  render_index "${desc_mt}" "${desc_size}" "${desc_digest}" "${vt_expect}" \
    > "${vt_work}/${vt_label}.index.want"
  if ! cmp -s "${vt_work}/${vt_label}.index.want" "${vt_x}/index.json"; then
    echo "${vt_fail} index.json is not the canonical compact rendering:" >&2
    printf 'want: ' >&2; cat "${vt_work}/${vt_label}.index.want" >&2; echo >&2
    printf 'got:  ' >&2; cat "${vt_x}/index.json" >&2; echo >&2
    exit 1
  fi
  if ! awk '{ print $6, $3 }' "${vt_listing}" | cmp -s - "${vt_work}/${vt_label}.pairs"; then
    echo "${vt_fail} members are not exactly oci-layout, index.json, the manifest, its config and its layers, in that order and with those sizes (< archive, > wanted):" >&2
    awk '{ print $6, $3 }' "${vt_listing}" > "${vt_work}/${vt_label}.got-pairs"
    diff "${vt_work}/${vt_label}.got-pairs" "${vt_work}/${vt_label}.pairs" >&2 || true
    exit 1
  fi
}

# bundle_image <ref> <digest> <dest tarball>
# Pull ref@digest for TARGETARCH as an OCI image layout into a scratch
# directory outside OUT_DIR, replace crane's index.json with the canonical one
# (crane writes NO org.opencontainers.image.ref.name annotation, and without it
# containerd's importer creates no named image record at all), archive it to the
# canonical profile and move it to <dest> ONLY after the finished tarball
# re-validates on its own bytes. Sets bundled_sha, bundled_manifest_digest and
# bundled_config_digest from that validation.
bundle_image() {
  bi_ref="$1"
  bi_digest="$2"
  bi_dest="$3"

  bi_work="$(mktemp -d)"
  trap 'rm -rf "${bi_work}"' EXIT
  case "${bi_work%/}/" in
    "${OUT_DIR%/}/"*) echo "FATAL: scratch directory ${bi_work} is under OUT_DIR ${OUT_DIR}" >&2; exit 1 ;;
  esac
  if [ -e "${bi_dest}" ]; then
    echo "FATAL: ${bi_dest} already exists (two refs map to one tarball name?)" >&2
    exit 1
  fi

  # The digest is the cosign-verified INDEX digest; --platform selects the
  # TARGETARCH manifest from that content-addressed index. crane writes the
  # layer blobs COMPRESSED and named by their registry digest, and the config
  # and manifest blobs as the registry's own bytes, so nothing below rewrites a
  # single byte the registry served -- the whole F-IMPORTBLOB bug was that
  # docker-save carries NO manifest document, leaving containerd to
  # RE-SERIALIZE one whose digest exists in no registry.
  bi_lay="${bi_work}/layout"
  retry crane pull --format=oci --platform "linux/${TARGETARCH}" \
    "${bi_ref}@${bi_digest}" "${bi_lay}"
  if [ "$(cd "${bi_lay}" && find . -mindepth 1 ! -type d ! -type f | wc -l)" != 0 ] \
    || [ "$(cd "${bi_lay}" && find . -mindepth 1 -maxdepth 1 ! -name oci-layout ! -name index.json ! -name blobs | wc -l)" != 0 ] \
    || [ "$(cd "${bi_lay}" && find . -mindepth 1 -type d | sort | tr '\n' ' ')" != "./blobs ./blobs/sha256 " ]; then
    echo "FATAL: crane wrote something other than an OCI image layout for ${bi_ref}@${bi_digest}:" >&2
    find "${bi_lay}" >&2
    exit 1
  fi
  echo "pulled ${bi_ref}@${bi_digest} (linux/${TARGETARCH}) as an OCI image layout"

  # Derive the canonical member list from crane's own layout (this reads
  # crane's pretty-printed index.json), then write OUR oci-layout and
  # index.json over crane's: same manifest, same blobs, plus the one annotation
  # containerd names the image from, and nothing crane adds that the boot
  # checker does not know (it decodes index.json with DisallowUnknownFields,
  # and crane v0.22.1 emits an artifactType field).
  derive_canonical "${bi_lay}" "${bi_work}" layout "${bi_ref}"
  render_oci_layout > "${bi_lay}/oci-layout"
  render_index "${desc_mt}" "${desc_size}" "${desc_digest}" "${bi_ref}" > "${bi_lay}/index.json"
  bi_manifest_digest="${desc_digest}"

  # The canonical tar PROFILE: fixed member order, no directory members,
  # uid/gid 0, mode 0644, mtime epoch 0. busybox tar has no --owner/--mode/
  # --mtime, so the metadata is normalized on disk first and asserted here;
  # tar then stores exactly what it finds. Reproducible for a given set of
  # registry blobs, independent of the build time and umask.
  chown -R 0:0 "${bi_lay}"
  find "${bi_lay}" -mindepth 1 -type f -exec chmod 0644 {} +
  find "${bi_lay}" -mindepth 1 -type f -exec touch -d @0 {} +
  if [ -n "$(find "${bi_lay}" -mindepth 1 -type f ! \( -user 0 -group 0 -perm 0644 \))" ]; then
    echo "FATAL: could not normalize the ${bi_ref} layout to root:root 0644:" >&2
    find "${bi_lay}" -mindepth 1 -type f ! \( -user 0 -group 0 -perm 0644 \) >&2
    exit 1
  fi
  tar -cf "${bi_work}/final.tar" -C "${bi_lay}" --no-recursion -T "${bi_work}/layout.order"

  # Re-read the FINISHED tarball and hold it to the whole contract, from its
  # own bytes: names, order, profile, blob digests, the canonical oci-layout
  # and index.json, and the index -> manifest -> config/layers closure.
  bi_checked_sha="$(sha256sum "${bi_work}/final.tar" | cut -d' ' -f1)"
  validate_image_tar "${bi_work}/final.tar" "${bi_ref}" "${bi_work}" final
  if [ "${desc_digest}" != "${bi_manifest_digest}" ]; then
    echo "FATAL: ${bi_ref} archive names manifest ${desc_digest}, the pull resolved ${bi_manifest_digest}" >&2
    exit 1
  fi

  mv "${bi_work}/final.tar" "${bi_dest}"
  bundled_sha="$(sha256sum "${bi_dest}" | cut -d' ' -f1)"
  if [ "${bundled_sha}" != "${bi_checked_sha}" ]; then
    echo "FATAL: ${bi_dest} sha256 ${bundled_sha} != validated ${bi_checked_sha}" >&2
    exit 1
  fi
  bundled_manifest_digest="${desc_digest}"
  bundled_config_digest="${config_digest}"
  echo "bundled ${bi_ref} as an OCI layout: manifest=${bundled_manifest_digest} config=${bundled_config_digest} final=${bundled_sha}"
  rm -rf "${bi_work}"
  trap - EXIT
}

# extract_etcd_tools <ref> <index digest> <manifest digest> <config digest>
#                    <tarball> <tarball-sha256>
# Install etcdctl + etcdutl into TOOLS_DIR from the etcd image the bundle
# carries. The image filesystem is flattened with `crane export ref@<manifest
# digest>` -- the per-platform manifest digest the BUNDLED archive names, so the
# export is content-addressed to the bundled image (export from stdin would
# read docker-save only, which ADR-16-A3 no longer produces). The flattened
# image's config digest MUST equal the one the bundled archive's manifest
# names, so the binaries are provably from the bundled image; a mismatch is
# FATAL. Every other deviation is FATAL too: the tarball must be unchanged
# since its pull, each member must appear exactly once as a regular file, and
# each installed binary must be a root-owned 0755 (no setuid/setgid) ELF for
# TARGETARCH that EXECUTES here (musl-runnable) and reports the etcd image
# tag's version (3.7.0-0 -> "etcdctl version: 3.7.0").
extract_etcd_tools() {
  xe_ref="$1"
  xe_digest="$2"
  xe_manifest_digest="$3"
  xe_config_digest="$4"
  xe_tar="$5"
  xe_tar_sha="$6"

  xe_tag="${xe_ref##*:}"
  if ! printf '%s\n' "${xe_tag}" | grep -Eqx '[0-9]+\.[0-9]+\.[0-9]+-[0-9]+'; then
    echo "FATAL: etcd image tag '${xe_tag}' (${xe_ref}) is not <major>.<minor>.<patch>-<N>" >&2
    exit 1
  fi
  xe_want="${xe_tag%-*}"
  for xe_d in "${xe_manifest_digest}" "${xe_config_digest}"; do
    if ! printf '%s\n' "${xe_d}" | grep -Eqx 'sha256:[0-9a-f]{64}'; then
      echo "FATAL: etcd digest '${xe_d}' is not sha256:<64 lowercase hex>" >&2
      exit 1
    fi
  done

  # The tarball must be byte-identical to what bundle_image validated and moved
  # into OUT_DIR for the verified digest (no rewrite between then and here),
  # which is what makes the digests passed in describe THIS file.
  xe_got_sha="$(sha256sum "${xe_tar}" | cut -d' ' -f1)"
  if [ "${xe_got_sha}" != "${xe_tar_sha}" ]; then
    echo "FATAL: ${xe_tar} changed since it was bundled (sha256 ${xe_got_sha} != ${xe_tar_sha})" >&2
    exit 1
  fi

  xe_scratch="$(mktemp -d)"
  trap 'rm -rf "${xe_scratch}"' EXIT
  # Fail closed on provenance BEFORE touching the filesystem: the config blob
  # for the manifest the bundled archive names must be the config the bundled
  # archive carries. `crane config` prints the raw config blob, so its sha256
  # IS the config digest.
  retry crane config "${xe_ref}@${xe_manifest_digest}" > "${xe_scratch}/config.json"
  xe_cfg_sha="$(sha256sum "${xe_scratch}/config.json" | cut -d' ' -f1)"
  if [ "sha256:${xe_cfg_sha}" != "${xe_config_digest}" ]; then
    echo "FATAL: ${xe_ref}@${xe_manifest_digest} has config sha256:${xe_cfg_sha}, the bundled archive names ${xe_config_digest}; refusing to extract etcdctl/etcdutl" >&2
    exit 1
  fi
  echo "etcd-tools: ${xe_ref}@${xe_manifest_digest} config ${xe_config_digest} matches the bundled archive"
  # Flatten the image filesystem, pinned to that same manifest digest.
  retry crane export "${xe_ref}@${xe_manifest_digest}" "${xe_scratch}/fs.tar"
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
  digest="$(retry crane digest "${ref}")"
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
  #    names the tarball's image with the exact ref -- in index.json's
  #    org.opencontainers.image.ref.name annotation -- before it enters OUT_DIR, and
  #    reports the per-platform manifest digest that index.json names.
  f="${OUT_DIR}/$(echo "${ref}" | tr '/:' '__').tar"
  bundle_image "${ref}" "${digest}" "${f}"
  tarball="$(basename "${f}")"
  # Derived from the validated archive, never from a second registry round trip,
  # and held to the lock's own digest rule before it is written.
  if ! printf '%s\n' "${bundled_manifest_digest}" | grep -Eqx 'sha256:[0-9a-f]{64}'; then
    echo "FATAL: ${ref} bundled manifest digest '${bundled_manifest_digest}' is not sha256:<64 lowercase hex>" >&2
    exit 1
  fi

  # Record the etcd entry for the etcdctl/etcdutl extraction below, binding the
  # bytes of the final (validated) tarball as moved into OUT_DIR and the manifest
  # and config digests that tarball names.
  case "${ref}" in
    */etcd:*)
      etcd_count=$((etcd_count + 1))
      etcd_ref="${ref}"
      etcd_digest="${digest}"
      etcd_manifest_digest="${bundled_manifest_digest}"
      etcd_config_digest="${bundled_config_digest}"
      etcd_tar="${f}"
      etcd_tar_sha="${bundled_sha}"
      etcd_verified="${verified}"
      ;;
  esac

  entry="$(printf '    {"ref": "%s", "digest": "%s", "manifestDigest": "%s", "tarball": "%s", "verified": %s, "verifyReason": "%s"}' \
    "${ref}" "${digest}" "${bundled_manifest_digest}" "${tarball}" "${verified}" "${reason}")"
  if [ -z "${entries}" ]; then
    entries="${entry}"
  else
    entries="$(printf '%s,\n%s' "${entries}" "${entry}")"
  fi
  n=$((n + 1))
  echo "bundled ${ref}@${digest} (manifest ${bundled_manifest_digest}) -> ${tarball} (verified=${verified})"
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
# (internal/imageimport/testdata/images.lock.<version>) in the same change. The
# release attestation predicate is these bytes too.
#
# manifestDigest (ADR-16-A3) is the per-platform manifest digest the bundled
# archive's index.json names, and the boot importer PINS the archive to it
# (CheckTarPinned): it is the only runtime comparison between the bundle and
# what cosign verified here, since `digest` -- the INDEX digest cosign verified
# -- is otherwise declarative only. It therefore makes images.lock
# ARCH-SPECIFIC: an amd64 bundle records the linux/amd64 child manifest, an
# arm64 bundle the linux/arm64 one, for the same ref and the same `digest`.
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
extract_etcd_tools "${etcd_ref}" "${etcd_digest}" "${etcd_manifest_digest}" \
  "${etcd_config_digest}" "${etcd_tar}" "${etcd_tar_sha}"
