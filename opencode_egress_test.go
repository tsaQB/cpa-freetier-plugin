package main

import (
	"net"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The sidecar used to win whenever :8789 happened to be reachable, which sent
// traffic down the slower external path even though the in-process transport
// passes the gate. These cases pin the precedence down.

func TestResolveOpenCodeBaseURLPrecedence(t *testing.T) {
	up := func() bool { return true }
	down := func() bool { return false }

	cases := []struct {
		name    string
		sidecar string
		env     string
		native  bool
		probe   func() bool
		want    string
	}{
		{"settings host wins over everything", "10.0.0.5:9000", "http://env:1/", true, up, "http://10.0.0.5:9000"},
		{"settings url kept as-is", "http://zen.local:9900/", "", true, up, "http://zen.local:9900"},
		{"settings dash forces direct even with sidecar up", "-", "", true, up, ""},
		{"settings auto probes the sidecar", "auto", "", true, up, "http://127.0.0.1:8789"},
		{"settings auto with dead port stays direct", "auto", "", true, down, ""},
		{"env base url beats native default", "", "http://env:8080/", true, up, "http://env:8080"},
		{"native default never probes", "", "", true, up, ""},
		{"native disabled falls back to sidecar probe", "", "", false, up, "http://127.0.0.1:8789"},
		{"native disabled and dead port is direct", "", "", false, down, ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveOpenCodeBaseURL(tc.sidecar, tc.env, tc.native, tc.probe)
			if got != tc.want {
				t.Fatalf("got %q want %q", got, tc.want)
			}
		})
	}
}

func TestResolveOpenCodeBaseURLIgnoresBlankEnv(t *testing.T) {
	if got := resolveOpenCodeBaseURL("", "   ", true, func() bool { return true }); got != "" {
		t.Fatalf("blank env must not become a base URL, got %q", got)
	}
}

// Quota is metered per network, so a burnt egress must be remembered instead of
// re-tried on every request. These cases pin the memory and the ordering.

func isolateEgressState(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("CPA_EGRESS_COOLING_FILE", filepath.Join(dir, "cooling.json"))
	t.Setenv("CPA_FREETIER_SETTINGS", filepath.Join(dir, "settings.json"))
	t.Setenv("OPENCODE_PROXY", "")
	t.Setenv("CPA_NATIVE_TLS", "1")
	openCodeEgress.mu.Lock()
	openCodeEgress.until = map[string]time.Time{}
	openCodeEgress.path = ""
	openCodeEgress.mu.Unlock()
}

func listenLocal(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	return ln.Addr().String()
}

func TestEgressCooldownRoundTrip(t *testing.T) {
	isolateEgressState(t)

	if !coolingUntil("http://127.0.0.1:9000").IsZero() {
		t.Fatal("fresh egress must not be cooling")
	}
	until := markEgressCooling("http://127.0.0.1:9000", 2*time.Hour)
	if until.Before(time.Now().Add(time.Hour)) {
		t.Fatalf("cooldown must honour Retry-After, got %s", until)
	}
	if coolingUntil("http://127.0.0.1:9000").IsZero() {
		t.Fatal("benched egress must report cooling")
	}

	// Survives a process restart: the map is on disk, the memory copy is not.
	openCodeEgress.mu.Lock()
	openCodeEgress.until = map[string]time.Time{}
	openCodeEgress.path = ""
	openCodeEgress.mu.Unlock()
	if coolingUntil("http://127.0.0.1:9000").IsZero() {
		t.Fatal("cooldown must be reloaded from disk")
	}

	clearEgressCooling("http://127.0.0.1:9000")
	if !coolingUntil("http://127.0.0.1:9000").IsZero() {
		t.Fatal("served egress must be healthy again")
	}
	if _, err := os.Stat(os.Getenv("CPA_EGRESS_COOLING_FILE")); err == nil {
		t.Fatal("empty cooldown table should remove the file")
	}
}

func TestEgressShortRetryFallsBackToDefault(t *testing.T) {
	isolateEgressState(t)
	until := markEgressCooling("direct", 0)
	if !until.After(time.Now().Add(defaultQuotaCooldown - time.Minute)) {
		t.Fatalf("missing Retry-After must use the default cooldown, got %s", until)
	}
}

func TestOpenCodeEgressAttemptsPrefersHealthy(t *testing.T) {
	isolateEgressState(t)
	a, b := listenLocal(t), listenLocal(t)
	t.Setenv("OPENCODE_PROXY_POOL", a+","+b)

	markEgressCooling("http://"+a, time.Hour)
	got := openCodeEgressAttempts(false)
	if len(got) != 1 || got[0].key != "http://"+b {
		t.Fatalf("healthy egress must be the only attempt, got %+v", got)
	}
}

func TestOpenCodeEgressAttemptsOnlyCooling(t *testing.T) {
	isolateEgressState(t)
	a, b := listenLocal(t), listenLocal(t)
	t.Setenv("OPENCODE_PROXY_POOL", a+","+b)

	markEgressCooling("http://"+a, 3*time.Hour)
	markEgressCooling("http://"+b, time.Hour)
	got := openCodeEgressAttempts(false)
	if len(got) < 2 {
		t.Fatalf("all-cooling pool must still be attempted, got %+v", got)
	}
	if got[0].key != "http://"+b {
		t.Fatalf("soonest recovery must come first, got %+v", got)
	}
}

func TestOpenCodeEgressAttemptsExplicitProxyIsSingle(t *testing.T) {
	isolateEgressState(t)
	a := listenLocal(t)
	t.Setenv("OPENCODE_PROXY", "http://"+a)

	got := openCodeEgressAttempts(false)
	if len(got) != 1 {
		t.Fatalf("explicit proxy must not fan out, got %+v", got)
	}
	if got[0].key != "http://"+a {
		t.Fatalf("explicit proxy must be the attempt, got %+v", got)
	}
}

func TestOpenCodeEgressAttemptsLocalBaseURL(t *testing.T) {
	isolateEgressState(t)
	got := openCodeEgressAttempts(true)
	if len(got) != 1 || got[0].key != "local" || got[0].proxy != nil {
		t.Fatalf("local base URL must be a single direct attempt, got %+v", got)
	}
}

func TestEgressKeyStable(t *testing.T) {
	u, err := url.Parse("http://user:pass@10.0.0.1:8080")
	if err != nil {
		t.Fatal(err)
	}
	if egressKey(u) != u.String() {
		t.Fatal("key must be the proxy URL")
	}
	if egressKey(nil) != directEgressKey {
		t.Fatal("nil proxy must key as direct")
	}
}
