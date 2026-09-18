#!/usr/bin/env bash
# verify-image-files.sh <image> [--units-only]
#
# Verify the owner and mode of the files the Dockerfile installs into a built
# provider-kubernetes image. tar run as root keeps the owner stored in an archive
# (the cri-tools release tarball stores crictl as uid 1001), and COPY --from keeps
# the owner a build stage left, so a foreign owner can reach the image without any
# build step failing. Asserted against the SHIPPED image:
#   (1) the directories the files below live in are root:root 0755 directories
#       (not symlinks), so the tree walk in (5) covers what it names;
#   (2) every binary the Dockerfile installs is a root:root regular file with mode
#       exactly 0755 (no group/other write, no setuid/setgid/sticky);
#   (3) every configuration file it installs is a root:root regular file with mode
#       exactly 0644;
#   (4) every entry in /opt/cni/bin is a root:root regular file with an exact mode:
#       0755 for the CNI plugins, and 0644 for the two documentation files the
#       plugins release tarball also ships (LICENSE, README.md). Naming both modes
#       exactly, rather than only excluding group/other write and setuid/setgid,
#       means a plugin that arrived non-executable -- or any new entry a future
#       tarball adds -- fails here instead of passing a mode-range test;
#   (5) nothing under /usr/bin or /system other than a symlink is owned by a user
#       other than root or writable by group or others. This also covers a file
#       added to those directories without updating the lists below. The base
#       image's own setuid tools and group-owned files (for example wall) pass (5).
# It also asserts the ADR-19 U1 (F-UNITPATH) exec-path settings, which only take
# effect if they actually reach the image with the exact content we reviewed:
#   (6) each systemd drop-in under /usr/lib/systemd/system/<unit>.d/ is byte-identical
#       to its source in the build context (both hashed by the IMAGE's own
#       sha256sum, so the runner needs no extra tool);
#   (7) containerd's config.toml carries each exec-path line exactly once: the `opt`
#       internal plugin and both erofs plugins disabled (`opt` would otherwise prepend
#       the persistent /opt/containerd/bin to the daemon's own PATH; the erofs plugins
#       look up mkfs.erofs by name), the image-verifier bin_dir and the NRI
#       plugin_path moved out of /opt, and the absolute shim and runc paths;
#   (8) the two image-only directories containerd executes from ship empty;
#   (9) every name containerd, its shim, its CNI plugins or the kubelet resolve BY
#       NAME either already exists inside the image on the daemons' PATH as a
#       root-owned regular file with an exact mode (0755, or 4755 for the base
#       image's setuid mount/umount), or is closed by a setting asserted here. This
#       is the general rule behind (7) and the drop-in contents: containerd's
#       deprecated NRI v0.1 client APPENDS the persistent /opt/nri/bin to its own
#       PATH and no key disables it (R-19-13), and an appended element can only ever
#       supply a name the image does NOT provide. A future name that is neither
#       present nor closed therefore fails this gate instead of silently becoming
#       reachable from a persistent directory.
# The shim and runc themselves are in (2), so they are also proven to be root:root
# 0755 regular files at the absolute paths (7) names.
# Finally it asserts the ADR-19 U2 (F-UNITPATH) unit layout, which is the whole
# content of --units-only:
#  (10) no provider-owned unit name -- a fragment, a `<unit>.d/` drop-in directory
#       or a `.wants` link -- exists under /etc/systemd/system, .control,
#       .attached or /usr/local/lib/systemd/system. All of those outrank
#       /usr/lib/systemd/system in systemd's search path and all of them are
#       persistent on Kairos, so one file left there would silently keep
#       overriding the image's unit across every future upgrade;
#  (11) each unit and the kubeadm drop-in under /usr/lib/systemd/system is a
#       root:root 0644 regular file, byte-identical to the build context, with no
#       [Install] section (an [Install] section makes `systemctl is-enabled`
#       report "disabled" instead of "static" and lets `systemctl disable` write a
#       persistent override into /etc);
#  (12) each unit is enabled by a symlink in
#       /usr/lib/systemd/system/multi-user.target.wants named after the unit whose
#       target is exactly `../<unit>` -- relative, so it can never point out of the
#       image -- and the one-time migration unit is among them.
# S19-11 requires (10)-(12) on the e2e node image as well, which FROM-derives the
# release image and used to re-run `systemctl enable ... || true`: pass
# --units-only to check just those on a derived image.
# CI runs it on every supported minor and the release workflow runs it before
# `docker push`, so both gates run the same checks.
#
# Requires: bash, docker.
set -euo pipefail
export LC_ALL=C

# Directories that hold the files below. /usr/bin and /system are also the roots of
# the tree walk in (5); find does not follow a symlinked starting point.
readonly DIRS=(
  /usr/bin
  /system
  /system/providers
  /opt/cni
  /opt/cni/bin
  /etc/containerd
  /usr/lib/systemd/system/containerd.service.d
  /usr/lib/systemd/system/kubelet.service.d
  /usr/lib/containerd
  /usr/lib/containerd/image-verifier
  /usr/lib/containerd/image-verifier/bin
  /usr/lib/nri
  /usr/lib/nri/plugins
)
# Binaries the Dockerfile installs (the containerd build output is containerd,
# containerd-shim-runc-v2 and ctr).
readonly BINARIES=(
  /usr/bin/kubeadm
  /usr/bin/kubelet
  /usr/bin/kubectl
  /usr/bin/crictl
  /usr/bin/etcdctl
  /usr/bin/etcdutl
  /usr/bin/containerd
  /usr/bin/containerd-shim-runc-v2
  /usr/bin/ctr
  /usr/bin/runc
  /system/providers/agent-provider-kubernetes
)
# Configuration files the Dockerfile installs. The unit files and the kubeadm
# drop-in are NOT here: check (11) covers them, so that --units-only can run the
# same assertions on a derived image.
readonly CONFIGS=(
  /etc/containerd/config.toml
  /etc/sysctl.d/k8s.conf
  /etc/modules-load.d/k8s.conf
  /usr/lib/systemd/system/containerd.service.d/50-provider-kubernetes-exec-path.conf
  /usr/lib/systemd/system/kubelet.service.d/50-provider-kubernetes-exec-path.conf
)
readonly CNI_BIN_DIR=/opt/cni/bin
# The non-plugin files the CNI plugins release tarball ships next to the binaries.
# They are documentation, so they are the only /opt/cni/bin entries allowed to be
# non-executable, and they are allowed only at exactly 0644.
readonly CNI_DOC_NAMES=(LICENSE README.md)
readonly TREE_ROOTS=(/usr/bin /system)

# (6) "<path in the image>|<path in the build context>" for every file that must be
# byte-identical to what the checkout holds.
readonly EXACT_COPIES=(
  "/usr/lib/systemd/system/containerd.service.d/50-provider-kubernetes-exec-path.conf|usr-lib-systemd/containerd.service.d/50-provider-kubernetes-exec-path.conf"
  "/usr/lib/systemd/system/kubelet.service.d/50-provider-kubernetes-exec-path.conf|usr-lib-systemd/kubelet.service.d/50-provider-kubernetes-exec-path.conf"
)
# (7) containerd config lines (ADR-19 U1). Each must appear exactly once among the
# file's uncommented lines, after trimming the indentation. The same values are
# asserted against the SOURCE file by TestContainerdConfigClosesPersistentExecPaths
# (image_exec_path_test.go), so an edit that never reaches the image fails there and
# an image that lost the setting fails here.
readonly CONTAINERD_CONFIG=/etc/containerd/config.toml
readonly CONTAINERD_CONFIG_LINES=(
  'disabled_plugins = ["io.containerd.internal.v1.opt", "io.containerd.differ.v1.erofs", "io.containerd.snapshotter.v1.erofs"]'
  'bin_dir = "/usr/lib/containerd/image-verifier/bin"'
  'plugin_path = "/usr/lib/nri/plugins"'
  'runtime_path = "/usr/bin/containerd-shim-runc-v2"'
  'BinaryName = "/usr/bin/runc"'
)
# (9) The directories containerd and the kubelet search, in order: exactly
# internal/hostexec.ChildPATH, which the drop-ins set (a Go test binds the two).
readonly EXEC_PATH_DIRS=(/usr/sbin /usr/bin /sbin /bin)
# Names containerd, its shim, the CNI plugins it starts, or the kubelet resolve by
# name, and which the image MUST therefore provide, so that a directory appended to
# the daemons' PATH can never win the lookup. Each must resolve, from EXEC_PATH_DIRS
# in order, to a root:root 0755 regular file whose canonical path is inside /usr and
# outside the persistent /usr/local.
readonly RESOLVABLE_NAMES=(
  containerd-shim-runc-v2   # containerd, when runtime_path is not honored
  runc                      # the shim, when BinaryName is not absolute
  iptables                  # kubelet startup rules and the periodic canary
  ip6tables
  iptables-save
  iptables-restore
  ip                        # CNI reference plugins (tap, and go-iptables' helpers)
  mount                     # kubelet mount-utils, every projected-token volume
  umount
  systemd-run               # kubelet mount-utils, systemd-run scope for mounts
  losetup                   # kubelet block volumes
  blkid                     # kubelet filesystem probing
)
# Names whose target the BASE IMAGE ships setuid root, which util-linux does for
# mount and umount so that unprivileged users can mount fstab entries. The setuid
# bit is the base OS's choice, it predates anything here, and it is irrelevant to
# what this check is about: the name resolving inside the image so that an appended
# PATH element cannot answer for it. They are listed rather than waved through, and
# asserted at exactly 4755, so that a change in either direction is a finding.
readonly SETUID_RESOLVABLE_NAMES=(mount umount)
# Names we CANNOT satisfy from the image, closed at the source instead. Each entry
# is "<name>=<what closes it>", and the closing assertion follows. They are not
# required to be absent: if a future base image ships one, it simply becomes a
# RESOLVABLE name and the closure stays harmless.
readonly CLOSED_NAMES=(
  'igzip=Environment=CONTAINERD_DISABLE_IGZIP=1 in the containerd drop-in'
  'unpigz=Environment=CONTAINERD_DISABLE_PIGZ=1 in the containerd drop-in'
  'mkfs.erofs=both erofs plugin URIs in disabled_plugins'
)
# The lines that make the closures above true. The drop-in is byte-compared in (6)
# as well; naming the two variables here states the requirement instead of leaving
# it implicit in a hash, and points at the right line when it is dropped.
readonly CONTAINERD_DROPIN=/usr/lib/systemd/system/containerd.service.d/50-provider-kubernetes-exec-path.conf
readonly CONTAINERD_DROPIN_LINES=(
  'Environment=CONTAINERD_DISABLE_IGZIP=1'
  'Environment=CONTAINERD_DISABLE_PIGZ=1'
)
readonly CLOSED_PLUGIN_URIS=(
  io.containerd.differ.v1.erofs
  io.containerd.snapshotter.v1.erofs
)
# (8) directories containerd is configured to execute from, which must ship empty:
# an entry here would run as root at every daemon start (NRI) or on every image
# pull (image verifier).
readonly EMPTY_EXEC_DIRS=(
  /usr/lib/containerd/image-verifier/bin
  /usr/lib/nri/plugins
)

# (10)-(12) ADR-19 U2: the units are a property of the booted image.
# The directory that holds them, the one the kubeadm drop-in lives in, and the
# .wants directory the enablement links live in.
readonly UNIT_DIR=/usr/lib/systemd/system
readonly UNIT_DROPIN_DIR=/usr/lib/systemd/system/kubelet.service.d
readonly WANTS_DIR=/usr/lib/systemd/system/multi-user.target.wants
# Every unit this project owns. Each must be a static fragment under UNIT_DIR and
# be linked from WANTS_DIR by a relative `../<unit>` symlink. The migration unit is
# in the list, so "the migrate unit is present and linked" is not a separate check.
readonly IMAGE_UNITS=(
  containerd.service
  kubelet.service
  provider-kubernetes-image-import.service
  provider-kubernetes-unit-migrate.service
)
# "<path in the image>|<path in the build context>" for the units and the drop-in:
# same rule as EXACT_COPIES, kept separate so --units-only checks them too.
readonly UNIT_EXACT_COPIES=(
  "/usr/lib/systemd/system/containerd.service|usr-lib-systemd/containerd.service"
  "/usr/lib/systemd/system/kubelet.service|usr-lib-systemd/kubelet.service"
  "/usr/lib/systemd/system/provider-kubernetes-image-import.service|usr-lib-systemd/provider-kubernetes-image-import.service"
  "/usr/lib/systemd/system/provider-kubernetes-unit-migrate.service|usr-lib-systemd/provider-kubernetes-unit-migrate.service"
  "/usr/lib/systemd/system/kubelet.service.d/10-kubeadm.conf|usr-lib-systemd/kubelet.service.d/10-kubeadm.conf"
)
# The unit search directories that OUTRANK /usr/lib/systemd/system and are
# persistent on Kairos (path-lookup.c:513-532; /etc/systemd and /usr/local are both
# binds from /usr/local/.state). Nothing provider-owned may ship in any of them.
# Expressed as the two roots that always exist plus the -path patterns below, so
# one find call covers all of them without failing on an absent directory.
readonly PERSISTENT_UNIT_ROOTS=(/etc/systemd /usr/local)
readonly PERSISTENT_UNIT_PATHS=(
  '*/systemd/system/*'
  '*/systemd/system.control/*'
  '*/systemd/system.attached/*'
)
# The names that must not appear there: the fragments and `.wants` links (same
# names) plus the per-unit drop-in directories.
readonly FORBIDDEN_UNIT_NAMES=(
  containerd.service
  kubelet.service
  'provider-kubernetes-*.service'
  containerd.service.d
  kubelet.service.d
  'provider-kubernetes-*.service.d'
)

MODE=full
case "$#" in
  1) : ;;
  2)
    case "$2" in
      --units-only) MODE=units ;;
      *) echo "usage: $0 <image> [--units-only] (got '$2')" >&2; exit 2 ;;
    esac
    ;;
  *) echo "usage: $0 <image> [--units-only]" >&2; exit 2 ;;
esac
readonly MODE
IMG="$1"
case "$IMG" in
  "" | -*) echo "usage: $0 <image> [--units-only] (got an empty or option-like argument '${IMG}')" >&2; exit 2 ;;
esac

fail() { echo "$*" >&2; exit 1; }

# in_image <binary> <args...>: run one of the image's own binaries (GNU coreutils and
# findutils on Hadron) against the image's filesystem. Never pulls, no network.
in_image() {
  local bin="$1"
  shift
  docker run --rm --pull never --network none --entrypoint "$bin" "$IMG" "$@"
}

# check_exact <type> <mode> <path>...: each path must be exactly that file type,
# owned 0:0, with exactly that octal mode as stat %a prints it (a setuid, setgid or
# sticky bit adds a leading fourth digit). stat -c without -L describes each path
# itself, so a symlink is reported as "symbolic link", not as its target.
check_exact() {
  local want_type="$1" want_mode="$2"
  shift 2
  local out
  out="$(in_image /usr/bin/stat -c '%F|%u|%g|%a|%n' -- "$@")" \
    || fail "stat failed for one of: $* (missing from the image?)"
  [ "$(printf '%s\n' "$out" | wc -l)" -eq "$#" ] \
    || { printf '%s\n' "$out" >&2; fail "stat did not report every path"; }
  local ftype uid gid mode path
  while IFS='|' read -r ftype uid gid mode path; do
    [ "$ftype" = "$want_type" ] || fail "${path} is a ${ftype}, want a ${want_type}"
    [ "${uid}:${gid}" = 0:0 ] || fail "${path} is owned by ${uid}:${gid}, want 0:0"
    [ "$mode" = "$want_mode" ] || fail "${path} has mode ${mode}, want ${want_mode}"
    echo "ok: ${path} ${ftype} ${uid}:${gid} ${mode}"
  done <<< "$out"
}

# The build context lives one directory above this script, which is how CI, the
# release workflow and the e2e job all invoke it. Hashing both sides with the
# IMAGE's own sha256sum (the source over stdin) keeps the host requirement at bash
# + docker and compares bytes, not text, so a lost or added trailing newline is a
# failure.
REPO_ROOT="."
case "${BASH_SOURCE[0]}" in
  */*) REPO_ROOT="${BASH_SOURCE[0]%/*}/.." ;;
esac
readonly REPO_ROOT

image_sha256() {
  local out
  out="$(in_image /usr/bin/sha256sum -- "$1")" || return 1
  printf '%s\n' "${out%% *}"
}
# source_sha256 reads the file to hash on stdin, so the build-context file is never
# named inside the container and `docker run -i` streams it in.
source_sha256() {
  local out
  out="$(docker run --rm -i --pull never --network none --entrypoint /usr/bin/sha256sum "$IMG" -)" || return 1
  printf '%s\n' "${out%% *}"
}

# check_exact_copies <pair>...: each "<path in the image>|<path in the context>"
# must be byte-identical on both sides.
check_exact_copies() {
  local pair in_img src want got
  for pair in "$@"; do
    in_img="${pair%%|*}"
    src="${REPO_ROOT}/${pair#*|}"
    [ -f "$src" ] || fail "${src} is missing from the build context, so ${in_img} cannot be checked against it"
    want="$(source_sha256 < "$src")" || fail "could not hash ${src} with the image's sha256sum"
    got="$(image_sha256 "$in_img")" || fail "could not hash ${in_img} in the image"
    [ "$want" = "$got" ] \
      || fail "${in_img} is not byte-identical to ${src} (image sha256 ${got}, source sha256 ${want})"
    echo "ok: ${in_img} is byte-identical to ${src} (sha256 ${got})"
  done
}

# Compare trimmed, uncommented lines: the files are indented, and a value may not
# carry a trailing comment (the Go and shell sandbox_image parsers refuse one too).
# LINES is filled by read_lines and consumed right after each call.
LINES=()
read_lines() {
  local raw line
  raw="$(in_image /usr/bin/cat -- "$1")" || fail "could not read $1 from the image"
  LINES=()
  while IFS= read -r line; do
    line="${line#"${line%%[![:space:]]*}"}"
    line="${line%"${line##*[![:space:]]}"}"
    case "$line" in "" | "#"*) continue ;; esac
    LINES+=("$line")
  done <<< "$raw"
}
# want_once <file> <wanted line>: exactly one uncommented occurrence in LINES.
want_once() {
  local file="$1" want="$2" line n=0
  for line in "${LINES[@]}"; do
    [ "$line" = "$want" ] && n=$((n + 1))
  done
  [ "$n" -eq 1 ] || fail "${file} has ${n} lines '${want}', want exactly 1 (ADR-19 U1)"
  echo "ok: ${file} has exactly one '${want}'"
}

# (10)-(12) The ADR-19 U2 unit layout. This is the whole of --units-only, so it
# repeats the owner/mode and byte-identity rules for the units rather than leaning
# on the arrays the full run checks.
check_image_owned_units() {
  local unit path line target bad n
  # (11a) the directories the units and their links live in.
  check_exact directory 755 "$UNIT_DIR" "$UNIT_DROPIN_DIR" "$WANTS_DIR"

  # (11b) the units and the kubeadm drop-in: root:root 0644 regular files, and
  # byte-identical to the build context.
  local unit_paths=()
  for unit in "${IMAGE_UNITS[@]}"; do
    unit_paths+=("${UNIT_DIR}/${unit}")
  done
  unit_paths+=("${UNIT_DROPIN_DIR}/10-kubeadm.conf")
  check_exact "regular file" 644 "${unit_paths[@]}"
  check_exact_copies "${UNIT_EXACT_COPIES[@]}"

  # (11c) no [Install] section: with one, `systemctl is-enabled` reports "disabled"
  # rather than "static" (install.c:3197-3203), kubeadm's preflight warns, and
  # `systemctl disable` writes a persistent override into /etc.
  for path in "${unit_paths[@]}"; do
    read_lines "$path"
    for line in "${LINES[@]}"; do
      [ "$line" != "[Install]" ] \
        || fail "${path} has an [Install] section; the units are enabled by the ${WANTS_DIR} links and must report is-enabled 'static' (ADR-19 U2)"
    done
    echo "ok: ${path} has no [Install] section"
  done

  # (12) each unit is linked from the .wants directory by a RELATIVE ../<unit>
  # symlink. systemd takes the dependency from the entry NAME and only compares the
  # target's basename with it (load-dropin.c:62-73,75-98,100 at v260.2), so the
  # relative form is both sufficient and unable to point out of the image.
  local links=()
  for unit in "${IMAGE_UNITS[@]}"; do
    links+=("${WANTS_DIR}/${unit}")
  done
  check_exact "symbolic link" 777 "${links[@]}"
  local targets
  targets="$(in_image /usr/bin/readlink -- "${links[@]}")" \
    || fail "readlink failed for one of: ${links[*]}"
  n=0
  while IFS= read -r target; do
    unit="${IMAGE_UNITS[$n]}"
    [ "$target" = "../${unit}" ] \
      || fail "${WANTS_DIR}/${unit} points at '${target}', want exactly '../${unit}' (ADR-19 U2)"
    echo "ok: ${WANTS_DIR}/${unit} -> ../${unit}"
    n=$((n + 1))
  done <<< "$targets"
  [ "$n" -eq "${#IMAGE_UNITS[@]}" ] \
    || fail "readlink reported ${n} targets for ${#IMAGE_UNITS[@]} links"

  # (10) nothing provider-owned in a persistent search directory that outranks
  # /usr/lib/systemd/system. One find over the two roots that always exist,
  # restricted by -path to the unit directories, so an absent
  # /usr/local/lib/systemd/system is simply no match rather than an error.
  local expr=()
  for path in "${PERSISTENT_UNIT_PATHS[@]}"; do
    if [ "${#expr[@]}" -eq 0 ]; then expr+=(-path "$path"); else expr+=(-o -path "$path"); fi
  done
  local names=()
  for path in "${FORBIDDEN_UNIT_NAMES[@]}"; do
    if [ "${#names[@]}" -eq 0 ]; then names+=(-name "$path"); else names+=(-o -name "$path"); fi
  done
  bad="$(in_image /usr/bin/find "${PERSISTENT_UNIT_ROOTS[@]}" -maxdepth 6 \
    \( "${expr[@]}" \) \( "${names[@]}" \) -print)" \
    || fail "find failed in ${PERSISTENT_UNIT_ROOTS[*]}"
  [ -z "$bad" ] || { printf '%s\n' "$bad" >&2; fail "the entries above are provider-owned unit names in a PERSISTENT systemd search directory that outranks ${UNIT_DIR}; on Kairos they would keep overriding the image's units across every upgrade (ADR-19 U2, S19-11)"; }
  echo "ok: no provider-owned unit name under ${PERSISTENT_UNIT_ROOTS[*]} in a systemd unit directory"
}

check_image_owned_units
if [ "$MODE" = units ]; then
  echo "ok: ${IMG} passes the ADR-19 U2 unit checks (--units-only)"
  exit 0
fi

# (1)-(3)
check_exact directory 755 "${DIRS[@]}"
check_exact "regular file" 755 "${BINARIES[@]}"
check_exact "regular file" 644 "${CONFIGS[@]}"

# (4) Built as two find expressions over the same directory, each printing every
# entry that breaks its rule; an empty result is the pass. The documentation names
# are excluded from the first expression and are the whole subject of the second, so
# every entry is covered by exactly one of them and an unexpected new name lands in
# the plugin rule, where a non-executable file fails.
cni_not_doc=()
cni_doc=()
for name in "${CNI_DOC_NAMES[@]}"; do
  cni_not_doc+=(! -name "$name")
  if [ "${#cni_doc[@]}" -eq 0 ]; then cni_doc+=(-name "$name"); else cni_doc+=(-o -name "$name"); fi
done

bad="$(in_image /usr/bin/find "$CNI_BIN_DIR" -mindepth 1 "${cni_not_doc[@]}" \
  ! \( -type f -user 0 -group 0 -perm 0755 \) -print)" \
  || fail "find failed in ${CNI_BIN_DIR}"
[ -z "$bad" ] || { printf '%s\n' "$bad" >&2; fail "the ${CNI_BIN_DIR} entries above are not root:root regular files with mode exactly 0755 (only ${CNI_DOC_NAMES[*]} may be non-executable)"; }

bad="$(in_image /usr/bin/find "$CNI_BIN_DIR" -mindepth 1 \( "${cni_doc[@]}" \) \
  ! \( -type f -user 0 -group 0 -perm 0644 \) -print)" \
  || fail "find failed in ${CNI_BIN_DIR}"
[ -z "$bad" ] || { printf '%s\n' "$bad" >&2; fail "the ${CNI_BIN_DIR} entries above are not root:root regular files with mode exactly 0644"; }

n="$(in_image /usr/bin/find "$CNI_BIN_DIR" -mindepth 1 "${cni_not_doc[@]}" -type f -print | wc -l)" \
  || fail "find failed in ${CNI_BIN_DIR}"
[ "$n" -gt 0 ] || fail "${CNI_BIN_DIR} holds no CNI plugin"
echo "ok: ${n} CNI plugins in ${CNI_BIN_DIR} are root:root regular files with mode exactly 0755, and ${CNI_DOC_NAMES[*]} (if present) exactly 0644"

# (5)
bad="$(in_image /usr/bin/find "${TREE_ROOTS[@]}" ! -type l \( ! -user 0 -o -perm /022 \) -print)" \
  || fail "find failed in ${TREE_ROOTS[*]}"
[ -z "$bad" ] || { printf '%s\n' "$bad" >&2; fail "the entries above are owned by a user other than root or writable by group or others"; }
echo "ok: nothing under ${TREE_ROOTS[*]} is owned by a user other than root or writable by group or others"

# (6) Byte-identity of the U1 drop-ins, with the same helper the unit check uses.
check_exact_copies "${EXACT_COPIES[@]}"

# (7) The containerd config lines, compared as trimmed, uncommented lines.
read_lines "$CONTAINERD_CONFIG"
for want in "${CONTAINERD_CONFIG_LINES[@]}"; do
  want_once "$CONTAINERD_CONFIG" "$want"
done

# (8)
for dir in "${EMPTY_EXEC_DIRS[@]}"; do
  bad="$(in_image /usr/bin/find "$dir" -mindepth 1 -print)" || fail "find failed in ${dir}"
  [ -z "$bad" ] || { printf '%s\n' "$bad" >&2; fail "the entries above ship in ${dir}, which containerd executes as root"; }
  echo "ok: ${dir} ships empty"
done

# (9a) Each RESOLVABLE name must resolve from EXEC_PATH_DIRS in order. readlink -e
# canonicalizes every component and prints nothing for a path that does not exist,
# so the FIRST line it prints is what a PATH lookup would run -- and the value is
# already followed through Hadron's merged-/usr symlinks (/usr/sbin -> bin) and
# through multi-call binaries such as xtables-legacy-multi and busybox.
resolved=()
resolved_setuid=()
for name in "${RESOLVABLE_NAMES[@]}"; do
  candidates=()
  for dir in "${EXEC_PATH_DIRS[@]}"; do
    candidates+=("${dir}/${name}")
  done
  # A missing candidate makes readlink exit non-zero; that is expected, and the
  # printed lines are still the ones that do exist.
  found="$(in_image /usr/bin/readlink -e -- "${candidates[@]}" || true)"
  target="${found%%$'\n'*}"
  [ -n "$target" ] \
    || fail "no ${name} in ${EXEC_PATH_DIRS[*]}: containerd or the kubelet resolve it by name, so a directory appended to their PATH (for example the persistent /opt/nri/bin, R-19-13) could supply it. Ship it in the image, or close the lookup with a setting and add it to CLOSED_NAMES"
  case "$target" in
    /usr/local/*) fail "${name} resolves to ${target}, inside the persistent /usr/local" ;;
    /usr/*) : ;;
    *) fail "${name} resolves to ${target}, outside /usr" ;;
  esac
  echo "ok: ${name} resolves to ${target}"
  setuid=""
  for s in "${SETUID_RESOLVABLE_NAMES[@]}"; do
    [ "$name" = "$s" ] && setuid=1
  done
  if [ -n "$setuid" ]; then
    resolved_setuid+=("$target")
  else
    resolved+=("$target")
  fi
done
# The same path can back several names (xtables-legacy-multi, busybox); check_exact
# reports each path it is given, so a repeat is a repeated "ok" line, not a problem.
check_exact "regular file" 755 "${resolved[@]}"
[ "${#resolved_setuid[@]}" -eq 0 ] || check_exact "regular file" 4755 "${resolved_setuid[@]}"

# (9b) The names the image cannot provide are closed at the source instead. Assert
# what does the closing, not the absence of the binary: if a future base image ships
# one of these, the closure stays correct and (9a)'s rule would cover it anyway.
read_lines "$CONTAINERD_DROPIN"
for want in "${CONTAINERD_DROPIN_LINES[@]}"; do
  want_once "$CONTAINERD_DROPIN" "$want"
done
read_lines "$CONTAINERD_CONFIG"
for uri in "${CLOSED_PLUGIN_URIS[@]}"; do
  hit=""
  for line in "${LINES[@]}"; do
    case "$line" in
      disabled_plugins*"\"${uri}\""*) hit=1; break ;;
    esac
  done
  [ -n "$hit" ] || fail "${CONTAINERD_CONFIG} does not list ${uri} in disabled_plugins; that plugin looks up a name the image does not ship"
  echo "ok: ${CONTAINERD_CONFIG} disables ${uri}"
done
for entry in "${CLOSED_NAMES[@]}"; do
  echo "ok: ${entry%%=*} is not in the image and is closed by ${entry#*=}"
done
