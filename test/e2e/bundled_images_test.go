//go:build e2e

package e2e

// bundled_images_test.go is IR-9, the Tier-1 gate for ADR-16-A1 / F-IMPORTREF
// (see bundled_images.go for why exact references matter). It runs in its own
// node container, apart from the E-B7 shims in TestSingleNodeInitConverges, and
// deliberately never pre-pulls: every image it finds in containerd must have
// come from the bundled tarballs.

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kairos-io/provider-kubernetes/internal/hostexec"
)

const (
	// importUnitTimeout bounds the wait for the boot-time import unit: its own
	// TimeoutStartSec (360s) plus margin, so a stuck unit fails the test instead
	// of hanging it.
	importUnitTimeout = 7 * time.Minute
	// tarballReadTimeout bounds streaming manifest.json out of one image tarball.
	tarballReadTimeout = 2 * time.Minute
	// bundleImageRepository is the repository the image bundles from, and the one
	// kubeadm's own image list is taken against.
	bundleImageRepository = "registry.k8s.io"
)

// TestBundledImagesImportUnderExactRefs (IR-9): on a node that never pulled
// anything, the images the boot-time import loaded from the bundle are present
// under exactly the references the bundled kubeadm and containerd ask for.
func TestBundledImagesImportUnderExactRefs(t *testing.T) {
	nc := startNode(t, uniqueName("imgrefs"))
	k8sVer := kubernetesVersion()

	// The lock is the bundle's own record of what it holds; reading it cannot
	// race the import. It must describe the Kubernetes version under test, or
	// every later comparison is against the wrong image.
	rawLock, lockStderr, err := nc.ExecStdoutTimeout(dockerTimeout, binCat, imagesLockPath)
	if err != nil {
		t.Fatalf("IR-9: read %s: %v\n%s", imagesLockPath, err, lockStderr)
	}
	lock, err := parseImagesLock(rawLock)
	if err != nil {
		t.Fatalf("IR-9: %v\n%s", err, trimForLog(rawLock))
	}
	if lock.KubernetesVersion != k8sVer || lock.ImageRepository != bundleImageRepository {
		t.Fatalf("IR-9: images.lock is for %s from %s, want %s from %s (wrong node image?)",
			lock.KubernetesVersion, lock.ImageRepository, k8sVer, bundleImageRepository)
	}
	t.Logf("IR-9: images.lock lists %d images for %s: %v", len(lock.Images), lock.KubernetesVersion, lock.refs())

	// (a) The node image leaves the import unit enabled, so it runs at container
	// boot. Wait for it to settle and require a real, successful run before
	// looking at containerd: otherwise the checks race the unit, or pass on a
	// unit that was skipped (a failed ConditionPathExists also reports
	// Result=success).
	props := waitImportUnitSettled(t, nc)
	if v := importUnitViolations(props); len(v) > 0 {
		// Best effort: the unit's own journal usually says why.
		journal, _, _ := nc.ExecStdoutTimeout(dockerTimeout, binJournalctl, "-u", imageImportUnit, "--no-pager", "-n", "50")
		t.Fatalf("IR-9 (a): %s did not complete successfully: %s (properties %v)\n--- journal ---\n%s",
			imageImportUnit, strings.Join(v, "; "), props, trimForLog(journal))
	}
	t.Logf("IR-9 (a): %s: %v", imageImportUnit, props)

	// (b) The bundle must hold exactly the images the bundled kubeadm requires:
	// a missing one is pulled at init (no air gap), an extra one is unaccounted
	// for.
	kubeadmOut, kubeadmStderr, err := nc.ExecStdoutTimeout(dockerTimeout, hostexec.KubeadmPath,
		"config", "images", "list", "--kubernetes-version", k8sVer, "--image-repository", bundleImageRepository)
	if err != nil {
		t.Fatalf("IR-9 (b): kubeadm config images list: %v\n%s", err, kubeadmStderr)
	}
	required, err := parseImageRefList(kubeadmOut)
	if err != nil {
		t.Fatalf("IR-9 (b): kubeadm config images list: %v\n%s", err, trimForLog(kubeadmOut))
	}
	if missing, extra := setDiff(required, lock.refs()), setDiff(lock.refs(), required); len(missing) > 0 || len(extra) > 0 {
		t.Errorf("IR-9 (b): images.lock does not match kubeadm %s's image list: missing %v, extra %v", k8sVer, missing, extra)
	} else {
		t.Logf("IR-9 (b): images.lock == kubeadm config images list (%d refs)", len(required))
	}

	// Every image name in containerd's k8s.io namespace, for (c) and (d). Taken
	// before the explicit import below, so (c)-(e) judge what the boot-time import
	// alone produced.
	ctrOut, ctrStderr, err := nc.ExecStdoutTimeout(dockerTimeout, hostexec.CtrPath, "-n", "k8s.io", "images", "ls", "-q")
	if err != nil {
		t.Fatalf("IR-9: ctr -n k8s.io images ls -q: %v\n%s", err, ctrStderr)
	}
	ctrNames, err := parseImageRefList(ctrOut)
	if err != nil {
		t.Fatalf("IR-9: ctr -n k8s.io images ls -q: %v\n%s", err, trimForLog(ctrOut))
	}

	// (c) Each bundled image resolves by its exact ref, and it is the bundled
	// image, not merely something with that name.
	passed := map[string]bool{}
	for _, img := range lock.Images {
		if checkImportedImage(t, nc, img, ctrNames) {
			passed[img.Ref] = true
		}
	}

	// (d) Nothing was imported under crane's digest placeholder tag or ctr's
	// generated name: either means a tarball was imported under a name nothing
	// looks up.
	if bad := misnamedImportedImages(ctrNames); len(bad) > 0 {
		t.Errorf("IR-9 (d): containerd k8s.io holds image(s) imported under a placeholder name: %v", bad)
	} else {
		t.Logf("IR-9 (d): no k8s.io image name contains i-was-a-digest or starts with import- (%d names)", len(ctrNames))
	}

	// (e) containerd resolves its pod sandbox image by ref too; if it is not
	// bundled and present, the first pod sandbox pulls it even with every
	// control-plane image in place.
	cfg, cfgStderr, err := nc.ExecStdoutTimeout(dockerTimeout, binCat, containerdConfigPath)
	if err != nil {
		t.Fatalf("IR-9 (e): read %s: %v\n%s", containerdConfigPath, err, cfgStderr)
	}
	sandbox, err := parseSandboxImage(cfg)
	if err != nil {
		t.Fatalf("IR-9 (e): %v", err)
	}
	switch {
	case !containsString(lock.refs(), sandbox):
		t.Errorf("IR-9 (e): containerd sandbox_image %s is not in images.lock %v", sandbox, lock.refs())
	case !passed[sandbox]:
		t.Errorf("IR-9 (e): containerd sandbox_image %s is bundled but failed the exact-ref checks above", sandbox)
	default:
		t.Logf("IR-9 (e): containerd sandbox_image %s is bundled and present under its exact ref", sandbox)
	}

	// (a) Finally, one explicit import through the same subcommand: it must succeed
	// and import one tarball per lock entry, so the bundle holds exactly what the
	// lock records.
	importOut, err := nc.ExecTimeout(importImagesTimeout, providerBinaryPath, "import-images")
	if err != nil {
		t.Fatalf("IR-9 (a): import-images failed: %v\n%s", err, trimForLog(importOut))
	}
	imported, importDir, err := importedTarballCount(importOut)
	if err != nil {
		t.Fatalf("IR-9 (a): import-images output: %v\n%s", err, trimForLog(importOut))
	}
	if imported != len(lock.Images) || importDir != bundledImagesDir {
		t.Fatalf("IR-9 (a): import-images imported %d tarball(s) from %s, want %d (one per images.lock entry) from %s",
			imported, importDir, len(lock.Images), bundledImagesDir)
	}
	t.Logf("IR-9 (a): import-images exited 0 and imported %d tarball(s) from %s", imported, importDir)
}

// waitImportUnitSettled polls the boot-time import unit, bounded by
// importUnitTimeout, until importUnitSettled, and returns its properties.
func waitImportUnitSettled(t *testing.T, nc *nodeContainer) map[string]string {
	t.Helper()
	deadline := time.Now().Add(importUnitTimeout)
	var last map[string]string
	var lastErr error
	for {
		out, stderr, err := nc.ExecStdoutTimeout(dockerTimeout, hostexec.SystemctlPath,
			"show", "--property="+strings.Join(importUnitProps, ","), imageImportUnit)
		if err != nil {
			lastErr = fmt.Errorf("systemctl show: %w: %s", err, strings.TrimSpace(stderr))
		} else if props, perr := parseSystemdShow(out); perr != nil {
			lastErr = perr
		} else {
			last, lastErr = props, nil
			if importUnitSettled(props) {
				return props
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("IR-9 (a): %s did not settle within %s; last properties %v, last error %v",
				imageImportUnit, importUnitTimeout, last, lastErr)
		}
		time.Sleep(2 * time.Second)
	}
}

// checkImportedImage runs the IR-9 (c) checks for one bundled image and reports
// every failure. It returns true only if all of them pass:
//   - crictl inspecti by the exact ref succeeds: this is the lookup kubeadm
//     makes before deciding to pull;
//   - the CRI image ID is the config digest from that ref's own tarball, so the
//     name points at the bundled image;
//   - the ref is among the image's CRI repoTags;
//   - the exact ref is an image name in containerd's k8s.io namespace.
func checkImportedImage(t *testing.T, nc *nodeContainer, img imagesLockItem, ctrNames []string) bool {
	t.Helper()
	ok := true

	// parseImagesLock allows only a plain *.tar file name, so this stays inside
	// the bundle directory.
	tarPath := bundledImagesDir + "/" + img.Tarball
	manifest, tarStderr, err := nc.ExecStdoutTimeout(tarballReadTimeout, binTar, "-xOf", tarPath, "manifest.json")
	var configID string
	var tarTags []string
	if err != nil {
		t.Errorf("IR-9 (c) %s: read manifest.json from %s: %v\n%s", img.Ref, tarPath, err, strings.TrimSpace(tarStderr))
		ok = false
	} else if configID, tarTags, err = parseDockerSaveManifest(manifest); err != nil {
		t.Errorf("IR-9 (c) %s: %s: %v\n%s", img.Ref, tarPath, err, trimForLog(manifest))
		ok = false
	}

	status, crictlStderr, err := nc.ExecStdoutTimeout(dockerTimeout, binCrictl,
		"--runtime-endpoint", criEndpoint, "--image-endpoint", criEndpoint,
		"inspecti", "-o", "json", img.Ref)
	if err != nil {
		t.Errorf("IR-9 (c) %s: crictl inspecti by the exact ref failed, so kubeadm would try to pull it (tarball RepoTags %v): %v\n%s",
			img.Ref, tarTags, err, strings.TrimSpace(crictlStderr))
		ok = false
	} else {
		id, repoTags, perr := parseCrictlImageStatus(status)
		switch {
		case perr != nil:
			t.Errorf("IR-9 (c) %s: %v\n%s", img.Ref, perr, trimForLog(status))
			ok = false
		default:
			if configID != "" && id != configID {
				t.Errorf("IR-9 (c) %s: CRI image id %s != config digest %s from %s", img.Ref, id, configID, img.Tarball)
				ok = false
			}
			if !containsString(repoTags, img.Ref) {
				t.Errorf("IR-9 (c) %s: not among the CRI repoTags %v", img.Ref, repoTags)
				ok = false
			}
		}
	}

	if !containsString(ctrNames, img.Ref) {
		t.Errorf("IR-9 (c) %s: not an image name in ctr -n k8s.io images ls", img.Ref)
		ok = false
	}

	if ok {
		t.Logf("IR-9 (c) %s: CRI id %s == config of %s (tarball RepoTags %v); in CRI repoTags; in ctr k8s.io", img.Ref, configID, img.Tarball, tarTags)
	}
	return ok
}
