package main

import (
	"os"
	"strings"
	"testing"
)

func resetTLSPresetForTest() {
	activePreset = 0
}

func TestNativeTLSEnabledDefaultOn(t *testing.T) {
	t.Setenv("CPA_NATIVE_TLS", "")
	if !nativeTLSEnabled() {
		t.Fatal("native TLS should default on")
	}
	for _, off := range []string{"0", "false", "off", "no", "OFF"} {
		t.Setenv("CPA_NATIVE_TLS", off)
		if nativeTLSEnabled() {
			t.Fatalf("native TLS should be off for %q", off)
		}
	}
}

func TestPresetRotationCyclesAndWraps(t *testing.T) {
	t.Setenv("CPA_NATIVE_TLS_PRESET", "")
	resetTLSPresetForTest()

	first := currentPreset().name
	if first != presetOrder[0].name {
		t.Fatalf("first preset = %q, want %q", first, presetOrder[0].name)
	}

	seen := map[string]bool{first: true}
	for i := 0; i < len(presetOrder)-1; i++ {
		rotateTLSPreset()
		seen[currentPreset().name] = true
	}
	if len(seen) != len(presetOrder) {
		t.Fatalf("rotation visited %d presets, want %d", len(seen), len(presetOrder))
	}

	// One more rotation wraps back to the first preset.
	rotateTLSPreset()
	if currentPreset().name != presetOrder[0].name {
		t.Fatalf("rotation did not wrap: got %q", currentPreset().name)
	}
}

func TestPinnedPresetDoesNotRotate(t *testing.T) {
	t.Setenv("CPA_NATIVE_TLS_PRESET", "firefox")
	resetTLSPresetForTest()

	if currentPreset().name != "firefox" {
		t.Fatalf("pinned preset = %q, want firefox", currentPreset().name)
	}
	if got := rotateTLSPreset(); got != "firefox" {
		t.Fatalf("pinned preset rotated to %q, want firefox", got)
	}
	if currentPreset().name != "firefox" {
		t.Fatal("pinned preset must stay fixed after rotate")
	}
}

func TestUnknownPinnedPresetFallsThrough(t *testing.T) {
	t.Setenv("CPA_NATIVE_TLS_PRESET", "nonsense-browser")
	resetTLSPresetForTest()
	// Unknown pin is ignored: resolver falls back to the rotating default.
	if _, ok := pinnedPresetIndex(); ok {
		t.Fatal("unknown preset must not be treated as pinned")
	}
	if currentPreset().name != presetOrder[0].name {
		t.Fatalf("unknown pin should use default preset, got %q", currentPreset().name)
	}
}

func TestUTLSTransportBuildsWithDialTLS(t *testing.T) {
	t.Setenv("CPA_NATIVE_TLS_PRESET", "chrome")
	resetTLSPresetForTest()
	tr := getUTLSTransport("env", nil)
	if tr == nil {
		t.Fatal("nil transport")
	}
	if tr.DialTLSContext == nil {
		t.Fatal("DialTLSContext must be set so uTLS drives the handshake")
	}
	if tr.TLSClientConfig != nil {
		t.Fatal("stdlib TLSClientConfig must be cleared when DialTLSContext is used")
	}
	if tr.ForceAttemptHTTP2 {
		t.Fatal("must stay on HTTP/1.1")
	}
	// Cached: same key + preset returns the same instance.
	if tr2 := getUTLSTransport("env", nil); tr2 != tr {
		t.Fatal("transport not cached per key+preset")
	}
}

// Guard: the preset names used in docs/env must match what the code offers.
func TestPresetNamesAreLowercaseUnique(t *testing.T) {
	seen := map[string]bool{}
	for _, p := range presetOrder {
		if p.name != strings.ToLower(p.name) {
			t.Fatalf("preset name %q must be lowercase", p.name)
		}
		if seen[p.name] {
			t.Fatalf("duplicate preset name %q", p.name)
		}
		seen[p.name] = true
	}
	_ = os.Getenv // keep os import if trimmed later
}
