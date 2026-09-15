#!/usr/bin/env bash
# verify-bundle-refs.sh <image> <kubernetes-version>
#
# Verify the pre-bundled control-plane images of a built provider-kubernetes image:
# where they live and with what owner and mode (ADR-16-A2), that every tarball is
# named with its EXACT ref (F-IMPORTREF, ADR-16-A1), and that the image's own boot
# importer accepts the bundle. CI runs it on every supported minor and the release
# workflow runs it before `docker push`, so both gates run the same checks.
#
# `ctr images import` names each bundled image from its docker-save RepoTags, and
# kubeadm (CRI ImageStatus) and containerd's sandbox_image look images up by the
# EXACT ref, so an air-gap first boot needs every tarball named exactly as kubeadm
# expects. At boot, `import-images` imports only the images.lock entries and
# refuses a bundle or tarball whose path, owner, mode, file type, filesystem or
# structure is wrong. Asserted against the SHIPPED image:
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
#   (5) each tarball has unique, regular members (one sha256:<hex> config,
#       <hex>.tar.gz layers, manifest.json last) and its manifest.json bytes are
#       exactly [{"Config":"<config>","RepoTags":["<lock ref>"],"Layers":[...]}];
#   (6) the "i-was-a-digest" placeholder appears nowhere;
#   (7) the bundle stays within HALF of every boot importer bound;
#   (8) the image's own `import-images --verify-only` exits 0, and its last output
#       line is the summary with outcome=verified and entries == images.lock's.
# Exact bytes, never a JSON projection: containerd's importer matches keys
# case-insensitively and does not bind member bytes to member names. jq only reads
# images.lock, which the build itself generates. (8) adds what only the Go importer
# checks: PAX/sparse headers, the config member's sha256, the lock's canonical
# bytes and the filesystem of every file.
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
readonly MAX_MANIFEST_BYTES=$((32 * 1024))           # importer: 64 KiB
readonly MAX_CONFIG_BYTES=$((512 * 1024))            # importer: 1 MiB

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

# (5) + (7) members, manifest.json and config sizes.
layers_re='^"[0-9a-f]{64}\.tar\.gz"(,"[0-9a-f]{64}\.tar\.gz")*$'
n=0
while IFS=$'\t' read -r ref tarball; do
  t="$work/images/${tarball}"
  run_tar "$work/names" -tf "$t"
  run_tar "$work/listing" -tvf "$t"
  members="$(wc -l < "$work/names")"
  [ "$members" -eq "$(wc -l < "$work/listing")" ] || fail "${tarball}: member listings disagree"
  if [ "$members" -lt "$MIN_MEMBERS" ] || [ "$members" -gt "$MAX_MEMBERS" ]; then
    fail "${tarball}: ${members} members, want ${MIN_MEMBERS}..${MAX_MEMBERS} (half-bound)"
  fi
  if grep -Evx 'manifest\.json|sha256:[0-9a-f]{64}|[0-9a-f]{64}\.tar\.gz' "$work/names" >&2; then
    fail "${tarball}: unexpected member name(s) above"
  fi
  [ -z "$(sort "$work/names" | uniq -d)" ] || fail "${tarball}: duplicate member names"
  # GNU tar -tv: "<mode> <owner/group> <size> <date> <time> <name>"; names are
  # validated above, so a regular member is exactly six fields.
  if awk 'NF != 6 || substr($1, 1, 1) != "-"' "$work/listing" | grep . >&2; then
    fail "${tarball}: non-regular or unexpected member listing(s) above"
  fi
  awk '{ print $6 }' "$work/listing" | cmp -s - "$work/names" || fail "${tarball}: tar -tv names differ from tar -t"
  if [ "$(grep -cx 'manifest\.json' "$work/names")" -ne 1 ] || [ "$(tail -n 1 "$work/names")" != manifest.json ]; then
    fail "${tarball}: manifest.json must be present exactly once, as the last member"
  fi
  [ "$(grep -cEx 'sha256:[0-9a-f]{64}' "$work/names")" -eq 1 ] || fail "${tarball}: want exactly one sha256:<hex> config member"
  cfg="$(grep -Ex 'sha256:[0-9a-f]{64}' "$work/names")"
  cfg_bytes="$(awk -v n="$cfg" '$6 == n { print $3 }' "$work/listing")"
  if ! [[ "$cfg_bytes" =~ ^[0-9]+$ ]] || [ "$cfg_bytes" -gt "$MAX_CONFIG_BYTES" ]; then
    fail "${tarball}: config member ${cfg} is '${cfg_bytes}' bytes, half-bound ${MAX_CONFIG_BYTES}"
  fi
  run_tar "$work/manifest.json" -xOf "$t" manifest.json
  size="$(stat -c %s "$work/manifest.json")"
  [ "$size" -le "$MAX_MANIFEST_BYTES" ] || fail "${tarball}: manifest.json is ${size} bytes, half-bound ${MAX_MANIFEST_BYTES}"
  pre="[{\"Config\":\"${cfg}\",\"RepoTags\":[\"${ref}\"],\"Layers\":["
  suf=']}]'
  m="$(cat "$work/manifest.json")"
  if [[ "$m" != "$pre"*"$suf" ]]; then
    printf '%s\n' "$m" >&2
    fail "${tarball}: manifest.json (above) is not exactly ${pre}...${suf}"
  fi
  mid="${m#"$pre"}"
  mid="${mid%"$suf"}"
  [[ "$mid" =~ $layers_re ]] || fail "${tarball}: manifest.json Layers is not a plain list of <hex>.tar.gz names"
  want="$(printf '%s' "${pre}${mid}${suf}" | wc -c)"
  [ "$size" -eq "$want" ] \
    || fail "${tarball}: manifest.json has ${size} bytes, want ${want} (trailing or hidden bytes)"
  [ "$(grep -o '"RepoTags":\[' "$work/manifest.json" | wc -l)" -eq 1 ] \
    || fail "${tarball}: \"RepoTags\":[ must occur exactly once"
  diff <(tr ',' '\n' <<<"$mid" | tr -d '"' | sort -u) \
    <(grep -Ex '[0-9a-f]{64}\.tar\.gz' "$work/names" | sort -u) >/dev/null \
    || fail "${tarball}: manifest.json Layers != layer members"
  echo "ok: ${tarball} RepoTags=[\"${ref}\"] members=${members} layers=$(grep -cEx '[0-9a-f]{64}\.tar\.gz' "$work/names") config=${cfg_bytes}B manifest=${size}B"
  n=$((n + 1))
done < <(jq -r '.images[] | [.ref, .tarball] | @tsv' "$lock")
[ "$n" -eq "$entries" ] || fail "checked ${n} tarballs, images.lock lists ${entries}"

# (6)
if grep -rlF i-was-a-digest "$work/images" >&2; then
  fail "the i-was-a-digest placeholder is still present in the file(s) above"
fi

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
