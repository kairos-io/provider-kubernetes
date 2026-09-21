package main

import (
	"bytes"
	"encoding/json"
	"os"
	"runtime/debug"
	"strings"
	"testing"

	"github.com/kairos-io/kairos-sdk/clusterplugin"
	"github.com/mudler/go-pluggable"
	yip "github.com/mudler/yip/pkg/schema"
	"github.com/sirupsen/logrus"

	"github.com/kairos-io/provider-kubernetes/internal/clusterconfigdir"
)

// TestHandleProviderInfo verifies that handleProviderInfo returns a valid EventResponse
// whose Data field is parseable as a providerInfoPayload. This covers the gap that caused
// kairos-init (v0.14.6) to log "Failed to unmarshal provider info event: unexpected end
// of JSON input" when our binary returned an empty resp.Data string.
func TestHandleProviderInfo(t *testing.T) {
	resp := handleProviderInfo(&pluggable.Event{
		Name: eventInitProviderInfo,
		Data: "null", // kairos-init sends Publish(InitProviderInfo, nil) -> data is "null"
	})

	if resp.Error != "" {
		t.Fatalf("handleProviderInfo returned an error: %s", resp.Error)
	}

	var payload providerInfoPayload
	if err := json.Unmarshal([]byte(resp.Data), &payload); err != nil {
		t.Fatalf("resp.Data is not valid JSON parseable as providerInfoPayload: %v (data was %q)", err, resp.Data)
	}

	if payload.Provider == "" {
		t.Error("providerInfoPayload.Provider must not be empty")
	}
	if payload.Version == "" {
		t.Error("providerInfoPayload.Version must not be empty")
	}
}

// TestPluginFactoryRespondsToproProviderInfoEvent exercises the full PluginFactory
// path that kairos-init uses: run the factory with event "init.provider.info" and
// a JSON Event on stdin, then verify the stdout is a well-formed JSON EventResponse
// with non-empty Data that can be unmarshalled by kairos-init's response handler.
func TestPluginFactoryRespondsToProviderInfoEvent(t *testing.T) {
	factory := pluggable.NewPluginFactory(pluggable.FactoryPlugin{
		EventType:     eventInitProviderInfo,
		PluginHandler: handleProviderInfo,
	})

	// Build the exact stdin payload that kairos-init sends:
	// manager.Publish(InitProviderInfo, nil) -> NewEvent marshals nil -> data="null"
	event := pluggable.Event{
		Name: eventInitProviderInfo,
		Data: "null",
	}
	eventJSON, err := event.JSON()
	if err != nil {
		t.Fatalf("build event JSON: %v", err)
	}

	var out bytes.Buffer
	if err := factory.Run(eventInitProviderInfo, strings.NewReader(eventJSON), &out); err != nil {
		t.Fatalf("PluginFactory.Run returned error: %v", err)
	}

	var resp pluggable.EventResponse
	if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
		t.Fatalf("stdout is not valid JSON EventResponse: %v (raw: %q)", err, out.String())
	}

	if resp.Error != "" {
		t.Fatalf("EventResponse.Error must be empty, got: %s", resp.Error)
	}

	// kairos-init does: json.Unmarshal([]byte(resp.Data), &versionInfo)
	// An empty resp.Data causes "unexpected end of JSON input" -- verify it does not.
	var payload providerInfoPayload
	if err := json.Unmarshal([]byte(resp.Data), &payload); err != nil {
		t.Fatalf("resp.Data must be parseable as providerInfoPayload by kairos-init: %v (data: %q)", err, resp.Data)
	}

	if payload.Provider == "" {
		t.Error("providerInfoPayload.Provider must not be empty")
	}
}

// TestParseImportImagesArgs is ADR-16-A2 O-2: import-images takes no
// arguments, or exactly "--verify-only"; anything else (including the
// removed "--dir" flag) is a usage error.
func TestParseImportImagesArgs(t *testing.T) {
	cases := []struct {
		name           string
		args           []string
		wantVerifyOnly bool
		wantErr        bool
	}{
		{"no args", nil, false, false},
		{"empty slice", []string{}, false, false},
		{"--verify-only", []string{"--verify-only"}, true, false},
		{"--dir removed", []string{"--dir", "/opt/provider-kubernetes/images"}, false, true},
		{"unknown flag", []string{"--bogus"}, false, true},
		{"extra positional after --verify-only", []string{"--verify-only", "extra"}, false, true},
		{"bare positional", []string{"images"}, false, true},
		{"verify-only misspelled", []string{"-verify-only"}, false, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := parseImportImagesArgs(c.args)
			if c.wantErr {
				if err == nil {
					t.Fatalf("parseImportImagesArgs(%v) = (%v, nil), want an error", c.args, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseImportImagesArgs(%v) unexpected error: %v", c.args, err)
			}
			if got != c.wantVerifyOnly {
				t.Fatalf("parseImportImagesArgs(%v) verifyOnly = %v, want %v", c.args, got, c.wantVerifyOnly)
			}
		})
	}
}

// TestParseMigrateUnitsArgs is ADR-19 U2 / S19-8: migrate-units takes no
// arguments at all; any argument (a flag, a stray positional, anything) is a
// usage error, unlike import-images which accepts --verify-only.
func TestParseMigrateUnitsArgs(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		wantErr bool
	}{
		{"no args", nil, false},
		{"empty slice", []string{}, false},
		{"unknown flag", []string{"--verify-only"}, true},
		{"bare positional", []string{"units"}, true},
		{"extra positional", []string{"foo", "bar"}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := parseMigrateUnitsArgs(c.args)
			if c.wantErr && err == nil {
				t.Fatalf("parseMigrateUnitsArgs(%v) = nil, want an error", c.args)
			}
			if !c.wantErr && err != nil {
				t.Fatalf("parseMigrateUnitsArgs(%v) unexpected error: %v", c.args, err)
			}
		})
	}
}

// --- D-3 / F-UKIBOOT (security review 2026-09-18): S-D3-1 wrapper tests ---

// fakeProvider builds a clusterplugin.ClusterProvider that records every
// Cluster it is called with and returns a fixed yip.YipConfig.
func fakeProvider(cfg yip.YipConfig) (clusterplugin.ClusterProvider, *[]clusterplugin.Cluster) {
	var calls []clusterplugin.Cluster
	return func(c clusterplugin.Cluster) yip.YipConfig {
		calls = append(calls, c)
		return cfg
	}, &calls
}

// TestWrapProviderCallsThroughWhenSafe is S-D3-1's wrapper unit test: a safe
// (nothing-to-report) Ensure result must call through to next unchanged, and
// must pass next the exact same Cluster it was given.
func TestWrapProviderCallsThroughWhenSafe(t *testing.T) {
	want := yip.YipConfig{Name: "the-real-config"}
	next, calls := fakeProvider(want)
	ensureCalls := 0
	ensure := func(c clusterplugin.Cluster) clusterconfigdir.Report {
		ensureCalls++
		return clusterconfigdir.Report{}
	}

	cluster := clusterplugin.Cluster{Role: clusterplugin.RoleInit, ClusterToken: "tok3n-abcdefghijklmnop"}
	got := wrapProvider(next, ensure)(cluster)

	if ensureCalls != 1 {
		t.Fatalf("ensure called %d times, want 1", ensureCalls)
	}
	if len(*calls) != 1 || (*calls)[0].ClusterToken != cluster.ClusterToken {
		t.Fatalf("next was not called with the exact cluster passed in: %+v", *calls)
	}
	if got.Name != want.Name {
		t.Fatalf("got %+v, want next's config to pass through unchanged", got)
	}
}

// TestWrapProviderWithholdsWhenUnsafe is S-D3-5's required test: for an
// unsafe target, the wrapper's output must contain neither the token nor any
// Commands, and next (which would have produced the real, secret-bearing
// config) must never even be called.
//
// This is also the task's mutation (c) target: "return the real config
// instead of the inert one when unsafe -> the withhold test must fail" (see
// the mutation run recorded in the implementation report).
func TestWrapProviderWithholdsWhenUnsafe(t *testing.T) {
	const secretToken = "s3cr3t-token-must-never-appear-anywhere-abcdefgh"
	realConfig := yip.YipConfig{
		Name: "provider-kubernetes",
		Stages: map[string][]yip.Stage{
			"network.after": {{
				Name: "provider-kubernetes: write cluster state",
				Files: []yip.File{{
					Path:    "/run/provider-kubernetes/cluster.yaml",
					Content: "cluster_token: " + secretToken,
				}},
			}, {
				Name:     "provider-kubernetes: reconcile",
				Commands: []string{"/system/providers/agent-provider-kubernetes reconcile"},
			}},
		},
	}
	next, calls := fakeProvider(realConfig)
	ensure := func(c clusterplugin.Cluster) clusterconfigdir.Report {
		return clusterconfigdir.Report{Reason: clusterconfigdir.ReasonTokenFileUnsafe, Withhold: true}
	}

	cluster := clusterplugin.Cluster{Role: clusterplugin.RoleInit, ClusterToken: secretToken}
	got := wrapProvider(next, ensure)(cluster)

	if len(*calls) != 0 {
		t.Fatal("next (the real provider) must never be called when the target is unsafe")
	}
	for stageKey, stages := range got.Stages {
		for _, s := range stages {
			if len(s.Commands) != 0 {
				t.Fatalf("withheld config must carry no Commands, found some under stage %q: %v", stageKey, s.Commands)
			}
			if len(s.Files) != 0 {
				t.Fatalf("withheld config must carry no Files, found some under stage %q: %v", stageKey, s.Files)
			}
		}
	}
	serialized := got.ToString()
	if strings.Contains(serialized, secretToken) {
		t.Fatalf("withheld config's serialized form contains the token: %q", serialized)
	}
	if strings.Contains(serialized, "reconcile") {
		t.Fatalf("withheld config must carry no Commands at all: %q", serialized)
	}
}

// TestWrapProviderWithholdsOnAncestorUnsafe is the S-D3-5a follow-up's named
// test: "ancestor symlink -> withhold and the real provider is never
// called". clusterconfigdir_linux_test.go's TestEnsureSymlinkedLocalRefused
// already proves the real Ensure walk produces
// {Reason: ReasonAncestorUnsafe, Withhold: true} for a planted symlink; this
// test proves the wrapper honors that Report correctly at the main.go layer,
// exactly as it does for ReasonDirUnsafe/ReasonTokenFileUnsafe above.
func TestWrapProviderWithholdsOnAncestorUnsafe(t *testing.T) {
	const secretToken = "ancestor-s3cr3t-must-never-appear-abcdefghijk"
	realConfig := yip.YipConfig{
		Name: "provider-kubernetes",
		Stages: map[string][]yip.Stage{
			"network.after": {{
				Commands: []string{"/system/providers/agent-provider-kubernetes reconcile"},
			}},
		},
	}
	next, calls := fakeProvider(realConfig)
	ensure := func(c clusterplugin.Cluster) clusterconfigdir.Report {
		return clusterconfigdir.Report{Reason: clusterconfigdir.ReasonAncestorUnsafe, Withhold: true}
	}

	cluster := clusterplugin.Cluster{Role: clusterplugin.RoleInit, ClusterToken: secretToken}
	got := wrapProvider(next, ensure)(cluster)

	if len(*calls) != 0 {
		t.Fatal("next (the real provider) must never be called when an ancestor is unsafe")
	}
	if strings.Contains(got.ToString(), secretToken) || len(got.Stages) != 0 {
		t.Fatalf("withheld config must carry no secret and no stages: %+v", got)
	}
}

// TestWrapProviderNeverLogsTheToken is the S-D3-9 secret-hygiene test
// applied to the wrapper: greps the wrapper's own log output (logrus) and
// its returned config for the token, for every reason in the closed set,
// safe and unsafe alike.
func TestWrapProviderNeverLogsTheToken(t *testing.T) {
	const secretToken = "another-s3cr3t-abcdefghijklmnopqrstuvwx"
	reasons := []clusterconfigdir.Reason{
		clusterconfigdir.ReasonNone,
		clusterconfigdir.ReasonAncestorUnsafe,
		clusterconfigdir.ReasonAncestorMissing,
		clusterconfigdir.ReasonDirCreateFailed,
		clusterconfigdir.ReasonDirUnsafe,
		clusterconfigdir.ReasonDirWritable,
		clusterconfigdir.ReasonTokenFileUnsafe,
		clusterconfigdir.ReasonOverrideRejected,
		clusterconfigdir.ReasonNotPersistent,
	}

	var logBuf bytes.Buffer
	oldOut := logrus.StandardLogger().Out
	logrus.SetOutput(&logBuf)
	t.Cleanup(func() { logrus.SetOutput(oldOut) })

	realConfig := yip.YipConfig{
		Name: "provider-kubernetes",
		Stages: map[string][]yip.Stage{
			"network.after": {{
				Files: []yip.File{{Content: "cluster_token: " + secretToken}},
			}},
		},
	}

	for _, reason := range reasons {
		withhold := reason == clusterconfigdir.ReasonAncestorUnsafe ||
			reason == clusterconfigdir.ReasonDirUnsafe ||
			reason == clusterconfigdir.ReasonTokenFileUnsafe
		next, _ := fakeProvider(realConfig)
		ensure := func(c clusterplugin.Cluster) clusterconfigdir.Report {
			return clusterconfigdir.Report{Reason: reason, Withhold: withhold}
		}
		cluster := clusterplugin.Cluster{Role: clusterplugin.RoleInit, ClusterToken: secretToken}
		got := wrapProvider(next, ensure)(cluster)

		if !withhold {
			// The real config legitimately carries the token when not
			// withheld; only the log output is checked in that case.
			_ = got
		} else if strings.Contains(got.ToString(), secretToken) {
			t.Fatalf("reason %q: withheld config contains the token", reason)
		}
	}

	if strings.Contains(logBuf.String(), secretToken) {
		t.Fatalf("wrapper logged the token: %q", logBuf.String())
	}
}

// TestWrapProviderNeverReachesEnsureForResetOrProviderInfo documents (and
// pins) the S-D3-1 seam: only the ClusterProvider registered for
// bus.EventBoot is ever wrapped. EventClusterReset and init.provider.info are
// wired to entirely different, unwrapped handlers in main(), so they can
// never invoke clusterconfigdir.Ensure -- there is no code path for them to
// do so. This test asserts that invariant at the type level: handleProviderInfo
// and provider.HandleClusterReset are not clusterplugin.ClusterProvider values
// and therefore cannot be (and are not) passed through wrapProvider.
func TestWrapProviderNeverReachesEnsureForResetOrProviderInfo(t *testing.T) {
	// handleProviderInfo's signature, func(*pluggable.Event) pluggable.EventResponse,
	// cannot satisfy clusterplugin.ClusterProvider (func(Cluster) yip.YipConfig):
	// the compiler itself would refuse main()'s FactoryPlugin{PluginHandler:
	// handleProviderInfo} wiring if anyone tried to route it through
	// wrapProvider instead, so that invariant needs no runtime assertion here.
	// What this test DOES assert at runtime is the observable consequence:
	// invoking the init.provider.info handler creates nothing on disk.
	dir := t.TempDir()
	old, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("Chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(old) })

	handleProviderInfo(&pluggable.Event{Name: eventInitProviderInfo, Data: "null"})

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("init.provider.info handler created filesystem entries: %v", entries)
	}
}

// TestKairosSDKPinIsV0_5_0 is S-D3-10's pin guard: the wrapper's whole
// premise (the exact ENOENT-producing OpenFile call and the hardcoded
// clusterProviderCloudConfigFile path) is re-verified by hand only at
// kairos-sdk@v0.5.0. If the dependency is bumped, this test fails loudly so
// the bump is never silent, forcing a re-read of clusterplugin/plugin.go at
// the new tag before clusterconfigdir.DefaultPath is trusted again.
func TestKairosSDKPinIsV0_5_0(t *testing.T) {
	const wantVersion = "v0.5.0"
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		t.Fatal("runtime/debug.ReadBuildInfo() returned ok=false; cannot verify the kairos-sdk pin")
	}
	for _, dep := range bi.Deps {
		if dep.Path == "github.com/kairos-io/kairos-sdk" {
			if dep.Version != wantVersion {
				t.Fatalf("kairos-sdk pin moved to %s (want %s): re-verify clusterplugin/plugin.go's "+
					"clusterProviderCloudConfigFile constant, the O_CREATE|O_WRONLY|O_TRUNC open, and the lack of "+
					"MkdirAll at the new tag before updating clusterconfigdir.DefaultPath / DefaultDir / DefaultFileName",
					dep.Version, wantVersion)
			}
			return
		}
	}
	t.Fatal("github.com/kairos-io/kairos-sdk not found in build info deps")
}
