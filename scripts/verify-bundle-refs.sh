#!/usr/bin/env bash
# verify-bundle-refs.sh <image> <kubernetes-version>
#
# Verify the pre-bundled control-plane images of a built provider-kubernetes image:
# where they live and with what owner and mode (ADR-16-A2), that every tarball is
# named with its EXACT ref (F-IMPORTREF, ADR-16-A1), and that the image's own boot
# importer accepts the bundle. CI runs it on every supported minor and the release
# workflow runs it before `docker push`, so both gates run the same checks.
#
# Each tarball is a canonical single-platform OCI image LAYOUT (ADR-16-A3), never
# docker-save: `ctr images import` names the image from index.json's
# org.opencontainers.image.ref.name annotation, and kubeadm (CRI ImageStatus) and
# containerd's sandbox_image look images up by the EXACT ref, so an air-gap first
# boot needs every archive to carry exactly the ref kubeadm expects. At boot,
# `import-images` imports only the images.lock entries, pins each archive to that
# entry's manifestDigest, and refuses a bundle or tarball whose path, owner, mode,
# file type, filesystem or structure is wrong. Asserted against the SHIPPED image:
#   (0) /opt/provider-kubernetes does not exist (the bundle is image-only, and /opt
#       is persistent on Kairos); every directory the importer walks (/, /system,
#       /system/providers, /system/provider-kubernetes and its images/) is a
#       root-owned directory with no mode bits beyond 0755; the provider binary is
#       a root-owned regular file without group/other write or setuid/setgid;
#   (1) every copied-out entry (the bundle directory, images.lock, the tarballs and
#       containerd's config.toml) is a regular file, BEFORE anything parses it, and
#       in the image images.lock and every tarball are root:root 0644 regular files;
#   (2) images.lock refs == the bundled kubeadm's image list for this release;
#   (3) the *.tar files == the images.lock tarballs (nothing else present);
#   (4) containerd's sandbox_image is one of the bundled refs;
#   (5) each tarball is one canonical OCI image layout: unique, allowlisted,
#       regular members in the fixed order oci-layout, index.json, manifest,
#       config, layers, on the canonical profile (uid/gid 0, mode 0644, mtime
#       epoch 0); byte-exact oci-layout and index.json; index.json names exactly
#       the lock's manifestDigest; and the index -> manifest -> config/layers
#       closure holds, with every member's size equal to its descriptor's;
#   (6) index.json names the lock's ref in its one
#       org.opencontainers.image.ref.name annotation, and no docker-save artefact
#       (a manifest.json member, RepoTags bytes) exists anywhere in the bundle;
#   (7) the bundle stays within HALF of every boot importer bound;
#   (8) the image's own `import-images --verify-only` exits 0, and its last output
#       line is the summary with outcome=verified and entries == images.lock's.
# Exact bytes, never a JSON projection: containerd's importer matches keys
# case-insensitively, does not bind member bytes to member names, keys members on
# path.Clean(name) where a later duplicate silently overwrites, and takes the OCI
# branch (returning early) whenever oci-layout is present. jq only reads
# images.lock, which the build itself generates. (8) adds what only the Go importer
# checks: PAX/sparse headers, the lock's canonical bytes and the filesystem of
# every file. Layer blobs are deliberately NOT hashed here, as at boot: the
# content store is keyed by the COMPUTED digest, so a layer whose bytes do not
# match its path is unreachable and the import fails closed. The build
# (build/bundle-images.sh) does hash every layer, once, where the bytes arrive.
#
# Requires: bash, docker, jq, GNU tar, GNU coreutils/findutils/sed (ubuntu-latest).
set -euo pipefail
export LC_ALL=C

# Paths the boot importer uses (internal/hostexec BundleDir and ProviderBinaryPath).
readonly BUNDLE_DIR=/system/provider-kubernetes/images
readonly PROVIDER_BINARY=/system/providers/agent-provider-kubernetes
# Where images were bundled before ADR-16-A2. /opt is persistent on Kairos, so a
# bundle there could be changed on the node; nothing may ship there any more.
readonly LEGACY_BUNDLE_PATH=/opt/provider-kubernetes
# Every directory the importer walks, from / to the bundle and to the provider binary.
readonly WALK_DIRS=(/ /system /system/providers /system/provider-kubernetes "$BUNDLE_DIR")

# HALF of each boot importer bound (ADR-16-A2 decision 3): the shipped bundle must
# stay within half of every limit, so growth (a new image, bigger layers) fails CI
# long before a boot could refuse the bundle. Lower bounds are the importer's own.
readonly MAX_DIR_ENTRIES=512                         # importer: 1024 entries
readonly MAX_LOCK_BYTES=$((32 * 1024))               # importer: 64 KiB
readonly MIN_ENTRIES=1 MAX_ENTRIES=16                # importer: 1..32
readonly MIN_TARBALL_BYTES=2560                      # importer: 2560 bytes
readonly MAX_TARBALL_BYTES=$((512 * 1024 * 1024))    # importer: 1 GiB
readonly MAX_TOTAL_BYTES=$((2 * 1024 * 1024 * 1024)) # importer: 4 GiB
readonly MIN_MEMBERS=3 MAX_MEMBERS=128               # importer: 3..256
readonly MAX_INDEX_BYTES=$((4 * 1024))               # importer: 8 KiB
readonly MAX_MANIFEST_BYTES=$((32 * 1024))           # importer: 64 KiB
readonly MAX_CONFIG_BYTES=$((512 * 1024))            # importer: 1 MiB

# The canonical archive's fixed bytes and names (ADR-16-A3). These MUST equal
# internal/imageimport/tarcheck.go's and build/bundle-images.sh's.
readonly OCI_LAYOUT_BYTES='{
    "imageLayoutVersion": "1.0.0"
}'
readonly MEDIA_TYPE_OCI_INDEX='application/vnd.oci.image.index.v1+json'
readonly MEDIA_TYPE_DOCKER_INDEX='application/vnd.docker.distribution.manifest.list.v2+json'
readonly MEDIA_TYPE_OCI_MANIFEST='application/vnd.oci.image.manifest.v1+json'
readonly MEDIA_TYPE_DOCKER_MANIFEST='application/vnd.docker.distribution.manifest.v2+json'
readonly ANNOTATION_REF_NAME='org.opencontainers.image.ref.name'
# containerd's imageName() reads this annotation FIRST and only falls back to
# ref.name, so an archive carrying it would choose the imported record's NAME
# itself while a ref.name-only check reported success.
readonly ANNOTATION_CONTAINERD_NAME='io.containerd.image.name'

# parse_sandbox_image <config.toml>: print containerd's sandbox_image, or explain on
# stderr and return 1. This is the rule test/e2e/bundled_images.go parseSandboxImage
# applies, and a parity test runs both on the same inputs, so CI and the e2e gate
# cannot disagree about which line counts:
#   - the file holds no NUL byte;
#   - each line is trimmed of ASCII whitespace; blank lines and lines starting with
#     '#' are ignored;
#   - a line starting with '[' opens a table, named by the line without its
#     leading/trailing '[' and ']' characters and surrounding whitespace;
#   - every other line starting with sandbox_image must be exactly
#     sandbox_image = "<value>" (whitespace around '=', no '"' or '\' in the value,
#     and nothing after the closing quote, not even a comment), inside
#     [plugins."io.containerd.grpc.v1.cri"];
#   - there is exactly one such line.
# A trailing comment is refused rather than stripped: the Dockerfile rewrites the
# whole line, so a comment can only come from an edit that bypassed that rewrite.
parse_sandbox_image() {
  local -r cfg="$1"
  local -r cri='plugins."io.containerd.grpc.v1.cri"'
  # Go's \s: tab, LF, FF, CR, space. [:space:] below is, under LC_ALL=C, exactly
  # space, tab, LF, VT, FF and CR: the set the Go side trims.
  local -r assign_re=$'^sandbox_image[\t\n\f\r ]*=[\t\n\f\r ]*"([^"\\]+)"$'
  local line s table="" value="" n=0 lineno=0
  if [ "$(tr -cd '\000' < "$cfg" | wc -c)" -ne 0 ]; then
    echo "config.toml contains a NUL byte" >&2
    return 1
  fi
  while IFS= read -r line || [ -n "$line" ]; do
    lineno=$((lineno + 1))
    s="${line#"${line%%[![:space:]]*}"}"
    s="${s%"${s##*[![:space:]]}"}"
    if [ -z "$s" ] || [[ "$s" == \#* ]]; then
      continue
    fi
    if [[ "$s" == \[* ]]; then
      table="$s"
      while [[ "$table" == [][]* ]]; do table="${table:1}"; done
      while [[ "$table" == *[][] ]]; do table="${table%?}"; done
      table="${table#"${table%%[![:space:]]*}"}"
      table="${table%"${table##*[![:space:]]}"}"
      continue
    fi
    [[ "$s" == sandbox_image* ]] || continue
    if ! [[ "$s" =~ $assign_re ]]; then
      echo "config.toml line ${lineno}: unsupported sandbox_image assignment: ${s}" >&2
      return 1
    fi
    if [ "$table" != "$cri" ]; then
      echo "config.toml line ${lineno}: sandbox_image is in [${table}], want [${cri}]" >&2
      return 1
    fi
    value="${BASH_REMATCH[1]}"
    n=$((n + 1))
  done < "$cfg"
  if [ "$n" -ne 1 ]; then
    echo "config.toml has ${n} sandbox_image assignments, want exactly 1" >&2
    return 1
  fi
  printf '%s\n' "$value"
}

# Sourced (the e2e parity test sources this file for parse_sandbox_image): stop here.
if [[ "${BASH_SOURCE[0]}" != "$0" ]]; then
  return 0
fi

if [ "$#" -ne 2 ]; then
  echo "usage: $0 <image> <kubernetes-version>" >&2
  exit 2
fi
IMG="$1"
K8S="$2"
for arg in "$IMG" "$K8S"; do
  case "$arg" in
    "" | -*) echo "usage: $0 <image> <kubernetes-version> (got an empty or option-like argument '${arg}')" >&2; exit 2 ;;
  esac
done

work="$(mktemp -d)"
cid=""
cleanup() {
  if [ -n "$cid" ]; then docker rm -f "$cid" >/dev/null 2>&1 || true; fi
  rm -rf "$work"
}
trap cleanup EXIT
fail() { echo "$*" >&2; exit 1; }

# in_image <binary> <args...>: run one of the image's own binaries (GNU coreutils on
# Hadron) against the image's filesystem. Never pulls, no network.
in_image() {
  local bin="$1"
  shift
  docker run --rm --pull never --network none --entrypoint "$bin" "$IMG" "$@"
}

# stat_lines <path>...: "type|uid|gid|octal mode|size|path" per path, as the image's
# stat reports it. stat -c without -L describes each path itself, so a symlink is
# reported as "symbolic link", not as its target.
stat_lines() {
  in_image /usr/bin/stat -c '%F|%u|%g|%a|%s|%n' -- "$@"
}

# run_tar <stdout file> <GNU tar args...>: ANY stderr output is FATAL, like the
# bundler's busybox checks: a warning means tar read the archive differently from
# what its exit status suggests, and the names and bytes checked below must be the
# ones stored.
run_tar() {
  local out="$1"
  shift
  if ! tar "$@" > "$out" 2> "$work/tar.stderr"; then
    cat "$work/tar.stderr" >&2
    fail "tar $* failed"
  fi
  if [ -s "$work/tar.stderr" ]; then
    cat "$work/tar.stderr" >&2
    fail "tar $* reported the warning(s) above"
  fi
}

# (0) Nothing at the legacy path. test exits 0 if it exists (as anything, including a
# dangling symlink) and 1 if not; any other status is a docker or image error.
set +e
in_image /usr/bin/test -e "$LEGACY_BUNDLE_PATH" -o -L "$LEGACY_BUNDLE_PATH"
legacy_rc=$?
set -e
case "$legacy_rc" in
  1) echo "ok: ${LEGACY_BUNDLE_PATH} does not exist in the image" ;;
  0) fail "${LEGACY_BUNDLE_PATH} exists in the image; the bundle must ship only in ${BUNDLE_DIR}" ;;
  *) fail "could not test for ${LEGACY_BUNDLE_PATH} in the image (exit ${legacy_rc})" ;;
esac

# (0) The walk: every directory root-owned with no mode bits beyond 0755 (no group or
# other write, no setuid/setgid/sticky); the bundle's own directories also group 0.
# The provider binary (the importer's filesystem anchor) likewise, as a regular file.
stat_lines "${WALK_DIRS[@]}" "$PROVIDER_BINARY" > "$work/walk"
[ "$(wc -l < "$work/walk")" -eq $((${#WALK_DIRS[@]} + 1)) ] || { cat "$work/walk" >&2; fail "stat did not report every walked path"; }
while IFS='|' read -r ftype uid gid mode _size path; do
  want_type=directory
  [ "$path" != "$PROVIDER_BINARY" ] || want_type="regular file"
  [ "$ftype" = "$want_type" ] || fail "${path} is a ${ftype}, want a ${want_type} (not following symlinks)"
  [[ "$mode" =~ ^[0-7]{1,4}$ ]] || fail "${path}: unexpected mode '${mode}'"
  [ "$uid" = 0 ] || fail "${path} is owned by uid ${uid}, want 0"
  if (((8#$mode) & ~8#755)); then
    fail "${path} has mode ${mode}: bits beyond 0755 (group/other write, setuid/setgid or sticky)"
  fi
  case "$path" in
    /system/provider-kubernetes | "$BUNDLE_DIR" | "$PROVIDER_BINARY")
      [ "$gid" = 0 ] || fail "${path} has group ${gid}, want 0"
      ;;
  esac
  echo "ok: ${path} ${ftype} ${uid}:${gid} ${mode}"
done < "$work/walk"

# docker create only materializes the image filesystem for docker cp.
cid="$(docker create --pull never "$IMG" /usr/bin/kubeadm)"
docker cp "${cid}:${BUNDLE_DIR}" "$work/images"
docker cp "${cid}:/etc/containerd/config.toml" "$work/config.toml"
lock="$work/images/images.lock"

# (1) + (3, part 1): regular files only, before jq or tar read any of them. docker cp
# copies a symlink as a symlink, and find does not follow symlinks, so a symlinked
# directory, lockfile, tarball or config is reported here.
if [ ! -d "$work/images" ] || [ -L "$work/images" ]; then
  fail "the bundled image directory is not a plain directory"
fi
find "$work/images" -mindepth 1 \( ! -type f -o ! \( -name images.lock -o -name '*.tar' \) \) \
  > "$work/unexpected"
if [ -s "$work/unexpected" ]; then cat "$work/unexpected" >&2; fail "unexpected entries above in the bundled image directory"; fi
if [ ! -f "$lock" ] || [ -L "$lock" ]; then
  fail "images.lock is missing or not a regular file"
fi
if [ ! -f "$work/config.toml" ] || [ -L "$work/config.toml" ]; then
  fail "containerd config.toml is not a regular file"
fi

# (7) directory entries, lock size and entry count, before the lock is parsed further.
dir_entries="$(find "$work/images" -mindepth 1 -maxdepth 1 | wc -l)"
[ "$dir_entries" -le "$MAX_DIR_ENTRIES" ] || fail "the bundle directory has ${dir_entries} entries, half-bound ${MAX_DIR_ENTRIES}"
lock_bytes="$(stat -c %s "$lock")"
[ "$lock_bytes" -le "$MAX_LOCK_BYTES" ] || fail "images.lock is ${lock_bytes} bytes, half-bound ${MAX_LOCK_BYTES}"

# (2)
in_image /usr/bin/kubeadm config images list \
  --kubernetes-version "$K8S" --image-repository registry.k8s.io > "$work/kubeadm.refs"
[ -s "$work/kubeadm.refs" ] || fail "bundled kubeadm ${K8S} listed no images"
jq -r '.images[].ref' "$lock" > "$work/lock.refs"
entries="$(wc -l < "$work/lock.refs")"
if [ "$entries" -lt "$MIN_ENTRIES" ] || [ "$entries" -gt "$MAX_ENTRIES" ]; then
  fail "images.lock lists ${entries} images, want ${MIN_ENTRIES}..${MAX_ENTRIES} (half-bound)"
fi
[ -z "$(sort "$work/lock.refs" | uniq -d)" ] || fail "images.lock has duplicate refs"
diff <(sort "$work/kubeadm.refs") <(sort "$work/lock.refs") \
  || fail "images.lock refs != bundled kubeadm ${K8S} image list (< kubeadm, > images.lock)"

# (3, part 2)
(cd "$work/images" && find . -mindepth 1 -maxdepth 1 -name '*.tar' | sed 's#^\./##' | sort) > "$work/tars.fs"
jq -r '.images[].tarball' "$lock" | sort > "$work/tars.lock"
[ -z "$(uniq -d "$work/tars.lock")" ] || fail "images.lock has duplicate tarball names"
diff "$work/tars.fs" "$work/tars.lock" || fail "*.tar files != images.lock tarballs (< files, > images.lock)"
while IFS= read -r tarball; do
  [[ "$tarball" =~ ^[A-Za-z0-9._-]+\.tar$ ]] || fail "unexpected tarball name '${tarball}'"
done < "$work/tars.lock"

# (1, in the image) + (7) owner, mode and size of images.lock and every tarball as
# the image has them (docker cp does not keep ownership), and each size equal to the
# copy checked below.
mapfile -t bundle_files < <(sed "s#^#${BUNDLE_DIR}/#" "$work/tars.lock")
stat_lines "${BUNDLE_DIR}/images.lock" "${bundle_files[@]}" > "$work/files"
[ "$(wc -l < "$work/files")" -eq $((entries + 1)) ] || { cat "$work/files" >&2; fail "stat did not report images.lock and every tarball"; }
total=0
while IFS='|' read -r ftype uid gid mode size path; do
  [ "$ftype|$uid|$gid|$mode" = "regular file|0|0|644" ] \
    || fail "${path} is '${ftype}' ${uid}:${gid} mode ${mode}, want a root:root 0644 regular file"
  copy="$work/images/${path##*/}"
  [ "$(stat -c %s "$copy")" = "$size" ] || fail "${path} is ${size} bytes in the image but the copy is not"
  if [ "$path" != "${BUNDLE_DIR}/images.lock" ]; then
    if [ "$size" -lt "$MIN_TARBALL_BYTES" ] || [ "$size" -gt "$MAX_TARBALL_BYTES" ]; then
      fail "${path} is ${size} bytes, want ${MIN_TARBALL_BYTES}..${MAX_TARBALL_BYTES} (half-bound)"
    fi
    total=$((total + size))
  fi
done < "$work/files"
[ "$total" -le "$MAX_TOTAL_BYTES" ] || fail "the tarballs total ${total} bytes, half-bound ${MAX_TOTAL_BYTES}"
echo "ok: images.lock and ${entries} tarballs are root:root 0644 regular files; lock ${lock_bytes} bytes, tarballs ${total} bytes, ${dir_entries} directory entries"

# (4)
sandbox="$(parse_sandbox_image "$work/config.toml")" || fail "could not take sandbox_image from containerd config.toml (reason above)"
grep -qxF -- "$sandbox" "$work/lock.refs" || fail "sandbox_image ${sandbox} is not a bundled image ref"

# The architecture the bundle must be built for. Taken from the IMAGE itself, not
# from the runner, so an amd64 bundle inside an arm64 image is caught: images.lock
# is ARCH-SPECIFIC under ADR-16-A3 (its manifestDigest is the per-platform child).
img_arch="$(docker image inspect --format '{{.Architecture}}' "$IMG")"
[[ "$img_arch" =~ ^(amd64|arm64)$ ]] || fail "the image reports architecture '${img_arch}', want amd64 or arm64"

# clean_names <names file>: the member names as containerd keys them,
# path.Clean-style (leading "./" stripped, "//" collapsed, trailing "/" dropped).
# ".." is refused outright below, which is the one element this cannot resolve.
clean_names() { sed -e 's#^\(\./\)*##' -e 's#//*#/#g' -e 's#/$##' "$1"; }

# render_index <mediaType> <size> <digest> <arch> <ref>: the ONE canonical
# index.json rendering -- compact, NO trailing newline. Byte-identical to
# build/bundle-images.sh's render_index and internal/imageimport's renderIndex.
render_index() {
  printf '{"schemaVersion":2,"mediaType":"%s","manifests":[{"mediaType":"%s","size":%s,"digest":"%s","platform":{"architecture":"%s","os":"linux"},"annotations":{"%s":"%s"}}]}' \
    "$MEDIA_TYPE_OCI_INDEX" "$1" "$2" "$3" "$4" "$ANNOTATION_REF_NAME" "$5"
}

# json_slice <flat json> <key> <open> <close>: the body of the (single) object or
# array named by <key>, up to the first <close>, so it holds no nested object or
# array of that kind. Pure parameter expansion: no regex escaping of braces.
json_slice() {
  local s="$1" rest
  rest="${s#*\""$2"\":"$3"}"
  [ "$rest" != "$s" ] || return 1
  printf '%s' "${rest%%"$4"*}"
}

# count_matches <string> <ERE>: how many times the pattern occurs in the string.
count_matches() { grep -Eo -e "$2" <<<"$1" | grep -c . || true; }

# one_field <string> <key ERE>: the single occurrence of "<key>":<value>, or "".
# Returns non-zero unless it occurs exactly once.
one_field() {
  local got
  got="$(grep -Eo -e "$2" <<<"$1" || true)"
  [ "$(grep -c . <<<"$got")" -eq 1 ] || return 1
  printf '%s' "$got"
}

# (5) + (6) + (7): the canonical OCI image layout, member by member, byte for
# byte, from the archive's own bytes -- plus the pin to images.lock's
# manifestDigest, the one runtime link between the bundle and what cosign
# verified at build time.
n=0
while IFS=$'\t' read -r ref manifest_digest tarball; do
  t="$work/images/${tarball}"
  [[ "$manifest_digest" =~ ^sha256:[0-9a-f]{64}$ ]] \
    || fail "${tarball}: images.lock manifestDigest '${manifest_digest}' is not sha256:<64 lowercase hex>"
  mhex="${manifest_digest#sha256:}"
  run_tar "$work/names" -tf "$t"
  # --numeric-owner: assert uid/gid 0 as NUMBERS, whatever uname/gname the
  # headers carry. GNU tar -tv: "<mode> <owner/group> <size> <date> <time> <name>".
  run_tar "$work/listing" -tvf "$t" --numeric-owner
  members="$(wc -l < "$work/names")"
  [ "$members" -eq "$(wc -l < "$work/listing")" ] || fail "${tarball}: member listings disagree"
  if [ "$members" -lt "$MIN_MEMBERS" ] || [ "$members" -gt "$MAX_MEMBERS" ]; then
    fail "${tarball}: ${members} members, want ${MIN_MEMBERS}..${MAX_MEMBERS} (half-bound)"
  fi
  [ "$(tr -d '\000' < "$work/names" | wc -c)" -eq "$(wc -c < "$work/names")" ] \
    || fail "${tarball}: a member name contains a NUL byte"
  # Format confusion: containerd takes the OCI branch and returns EARLY when
  # oci-layout is present, so an archive carrying BOTH oci-layout and
  # manifest.json would validate as docker-save and import as OCI.
  if clean_names "$work/names" | grep -qx 'manifest\.json'; then
    fail "${tarball}: a manifest.json member is present; an OCI layout must never carry one"
  fi
  if grep -E '^/|^\./|//|/$|\\|(^|/)\.\.?($|/)' "$work/names" >&2; then
    fail "${tarball}: the member name(s) above are absolute, aliasing, or a directory (trailing '/')"
  fi
  if grep -Evx 'oci-layout|index\.json|blobs/sha256/[0-9a-f]{64}' "$work/names" >&2; then
    fail "${tarball}: unexpected member name(s) above"
  fi
  [ -z "$(sort "$work/names" | uniq -d)" ] || fail "${tarball}: duplicate member names"
  # ... and no two names that collide after path.Clean: "index.json" and
  # "./index.json" are ONE key to containerd, where a later member silently
  # OVERWRITES an earlier one, and two distinct strings to a naive seen-set.
  [ -z "$(clean_names "$work/names" | sort | uniq -d)" ] \
    || fail "${tarball}: member names that collide after path.Clean"
  if awk 'NF != 6 || substr($1, 1, 1) != "-"' "$work/listing" | grep . >&2; then
    fail "${tarball}: non-regular or unexpected member listing(s) above"
  fi
  awk '{ print $6 }' "$work/listing" | cmp -s - "$work/names" || fail "${tarball}: tar -tv names differ from tar -t"
  # The canonical profile: mode 0644, uid/gid 0, mtime epoch 0 (GNU tar -tv
  # prints the minute, not the second).
  if awk '$1 != "-rw-r--r--" || $2 != "0/0" || $4 != "1970-01-01" || $5 != "00:00"' "$work/listing" | grep . >&2; then
    fail "${tarball}: the member(s) above are not uid/gid 0, mode 0644, mtime epoch 0"
  fi

  # Fixed order, and the manifest member is the one images.lock pins.
  [ "$(sed -n 1p "$work/names")" = oci-layout ] || fail "${tarball}: member 1 is not oci-layout"
  [ "$(sed -n 2p "$work/names")" = index.json ] || fail "${tarball}: member 2 is not index.json"
  [ "$(sed -n 3p "$work/names")" = "blobs/sha256/${mhex}" ] \
    || fail "${tarball}: member 3 is $(sed -n 3p "$work/names"), want the manifest blobs/sha256/${mhex} images.lock pins"

  # oci-layout: exact bytes, never parsed.
  run_tar "$work/oci-layout" -xOf "$t" oci-layout
  printf '%s' "$OCI_LAYOUT_BYTES" | cmp -s - "$work/oci-layout" \
    || fail "${tarball}: oci-layout is not the exact $(printf '%s' "$OCI_LAYOUT_BYTES" | wc -c) canonical bytes"

  # index.json: bounded, then held to the canonical rendering byte for byte,
  # rebuilt from the ARCHIVE's own manifest member and the LOCK's digest and ref.
  run_tar "$work/index.json" -xOf "$t" index.json
  idx_bytes="$(stat -c %s "$work/index.json")"
  [ "$idx_bytes" -le "$MAX_INDEX_BYTES" ] \
    || fail "${tarball}: index.json is ${idx_bytes} bytes, half-bound ${MAX_INDEX_BYTES}"
  idx="$(cat "$work/index.json")"
  case "$idx" in
    *"$ANNOTATION_CONTAINERD_NAME"*)
      fail "${tarball}: index.json carries ${ANNOTATION_CONTAINERD_NAME}, which would let the archive choose the imported image's NAME" ;;
  esac
  # (6) POSITIVE: the ref images.lock lists is named, exactly once, in the one
  # annotation containerd reads -- not merely "no placeholder is present".
  [ "$(count_matches "$idx" "\"${ANNOTATION_REF_NAME}\":")" -eq 1 ] \
    || fail "${tarball}: index.json must carry exactly one ${ANNOTATION_REF_NAME} annotation"
  [ "$(count_matches "$idx" '"annotations":[[:space:]]*[{]')" -eq 1 ] \
    || fail "${tarball}: index.json must carry exactly one annotations object"
  [[ "$idx" == *"\"annotations\":{\"${ANNOTATION_REF_NAME}\":\"${ref}\"}}]}" ]] \
    || fail "${tarball}: index.json does not end with exactly {\"${ANNOTATION_REF_NAME}\":\"${ref}\"}"
  # The descriptor's mediaType: read from the descriptor body, since index.json
  # carries the index's own mediaType too.
  idx_body="${idx#*'"manifests":[{'}"
  idx_mt="$(one_field "$idx_body" '"mediaType":"[^"]+"' | sed -e 's#^"mediaType":"##' -e 's#"$##')" \
    || fail "${tarball}: index.json must hold exactly one manifest descriptor"
  case "$idx_mt" in
    "$MEDIA_TYPE_OCI_MANIFEST" | "$MEDIA_TYPE_DOCKER_MANIFEST") : ;;
    "$MEDIA_TYPE_OCI_INDEX" | "$MEDIA_TYPE_DOCKER_INDEX")
      fail "${tarball}: index.json names an INDEX (${idx_mt}); the archive must name one platform manifest" ;;
    *) fail "${tarball}: index.json descriptor mediaType '${idx_mt}' is not allowed" ;;
  esac
  man_bytes="$(awk -v m="blobs/sha256/${mhex}" '$6 == m { print $3 }' "$work/listing")"
  [[ "$man_bytes" =~ ^[0-9]+$ ]] && [ "$man_bytes" -le "$MAX_MANIFEST_BYTES" ] \
    || fail "${tarball}: manifest member is '${man_bytes}' bytes, half-bound ${MAX_MANIFEST_BYTES}"
  render_index "$idx_mt" "$man_bytes" "$manifest_digest" "$img_arch" "$ref" > "$work/index.want"
  if ! cmp -s "$work/index.want" "$work/index.json"; then
    printf 'want: ' >&2; cat "$work/index.want" >&2; echo >&2
    printf 'got:  ' >&2; cat "$work/index.json" >&2; echo >&2
    fail "${tarball}: index.json is not the canonical compact rendering for ${ref} on linux/${img_arch}"
  fi

  # The manifest blob: bound to its name by sha256, then parsed for the closure.
  run_tar "$work/manifest" -xOf "$t" "blobs/sha256/${mhex}"
  [ "$(sha256sum < "$work/manifest" | cut -d' ' -f1)" = "$mhex" ] \
    || fail "${tarball}: the manifest member's sha256 is not ${mhex}"
  man_flat="$(tr -d ' \t\n\r' < "$work/manifest")"
  [ "$(count_matches "$man_flat" '"config":[{]')" -eq 1 ] || fail "${tarball}: manifest needs exactly one config object"
  [ "$(count_matches "$man_flat" '"layers":[[]')" -eq 1 ] || fail "${tarball}: manifest needs exactly one layers array"
  man_top="$(sed -e 's#"config":{[^}]*}##' -e 's#"layers":\[[^]]*\]##' <<<"$man_flat")"
  [[ "$man_top" == *'"schemaVersion":2'* ]] || fail "${tarball}: manifest schemaVersion is not 2"
  man_mt="$(grep -Eo '"mediaType":"[^"]+"' <<<"$man_top" || true)"
  if [ -n "$man_mt" ]; then
    [ "$man_mt" = "\"mediaType\":\"${idx_mt}\"" ] \
      || fail "${tarball}: manifest mediaType ${man_mt} does not match index.json's '${idx_mt}'"
  elif [ "$idx_mt" != "$MEDIA_TYPE_OCI_MANIFEST" ]; then
    fail "${tarball}: manifest omits mediaType but index.json declares '${idx_mt}'"
  fi
  cfg_obj="$(json_slice "$man_flat" config '{' '}')"
  cfg_digest="$(one_field "$cfg_obj" '"digest":"sha256:[0-9a-f]{64}"' | sed -e 's#^"digest":"##' -e 's#"$##')" \
    || fail "${tarball}: the manifest config needs exactly one sha256 digest"
  cfg_bytes="$(one_field "$cfg_obj" '"size":[0-9]+' | sed 's#^"size":##')" \
    || fail "${tarball}: the manifest config needs exactly one numeric size"
  [ "$cfg_bytes" -ge 1 ] && [ "$cfg_bytes" -le "$MAX_CONFIG_BYTES" ] \
    || fail "${tarball}: config is ${cfg_bytes} bytes, half-bound ${MAX_CONFIG_BYTES}"
  cfg_hex="${cfg_digest#sha256:}"
  [ "$cfg_hex" != "$mhex" ] || fail "${tarball}: the manifest config is the manifest blob itself"

  # The canonical member list the manifest implies: config first, then each
  # DISTINCT layer in first-seen order (a repeated layer is stored once, at its
  # first position). Compared against the archive's listing, which binds the
  # closure (exactly these blobs), the ORDER and every member's size in one go.
  {
    printf 'oci-layout %s\n' "$(printf '%s' "$OCI_LAYOUT_BYTES" | wc -c)"
    printf 'index.json %s\n' "$(stat -c %s "$work/index.want")"
    printf 'blobs/sha256/%s %s\n' "$mhex" "$man_bytes"
    printf 'blobs/sha256/%s %s\n' "$cfg_hex" "$cfg_bytes"
  } > "$work/pairs.want"
  layers=0
  # json_slice prints no trailing newline, so the last element arrives as an
  # incomplete line: read returns non-zero for it while still setting elem.
  while IFS= read -r elem || [ -n "$elem" ]; do
    [ -n "$elem" ] || continue
    l_hex="$(one_field "$elem" '"digest":"sha256:[0-9a-f]{64}"' | sed -e 's#^"digest":"sha256:##' -e 's#"$##')" \
      || fail "${tarball}: layer $((layers + 1)) needs exactly one sha256 digest"
    l_bytes="$(one_field "$elem" '"size":[0-9]+' | sed 's#^"size":##')" \
      || fail "${tarball}: layer $((layers + 1)) needs exactly one numeric size"
    layers=$((layers + 1))
    [ "$l_bytes" -ge 1 ] || fail "${tarball}: layer ${layers} size ${l_bytes} is not positive"
    if [ "$l_hex" = "$mhex" ] || [ "$l_hex" = "$cfg_hex" ]; then
      fail "${tarball}: layer ${layers} reuses the manifest or config digest"
    fi
    if grep -q "^blobs/sha256/${l_hex} " "$work/pairs.want"; then
      grep -qx "blobs/sha256/${l_hex} ${l_bytes}" "$work/pairs.want" \
        || fail "${tarball}: layer ${layers} repeats a digest with a different size"
      continue
    fi
    printf 'blobs/sha256/%s %s\n' "$l_hex" "$l_bytes" >> "$work/pairs.want"
  done < <(json_slice "$man_flat" layers '[' ']' | sed 's#},{#\n#g')
  [ "$layers" -ge 1 ] || fail "${tarball}: the manifest names no layers"
  if ! awk '{ print $6, $3 }' "$work/listing" | cmp -s - "$work/pairs.want"; then
    awk '{ print $6, $3 }' "$work/listing" > "$work/pairs.got"
    diff "$work/pairs.got" "$work/pairs.want" >&2 || true
    fail "${tarball}: members are not exactly oci-layout, index.json, the manifest, its config and its layers, in that order and with those sizes (< archive, > wanted)"
  fi

  # The config blob: bound to its name, and declaring the image's own platform.
  run_tar "$work/config" -xOf "$t" "blobs/sha256/${cfg_hex}"
  [ "$(sha256sum < "$work/config" | cut -d' ' -f1)" = "$cfg_hex" ] \
    || fail "${tarball}: the config member's sha256 is not ${cfg_hex}"
  if [ "$(grep -Eo '"architecture":[[:space:]]*"[^"]*"' "$work/config" | grep -c .)" -ne 1 ] \
    || [ "$(grep -Eo "\"architecture\":[[:space:]]*\"${img_arch}\"" "$work/config" | grep -c .)" -ne 1 ] \
    || [ "$(grep -Eo '"os":[[:space:]]*"[^"]*"' "$work/config" | grep -c .)" -ne 1 ] \
    || [ "$(grep -Eo '"os":[[:space:]]*"linux"' "$work/config" | grep -c .)" -ne 1 ]; then
    grep -Eo '"(architecture|os)":[[:space:]]*"[^"]*"' "$work/config" >&2 || true
    fail "${tarball}: the image config does not declare exactly linux/${img_arch}"
  fi

  echo "ok: ${tarball} ${ANNOTATION_REF_NAME}=\"${ref}\" manifest=${manifest_digest} (${man_bytes}B) config=${cfg_digest} (${cfg_bytes}B) members=${members} layers=${layers} platform=linux/${img_arch}"
  n=$((n + 1))
done < <(jq -r '.images[] | [.ref, .manifestDigest, .tarball] | @tsv' "$lock")
[ "$n" -eq "$entries" ] || fail "checked ${n} tarballs, images.lock lists ${entries}"

# (6) No docker-save artefact anywhere in the bundle, so a docker-save tarball
# cannot sneak back in under a passing OCI check: no manifest.json member (each
# archive's members were allowlisted above, this re-checks the whole directory),
# no RepoTags bytes, and no "i-was-a-digest" placeholder (the pre-A3 rename's
# marker, which an OCI layout has no reason to contain).
for forbidden in RepoTags i-was-a-digest; do
  if grep -rlF "$forbidden" "$work/images" >&2; then
    fail "the docker-save marker '${forbidden}' is present in the file(s) above"
  fi
done
echo "ok: no RepoTags or i-was-a-digest bytes anywhere in the bundle"

# (8) The image's own importer, on the image's own filesystem, runs every boot check
# (walk, owner/mode, filesystem, lock bytes, tarball structure, config digest)
# without importing. It must need nothing but the image, hence --network none. Its
# last output line is the summary; logrus writes to stderr, so both streams are read.
if verify_out="$(in_image "$PROVIDER_BINARY" import-images --verify-only 2>&1)"; then
  verify_rc=0
else
  verify_rc=$?
fi
printf '%s\n' "$verify_out"
[ "$verify_rc" -eq 0 ] || fail "import-images --verify-only exited ${verify_rc}, want 0"
[ "$(grep -c 'image-import: summary ' <<<"$verify_out")" -eq 1 ] \
  || fail "import-images --verify-only printed $(grep -c 'image-import: summary ' <<<"$verify_out") summary lines, want 1"
last="$(sed '/^[[:space:]]*$/d' <<<"$verify_out" | tail -n 1)"
summary_re='image-import: summary outcome=([a-z-]+) entries=([0-9]+) imported=([0-9]+) refused=([0-9]+) failed=([0-9]+) unlisted=([0-9]+) readonly=(true|false) dir=([^[:space:]"]+)'
[[ "$last" =~ $summary_re ]] || fail "the last line of import-images --verify-only is not its summary: ${last}"
got="outcome=${BASH_REMATCH[1]} entries=${BASH_REMATCH[2]} imported=${BASH_REMATCH[3]} refused=${BASH_REMATCH[4]} failed=${BASH_REMATCH[5]} unlisted=${BASH_REMATCH[6]} dir=${BASH_REMATCH[8]}"
want="outcome=verified entries=${entries} imported=0 refused=0 failed=0 unlisted=0 dir=${BUNDLE_DIR}"
[ "$got" = "$want" ] || fail "import-images --verify-only summary: got ${got}, want ${want}"
echo "ok: import-images --verify-only: ${got}"

echo "OK: ${n} bundled tarballs in ${BUNDLE_DIR} are root-owned, within half of every importer bound, named with their exact refs and verified by the image's importer; sandbox_image ${sandbox} is bundled"
