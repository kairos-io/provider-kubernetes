#!/usr/bin/env bash
# verify-bundle-refs.sh <image> <kubernetes-version>
#
# Verify that a built provider-kubernetes image names every pre-bundled
# control-plane image tarball with its EXACT ref (F-IMPORTREF). CI runs it on every
# supported minor and the release workflow runs it before `docker push`, so both
# gates run the same checks.
#
# `ctr images import` names each bundled image from its docker-save RepoTags, and
# kubeadm (CRI ImageStatus) and containerd's sandbox_image look images up by the
# EXACT ref, so an air-gap first boot needs every tarball named exactly as kubeadm
# expects. Asserted against the SHIPPED image:
#   (0) every copied-out entry (the image directory, images.lock, the tarballs and
#       containerd's config.toml) is a regular file, BEFORE anything parses it;
#   (1) images.lock refs == the bundled kubeadm's image list for this release;
#   (2) the *.tar files == the images.lock tarballs (nothing else present);
#   (3) containerd's sandbox_image is one of the bundled refs;
#   (4) each tarball has unique, regular members (one sha256:<hex> config,
#       <hex>.tar.gz layers, manifest.json last) and its manifest.json bytes are
#       exactly [{"Config":"<config>","RepoTags":["<lock ref>"],"Layers":[...]}];
#   (5) the "i-was-a-digest" placeholder appears nowhere.
# Exact bytes, never a JSON projection: containerd's importer matches keys
# case-insensitively and does not bind member bytes to member names. jq only reads
# images.lock, which the build itself generates.
#
# Requires: docker, jq, GNU tar, GNU coreutils/findutils (ubuntu-latest).
set -euo pipefail
export LC_ALL=C

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

# docker create only materializes the image filesystem for docker cp.
cid="$(docker create --pull never "$IMG" /usr/bin/kubeadm)"
docker cp "${cid}:/opt/provider-kubernetes/images" "$work/images"
docker cp "${cid}:/etc/containerd/config.toml" "$work/config.toml"
lock="$work/images/images.lock"

# (0) + (2, part 1): regular files only, before jq or tar read any of them. docker cp
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

# (1)
docker run --rm --entrypoint /usr/bin/kubeadm "$IMG" config images list \
  --kubernetes-version "$K8S" --image-repository registry.k8s.io > "$work/kubeadm.refs"
[ -s "$work/kubeadm.refs" ] || fail "bundled kubeadm ${K8S} listed no images"
jq -r '.images[].ref' "$lock" > "$work/lock.refs"
[ -z "$(sort "$work/lock.refs" | uniq -d)" ] || fail "images.lock has duplicate refs"
diff <(sort "$work/kubeadm.refs") <(sort "$work/lock.refs") \
  || fail "images.lock refs != bundled kubeadm ${K8S} image list (< kubeadm, > images.lock)"

# (2, part 2)
(cd "$work/images" && find . -mindepth 1 -maxdepth 1 -name '*.tar' | sed 's#^\./##' | sort) > "$work/tars.fs"
jq -r '.images[].tarball' "$lock" | sort > "$work/tars.lock"
[ -z "$(uniq -d "$work/tars.lock")" ] || fail "images.lock has duplicate tarball names"
diff "$work/tars.fs" "$work/tars.lock" || fail "*.tar files != images.lock tarballs (< files, > images.lock)"

# (3)
[ "$(grep -cE '^[[:space:]]*sandbox_image[[:space:]]*=' "$work/config.toml")" -eq 1 ] \
  || fail "containerd config.toml must set sandbox_image exactly once"
sandbox="$(sed -nE 's/^[[:space:]]*sandbox_image[[:space:]]*=[[:space:]]*"([^"]+)"[[:space:]]*$/\1/p' "$work/config.toml")"
[ -n "$sandbox" ] || fail "could not parse sandbox_image from containerd config.toml"
grep -qxF -- "$sandbox" "$work/lock.refs" || fail "sandbox_image ${sandbox} is not a bundled image ref"

# (4)
layers_re='^"[0-9a-f]{64}\.tar\.gz"(,"[0-9a-f]{64}\.tar\.gz")*$'
n=0
while IFS=$'\t' read -r ref tarball; do
  [[ "$tarball" =~ ^[A-Za-z0-9._-]+\.tar$ ]] || fail "unexpected tarball name '${tarball}'"
  t="$work/images/${tarball}"
  tar -tf "$t" > "$work/names"
  tar -tvf "$t" > "$work/listing"
  [ "$(wc -l < "$work/names")" -eq "$(wc -l < "$work/listing")" ] || fail "${tarball}: member listings disagree"
  if grep -Evx 'manifest\.json|sha256:[0-9a-f]{64}|[0-9a-f]{64}\.tar\.gz' "$work/names" >&2; then
    fail "${tarball}: unexpected member name(s) above"
  fi
  [ -z "$(sort "$work/names" | uniq -d)" ] || fail "${tarball}: duplicate member names"
  if awk 'substr($1, 1, 1) != "-"' "$work/listing" | grep . >&2; then
    fail "${tarball}: non-regular member(s) above"
  fi
  if [ "$(grep -cx 'manifest\.json' "$work/names")" -ne 1 ] || [ "$(tail -n 1 "$work/names")" != manifest.json ]; then
    fail "${tarball}: manifest.json must be present exactly once, as the last member"
  fi
  [ "$(grep -cEx 'sha256:[0-9a-f]{64}' "$work/names")" -eq 1 ] || fail "${tarball}: want exactly one sha256:<hex> config member"
  cfg="$(grep -Ex 'sha256:[0-9a-f]{64}' "$work/names")"
  tar -xOf "$t" manifest.json > "$work/manifest.json"
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
  size="$(stat -c %s "$work/manifest.json")"
  want="$(printf '%s' "${pre}${mid}${suf}" | wc -c)"
  [ "$size" -eq "$want" ] \
    || fail "${tarball}: manifest.json has ${size} bytes, want ${want} (trailing or hidden bytes)"
  [ "$(grep -o '"RepoTags":\[' "$work/manifest.json" | wc -l)" -eq 1 ] \
    || fail "${tarball}: \"RepoTags\":[ must occur exactly once"
  diff <(tr ',' '\n' <<<"$mid" | tr -d '"' | sort -u) \
    <(grep -Ex '[0-9a-f]{64}\.tar\.gz' "$work/names" | sort -u) >/dev/null \
    || fail "${tarball}: manifest.json Layers != layer members"
  echo "ok: ${tarball} RepoTags=[\"${ref}\"] layers=$(grep -cEx '[0-9a-f]{64}\.tar\.gz' "$work/names")"
  n=$((n + 1))
done < <(jq -r '.images[] | [.ref, .tarball] | @tsv' "$lock")
[ "$n" -eq "$(wc -l < "$work/lock.refs")" ] || fail "checked ${n} tarballs, images.lock lists $(wc -l < "$work/lock.refs")"

# (5)
if grep -rlF i-was-a-digest "$work/images" >&2; then
  fail "the i-was-a-digest placeholder is still present in the file(s) above"
fi
echo "OK: ${n} bundled tarballs are named with their exact refs; sandbox_image ${sandbox} is bundled"
