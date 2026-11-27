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
	if v := pullIntegrityViolations(t, nc, lock, manifests); len(v) > 0 {
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
		if v := pullIntegrityViolations(t, nc, lock, manifests); len(v) > 0 {
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

// pullIntegrityViolations runs the three per-ref assertions for every lock ref
// against the node's current containerd state and returns every violation.
func pullIntegrityViolations(t *testing.T, nc *nodeContainer, lock imagesLock, manifests map[string]ociManifestInfo) []string {
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
