//go:build e2e

package e2e

// bundled_images_pull_test.go is IR-10, the gate for the defect class ADR-16-A3
// closes: a bundled image that is intact right after import but is broken by a
// LATER successful pull of the same ref.
//
// IR-9 structurally cannot see it: it checks CRI visibility right after the
// import, before any pull exists, and must stay a never-pulled steady-state
// check. An air-gapped VM proof cannot either: the window only exists when a pull
// SUCCEEDS, so this is a connected-mode defect. With a docker-save tarball
// containerd re-serialized a manifest at import (a digest in no registry); a
// later CRI pull re-pointed the record at the registry's index, orphaned that
// manifest, GC deleted it, and CreateContainer failed with "content digest ...:
// not found", failing `kubeadm init` (CI run 45887422562). Under OCI layout the
// record targets the registry's own manifest, so the pull finds it already local
// and nothing is orphaned.
//
// IR-10 asserts, for every images.lock ref, before and after a pull of a
// representative pair (etcd, the image that failed, and pause):
//   - `ctr -n k8s.io images check` reports the record complete (N/N);
//   - the record's target digest is present in `ctr -n k8s.io content ls`;
//   - the lock-pinned manifest blob carries containerd.io/gc.ref.content.config
//     and .l.<i> for every layer (the GC-rooting condition: without them the
//     children are unrooted). `ctr content info` does not exist in containerd
//     2.3.5; the LABELS column of `content ls` is the instrument.
//
// After the pull it watches, for a bounded window that spans containerd's GC, that
// the images stay complete and `crictl inspecti` keeps succeeding, and that no
// import-named record appeared. It asserts COMPLETENESS, not digest stability:
// after a pull the record's target legitimately moves from the amd64 manifest to
// the registry index.

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kairos-io/provider-kubernetes/internal/hostexec"
)

const (
	// pullTimeout bounds one `crictl pull` of a control-plane image.
	pullTimeout = 5 * time.Minute
	// gcObservationWindow bounds the post-pull watch (<= 60s): long enough for
	// containerd's GC, which is triggered by the pull's mutations, to have run.
	gcObservationWindow = 60 * time.Second
	// gcObservationTick is the interval between re-assertions in the window.
	gcObservationTick = 5 * time.Second
	// egressProbeTimeout bounds the reachability probe of the registry.
	egressProbeTimeout = 10 * time.Second
	// requireEgressEnv turns "no egress" from a skip into a failure, for CI
	// runners that are expected to have it.
	requireEgressEnv = "E2E_REQUIRE_EGRESS"
)

// TestBundledImagesSurvivePull (IR-10).
func TestBundledImagesSurvivePull(t *testing.T) {
	requireRegistryEgress(t)

	nc := startNode(t, uniqueName("imgpull"))
	lock := readBundleLock(t, nc, "IR-10")

	// The same settled, successful boot import IR-9 (a) requires; otherwise the
	// assertions below race the unit.
	props := waitImportUnitSettled(t, nc)
	if v := importUnitViolations(props); len(v) > 0 {
		t.Fatalf("IR-10: %s did not complete successfully: %s (properties %v)", imageImportUnit, strings.Join(v, "; "), props)
	}
	boot := waitImportJournalSummary(t, nc, "IR-10")
	if v := importRunViolations(importOutput{Summary: boot}, lock, wantImport{Outcome: "success", SummaryOnly: true}); len(v) > 0 {
		t.Fatalf("IR-10: the boot-time import did not import every images.lock entry: %s (summary %s)", strings.Join(v, "; "), boot)
	}

	// What each lock ref's manifest says, read from its own tarball: the labels
	// containerd must have written are derived from it.
	manifests := map[string]ociManifestInfo{}
	for _, img := range lock.Images {
		manifests[img.Ref] = bundledManifest(t, nc, img)
	}

	// Before any pull: every ref intact and rooted.
	if v := pullIntegrityViolations(t, nc, lock, manifests, phaseBeforePull); len(v) > 0 {
		t.Fatalf("IR-10: before any pull the bundled images are not intact: %s", strings.Join(v, "; "))
	}
	t.Logf("IR-10: before the pull all %d images are complete, their targets are in the content store and their manifests are GC-rooted", len(lock.Images))

	// The pair: etcd failed in CI; pause is the sandbox image every pod needs.
	var pulled []string
	for _, img := range lock.Images {
		if strings.Contains(img.Ref, "/etcd:") || strings.Contains(img.Ref, "/pause:") {
			pulled = append(pulled, img.Ref)
		}
	}
	if len(pulled) != 2 {
		t.Fatalf("IR-10: images.lock has %d etcd/pause entries (%v), want 2", len(pulled), pulled)
	}
	for _, ref := range pulled {
		start := time.Now()
		out, stderr, err := nc.ExecStdoutTimeout(pullTimeout, binCrictl,
			"--runtime-endpoint", criEndpoint, "--image-endpoint", criEndpoint, "pull", ref)
		if err != nil {
			t.Fatalf("IR-10: crictl pull %s: %v\n%s\n%s", ref, err, trimForLog(out), strings.TrimSpace(stderr))
		}
		t.Logf("IR-10: crictl pull %s succeeded in %s", ref, time.Since(start).Round(time.Second))
	}

	// Bounded watch past GC. Every tick re-asserts; the first violation fails the
	// test at once, and a clean window ends it at the deadline (never unbounded).
	deadline := time.Now().Add(gcObservationWindow)
	ticks := 0
	for {
		if v := pullIntegrityViolations(t, nc, lock, manifests, phaseAfterPull); len(v) > 0 {
			t.Fatalf("IR-10: %d tick(s) after the pull the bundled images are no longer intact: %s", ticks, strings.Join(v, "; "))
		}
		for _, ref := range pulled {
			if _, stderr, err := nc.ExecStdoutTimeout(dockerTimeout, binCrictl,
				"--runtime-endpoint", criEndpoint, "--image-endpoint", criEndpoint, "inspecti", "-o", "json", ref); err != nil {
				t.Fatalf("IR-10: crictl inspecti %s failed after the pull (kubeadm would pull, CreateContainer would fail): %v\n%s", ref, err, strings.TrimSpace(stderr))
			}
		}
		if bad := misnamedImportedImages(ctrImageNames(t, nc, "IR-10")); len(bad) > 0 {
			t.Fatalf("IR-10: containerd k8s.io holds image(s) under a placeholder or import- name after the pull: %v", bad)
		}
		ticks++
		if !time.Now().Add(gcObservationTick).Before(deadline) {
			break
		}
		time.Sleep(gcObservationTick)
	}
	t.Logf("IR-10: over %d assertion round(s) up to %s after pulling %v the images stayed complete and GC-rooted, inspecti succeeded and no import- record appeared",
		ticks, gcObservationWindow, pulled)

	// (e) POSITIVE CONTROL. Neither detector in (d) means anything until it has
	// been shown to fire: a gate that cannot fail is indistinguishable from one
	// that passes for the wrong reason. Rather than stand up a registry serving
	// substituted content, inject the same end state locally -- re-point one
	// bundled ref's record at a DIFFERENT bundled image -- and require both
	// detectors to report it. `ctr images tag --force` deletes the existing
	// record and recreates it against the donor's target (containerd v2.3.5
	// cmd/ctr/commands/images/tag.go), which is exactly what a substituted tag
	// resolve produces: a record under the right name holding the wrong content.
	//
	// This runs LAST and leaves the node deliberately inconsistent; it is torn
	// down immediately after.
	assertDriftDetectorsFire(t, nc, lock, manifests, pulled)
}

// assertDriftDetectorsFire is IR-10 (e). It corrupts one ref's name->content
// binding and requires BOTH (d) detectors to catch it, naming each so a future
// refactor cannot silently drop one and leave the other carrying the gate.
func assertDriftDetectorsFire(t *testing.T, nc *nodeContainer, lock imagesLock, manifests map[string]ociManifestInfo, pulled []string) {
	t.Helper()
	victim, donor := pulled[0], pulled[1]
	if _, stderr, err := nc.ExecStdoutTimeout(dockerTimeout, hostexec.CtrPath, "-n", "k8s.io",
		"images", "tag", "--force", donor, victim); err != nil {
		t.Fatalf("IR-10 (e): could not re-point %s at %s to test the detectors: %v\n%s",
			victim, donor, err, strings.TrimSpace(stderr))
	}
	t.Logf("IR-10 (e): re-pointed %s at %s's content; both (d) detectors must now fire", victim, donor)

	// The CRI image store updates from containerd events, so poll briefly rather
	// than assuming the change is visible the instant ctr returns.
	deadline := time.Now().Add(gcObservationWindow)
	var last []string
	for {
		last = pullIntegrityViolations(t, nc, lock, manifests, phaseAfterPull)
		var primary, secondary bool
		for _, v := range last {
			if !strings.HasPrefix(v, victim+":") {
				continue
			}
			if strings.Contains(v, "no longer resolves to the bundled content") {
				primary = true
			}
			if strings.Contains(v, "is not a digest images.lock") {
				secondary = true
			}
		}
		if primary && secondary {
			t.Logf("IR-10 (e): both detectors fired for %s -- the config-digest check and the record-target check", victim)
			return
		}
		if !time.Now().Add(gcObservationTick).Before(deadline) {
			t.Fatalf("IR-10 (e): after re-pointing %s at %s, primary(config-digest)=%t secondary(record-target)=%t; "+
				"a detector that does not fire here is vacuous. Violations seen: %s",
				victim, donor, primary, secondary, strings.Join(last, "; "))
		}
		time.Sleep(gcObservationTick)
	}
}

// requireRegistryEgress skips the test (never passes silently) when the registry
// is unreachable from the test host, or fails it when requireEgressEnv is set.
// Any HTTP answer, including 401, proves reachability.
func requireRegistryEgress(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), egressProbeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+bundleImageRepository+"/v2/", nil)
	if err != nil {
		t.Fatalf("IR-10: build the egress probe: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err == nil {
		_ = resp.Body.Close()
		return
	}
	if os.Getenv(requireEgressEnv) != "" {
		t.Fatalf("IR-10: no egress to %s and %s is set: %v", bundleImageRepository, requireEgressEnv, err)
	}
	t.Skipf("IR-10 needs egress to %s (a pull must succeed for the defect window to exist): %v", bundleImageRepository, err)
}

// bundledManifest reads img's manifest blob out of its tarball (index.json names
// it) and parses it.
func bundledManifest(t *testing.T, nc *nodeContainer, img imagesLockItem) ociManifestInfo {
	t.Helper()
	tarPath := hostexec.BundleDir + "/" + img.Tarball
	indexRaw, stderr, err := nc.ExecStdoutTimeout(tarballReadTimeout, binTar, "-xOf", tarPath, "index.json")
	if err != nil {
		t.Fatalf("IR-10 %s: read index.json from %s: %v\n%s", img.Ref, tarPath, err, strings.TrimSpace(stderr))
	}
	idx, err := parseOCIIndex(indexRaw)
	if err != nil {
		t.Fatalf("IR-10 %s: %s: %v\n%s", img.Ref, tarPath, err, trimForLog(indexRaw))
	}
	if idx.ManifestDigest != img.ManifestDigest {
		t.Fatalf("IR-10 %s: %s names manifest %s, images.lock pins %s", img.Ref, img.Tarball, idx.ManifestDigest, img.ManifestDigest)
	}
	raw, stderr, err := nc.ExecStdoutTimeout(tarballReadTimeout, binTar, "-xOf", tarPath, ociBlobPrefix+strings.TrimPrefix(idx.ManifestDigest, "sha256:"))
	if err != nil {
		t.Fatalf("IR-10 %s: read the manifest blob from %s: %v\n%s", img.Ref, tarPath, err, strings.TrimSpace(stderr))
	}
	m, err := parseOCIManifest(raw)
	if err != nil {
		t.Fatalf("IR-10 %s: %s: %v\n%s", img.Ref, tarPath, err, trimForLog(raw))
	}
	return m
}

// pullIntegrityViolations runs the five per-ref assertions for every lock ref
// against the node's current containerd state and returns every violation.
// pullPhase selects which name->content binding IR-10 (d) requires. The two
// are deliberately NOT interchangeable: set membership cannot tell "never
// pulled" from "pulled and got exactly what we attested", so the strict form is
// kept wherever it is still true.
type pullPhase int

const (
	// phaseBeforePull: nothing has pulled yet, so the record must still target
	// the per-platform manifest the boot import pinned. Strict equality.
	phaseBeforePull pullPhase = iota
	// phaseAfterPull: a legitimate tag pull re-points the record at the index,
	// so the target must be one of the digests images.lock attests.
	phaseAfterPull
)

// attestedDigests returns the digests images.lock attests for one image, newest
// relation first: the per-platform manifest the import pins, and the
// cosign-verified index a tag pull resolves to. Derived from the lock rather
// than hardcoded as a pair, because two is today's cardinality, not the
// invariant -- a lock that later attests a third digest must widen this set
// without touching the assertion.
func attestedDigests(img imagesLockItem) []string {
	var d []string
	for _, c := range []string{img.ManifestDigest, img.Digest} {
		if c != "" {
			d = append(d, c)
		}
	}
	return d
}

// configDigestViolation is IR-10 (d)'s PRIMARY check: the image the CRI resolves
// this ref to must have the config digest of the manifest images.lock pins.
//
// This is a strictly better substitution detector than the record target, and it
// corrects a premise I had wrong. The config blob is byte-identical from bundle
// or registry only when the CONTENT is the same and merely the manifest
// serialization differs -- which is why IR-9 (c) could not see F-IMPORTBLOB. It
// is NOT identical under substitution: the config carries rootfs.diff_ids, so
// different layer content gives different diff_ids, hence different config bytes,
// hence a different digest. containerd then enforces the chain below it,
// verifying each applied diff against the config's diffIDs and the layer count,
// so an attacker who keeps the config bytes identical but changes layers fails
// closed at unpack.
//
// One expected value, and it is invariant across both the index re-point a
// legitimate pull causes AND a mirror that recompresses or converts mediaTypes
// (neither changes diff_ids). So unlike the record-target check it has no
// phase and no legitimate drift.
func configDigestViolation(img imagesLockItem, want ociManifestInfo, criID string) string {
	if want.ConfigID == "" {
		return "the bundled manifest for this ref names no config digest, so provenance cannot be checked"
	}
	if criID != want.ConfigID {
		return "the CRI resolves this ref to image id " + criID + ", but the manifest images.lock pins " +
			"has config digest " + want.ConfigID + "; the name no longer resolves to the bundled content"
	}
	return ""
}

// driftViolation reports the IR-10 (d) violation for one ref, or "" when the
// record's target is what the phase requires.
//
// This is the assertion that makes R-IMGPROV detectable: a successful CRI pull
// of a bundled ref replaces verified bundle content with registry content under
// the same name, and nothing else in the suite compares what containerd resolves
// that name to against what images.lock attests. IR-9 (c) cannot: it compares
// the CRI image ID, which is the CONFIG digest and is byte-identical whether the
// bytes came from the bundle or the registry.
//
// Two limits, stated so nobody over-reads a pass. It constrains containerd's
// name->content binding, NOT what running containers were created from: a
// container started before a pull keeps running the old content. And a target
// outside the attested set has benign causes as well as hostile ones.
func driftViolation(img imagesLockItem, target string, phase pullPhase) string {
	if phase == phaseBeforePull {
		if target != img.ManifestDigest {
			return "before any pull the record targets " + target + ", want the manifest digest " +
				img.ManifestDigest + " that the boot import pins"
		}
		return ""
	}
	for _, want := range attestedDigests(img) {
		if target == want {
			return ""
		}
	}
	// "attests" would be a false claim for an entry the build could not verify:
	// build/bundle-images.sh records verified:false with a reason (e.g.
	// no-upstream-signature) and for those `digest` is only the digest the tag
	// resolved to at build time, i.e. trust-on-first-use, not a signed value.
	provenance := "images.lock attests"
	if !img.Verified {
		provenance = "images.lock records (UNSIGNED"
		if img.VerifyReason != "" {
			provenance += ": " + img.VerifyReason
		}
		provenance += ")"
	}
	return "the record targets " + target + ", which is not a digest " + provenance + " for this ref (" +
		strings.Join(attestedDigests(img), " or ") + "); investigate rather than assume substitution, " +
		"since an upstream tag re-push, a pull-through mirror that re-serializes the index, or stale " +
		"/var/lib/containerd state from before an OS upgrade all land here too"
}

func pullIntegrityViolations(t *testing.T, nc *nodeContainer, lock imagesLock, manifests map[string]ociManifestInfo, phase pullPhase) []string {
	t.Helper()
	var v []string

	checkOut, checkStderr, err := nc.ExecStdoutTimeout(dockerTimeout, hostexec.CtrPath, "-n", "k8s.io", "images", "check")
	if err != nil {
		return append(v, "ctr images check: "+err.Error()+": "+strings.TrimSpace(checkStderr))
	}
	rows, err := parseCtrImagesCheck(checkOut)
	if err != nil {
		return append(v, err.Error())
	}
	contentOut, contentStderr, err := nc.ExecStdoutTimeout(dockerTimeout, hostexec.CtrPath, "-n", "k8s.io", "content", "ls")
	if err != nil {
		return append(v, "ctr content ls: "+err.Error()+": "+strings.TrimSpace(contentStderr))
	}
	content, err := parseCtrContentLs(contentOut)
	if err != nil {
		return append(v, err.Error())
	}

	byRef := map[string]ctrImageCheck{}
	for _, r := range rows {
		byRef[r.Ref] = r
	}
	for _, img := range lock.Images {
		row, ok := byRef[img.Ref]
		switch {
		case !ok:
			v = append(v, img.Ref+": no record in ctr images check")
		case !row.Complete():
			v = append(v, fmt.Sprintf("%s: record %s is %s (%d/%d), want complete N/N", img.Ref, row.Digest, row.Status, row.Present, row.Required))
		}
		if ok {
			if _, present := content[row.Digest]; !present {
				v = append(v, img.Ref+": the record's target "+row.Digest+" is not in ctr content ls")
			}
			// (d) secondary, CI-only: the name->content binding containerd
			// holds must be one images.lock records. Kept out of production
			// because a mirror re-push makes the legitimate value set
			// open-ended.
			if dv := driftViolation(img, row.Digest, phase); dv != "" {
				v = append(v, img.Ref+": "+dv)
			}
		}
		// (d) PRIMARY: what the CRI actually resolves this ref to. Every lock
		// ref, not just the pulled pair, and phase-independent. This is the
		// assertion that makes R-IMGPROV detectable at all.
		{
			status, stderr, err := nc.ExecStdoutTimeout(dockerTimeout, binCrictl,
				"--runtime-endpoint", criEndpoint, "--image-endpoint", criEndpoint,
				"inspecti", "-o", "json", img.Ref)
			if err != nil {
				v = append(v, img.Ref+": crictl inspecti: "+err.Error()+": "+strings.TrimSpace(stderr))
				continue
			}
			criID, _, perr := parseCrictlImageStatus(status)
			if perr != nil {
				v = append(v, img.Ref+": "+perr.Error())
				continue
			}
			if cv := configDigestViolation(img, manifests[img.Ref], criID); cv != "" {
				v = append(v, img.Ref+": "+cv)
			}
		}
		manifest, present := content[img.ManifestDigest]
		if !present {
			v = append(v, img.Ref+": the manifest blob "+img.ManifestDigest+" is not in ctr content ls")
			continue
		}
		for _, gv := range gcRootViolations(manifests[img.Ref], manifest.Labels) {
			v = append(v, img.Ref+": manifest "+img.ManifestDigest+": "+gv)
		}
	}
	return v
}
