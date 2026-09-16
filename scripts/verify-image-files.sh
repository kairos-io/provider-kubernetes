#!/usr/bin/env bash
# verify-image-files.sh <image>
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
  /etc/systemd/system/kubelet.service.d
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
# Configuration files the Dockerfile installs.
readonly CONFIGS=(
  /etc/containerd/config.toml
  /etc/systemd/system/containerd.service
  /etc/systemd/system/kubelet.service
  /etc/systemd/system/kubelet.service.d/10-kubeadm.conf
  /etc/systemd/system/provider-kubernetes-image-import.service
  /etc/sysctl.d/k8s.conf
  /etc/modules-load.d/k8s.conf
)
readonly CNI_BIN_DIR=/opt/cni/bin
# The non-plugin files the CNI plugins release tarball ships next to the binaries.
# They are documentation, so they are the only /opt/cni/bin entries allowed to be
# non-executable, and they are allowed only at exactly 0644.
readonly CNI_DOC_NAMES=(LICENSE README.md)
readonly TREE_ROOTS=(/usr/bin /system)

if [ "$#" -ne 1 ]; then
  echo "usage: $0 <image>" >&2
  exit 2
fi
IMG="$1"
case "$IMG" in
  "" | -*) echo "usage: $0 <image> (got an empty or option-like argument '${IMG}')" >&2; exit 2 ;;
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
