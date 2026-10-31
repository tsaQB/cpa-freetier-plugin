package main

import (
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
)

func TestApplyOmniRushHeadersNativeFingerprint(t *testing.T) {
	t.Setenv("CPA_OMNIRUSH_NATIVE_HEADERS", "1")
	t.Setenv("OMNIRUSH_CLIENT_VERSION", "")
	t.Setenv("OMNIRUSH_DIR", t.TempDir())

	req, _ := http.NewRequest(http.MethodPost, "https://omnirush.ai/omnirush/v1/responses", nil)
	applyOmniRushHeaders(req, "tok123")

	if got := req.Header.Get("Authorization"); got != "Bearer tok123" {
		t.Fatalf("Authorization = %q", got)
	}
	if got := req.Header.Get("User-Agent"); got != "omnirush" {
		t.Fatalf("User-Agent must stay the bare product name, got %q", got)
	}
	if got := req.Header.Get("X-OmniRush-Client"); got != "cli/"+omniRushCLIVersionFallback {
		t.Fatalf("X-OmniRush-Client = %q, want cli/%s", got, omniRushCLIVersionFallback)
	}
	for _, h := range []string{
		"X-OmniRush-Install-Id", "X-OmniRush-Session-Id",
		"X-OmniRush-Client-Version", "X-OmniRush-Node",
		"X-OmniRush-Platform", "X-OmniRush-Arch",
	} {
		if strings.TrimSpace(req.Header.Get(h)) == "" {
			t.Fatalf("native header %s was not set", h)
		}
	}
	if sid := req.Header.Get("X-OmniRush-Session-Id"); !strings.HasPrefix(sid, "ses_") {
		t.Fatalf("session id %q does not use the ses_ shape", sid)
	}
}

func TestApplyOmniRushHeadersCanDisableNative(t *testing.T) {
	t.Setenv("CPA_OMNIRUSH_NATIVE_HEADERS", "0")
	req, _ := http.NewRequest(http.MethodPost, "https://omnirush.ai/", nil)
	applyOmniRushHeaders(req, "tok")
	if req.Header.Get("X-OmniRush-Install-Id") != "" {
		t.Fatal("native headers must be absent when disabled")
	}
	if req.Header.Get("X-OmniRush-Client") == "" {
		t.Fatal("base client header must still be present when native is off")
	}
}

func TestClientVersionOverride(t *testing.T) {
	t.Setenv("OMNIRUSH_CLIENT_VERSION", "cli/9.9.9")
	resetSettingsCache()
	if got := omniRushClientHeader(); got != "cli/9.9.9" {
		t.Fatalf("override not honoured: %q", got)
	}
	t.Setenv("OMNIRUSH_CLIENT_VERSION", "v3.2.1")
	if got := omniRushClientVersion(); got != "3.2.1" {
		t.Fatalf("normalize failed: %q", got)
	}
}

func TestInstallIDStableAndPersisted(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("OMNIRUSH_DIR", dir)
	t.Setenv("OMNIRUSH_INSTALL_ID", "")
	installIDOnce = sync.Once{}
	installIDValue = ""
	id1 := omniRushInstallID()
	if id1 == "" {
		t.Fatal("install id empty")
	}
	if _, err := os.Stat(dir + "/" + omniRushInstallIDFile); err != nil {
		t.Fatalf("install id not persisted: %v", err)
	}
	// Second call within the process is cached and identical.
	if id2 := omniRushInstallID(); id2 != id1 {
		t.Fatalf("install id not stable: %q vs %q", id1, id2)
	}
}
