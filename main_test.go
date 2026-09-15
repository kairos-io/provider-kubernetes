package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/mudler/go-pluggable"
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
