package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// This file centralises the OmniRush client fingerprint and the session
// lifecycle the gateway uses to tell a native CLI/desktop client apart from a
// bare headless caller.
//
// Why this exists: OmniRush closes accounts whose sessions "never uploaded",
// i.e. traffic that does not look like it came from the official CLI/desktop
// app. The real fix (uploading session traces + workspace snapshots) needs the
// upload endpoints reverse-engineered from a live CLI capture. Until then this
// layer does the part that needs no capture: keep the advertised client version
// current, stamp a stable install id and a TTL-scoped session id on every
// gateway call, and keep all of it override-able so the capture phase only has
// to drop in exact values — never rewrite call sites.

const (
	// omniRushCLIVersionFallback tracks the latest published `omnirush` npm
	// release. A stale version is exactly the "lookalike client" signal that
	// gets flagged, so bump this with each CLI release (or set
	// OMNIRUSH_CLIENT_VERSION / settings.identity.clientVersion to override
	// without a rebuild).
	omniRushCLIVersionFallback = "2.7.8"
	// omniRushNodeVersionFallback is the Node runtime the CLI requires
	// (docs: "Node.js 22.19 or newer"). Only used if the header set includes a
	// runtime hint; override with OMNIRUSH_NODE_VERSION.
	omniRushNodeVersionFallback = "22.19.0"

	// omniRushSessionTTL scopes one logical session. The gateway treats calls
	// sharing a session id as one conversation; regenerating on idle mirrors a
	// user who closed the CLI and started again.
	omniRushSessionTTL = 30 * time.Minute

	// omniRushInstallIDFile persists the per-install device id next to the
	// credentials, so a given host keeps the same id across restarts — a real
	// install does not mint a new device id on every process start.
	omniRushInstallIDFile = ".cpa-omnirush-install-id"
)

// nativeHeadersEnabled gates the extra client/session/install headers. Default
// on; set CPA_OMNIRUSH_NATIVE_HEADERS=0 to fall back to the bare two-header
// shape (useful to A/B against the gateway if a header starts getting refused).
func nativeHeadersEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("CPA_OMNIRUSH_NATIVE_HEADERS"))) {
	case "0", "false", "off", "no":
		return false
	}
	return true
}

// omniRushClientVersion resolves the advertised CLI version: env override, then
// the settings identity, then the compiled fallback.
func omniRushClientVersion() string {
	if v := strings.TrimSpace(os.Getenv("OMNIRUSH_CLIENT_VERSION")); v != "" {
		return normalizeClientVersion(v)
	}
	if v := strings.TrimSpace(loadPluginSettings().Identity.ClientVersion); v != "" {
		return normalizeClientVersion(v)
	}
	return omniRushCLIVersionFallback
}

// normalizeClientVersion strips a leading "cli/" or "v" so both "cli/2.7.8",
// "v2.7.8" and "2.7.8" resolve to the same bare version string.
func normalizeClientVersion(v string) string {
	v = strings.TrimSpace(v)
	v = strings.TrimPrefix(v, "cli/")
	v = strings.TrimPrefix(v, "v")
	return strings.TrimSpace(v)
}

// omniRushClientHeader is the value for X-OmniRush-Client, e.g. "cli/2.7.8".
// The product name stays in User-Agent (a versioned UA is refused as a
// lookalike); the version travels here.
func omniRushClientHeader() string {
	return "cli/" + omniRushClientVersion()
}

func omniRushNodeVersion() string {
	if v := strings.TrimSpace(os.Getenv("OMNIRUSH_NODE_VERSION")); v != "" {
		return strings.TrimPrefix(strings.TrimSpace(v), "v")
	}
	return omniRushNodeVersionFallback
}

// omniRushPlatform / omniRushArch map Go's build identifiers onto the names the
// Node CLI reports (process.platform / process.arch), so the hints match a real
// install on this host rather than Go's own GOOS/GOARCH spelling.
func omniRushPlatform() string {
	switch runtime.GOOS {
	case "darwin":
		return "darwin"
	case "windows":
		return "win32"
	default:
		return "linux"
	}
}

func omniRushArch() string {
	switch runtime.GOARCH {
	case "amd64":
		return "x64"
	case "arm64":
		return "arm64"
	case "386":
		return "ia32"
	default:
		return runtime.GOARCH
	}
}

// ── Install id (stable per host) ────────────────────────────────────────────

var (
	installIDOnce  sync.Once
	installIDValue string
)

func omniRushInstallID() string {
	installIDOnce.Do(func() {
		installIDValue = loadOrCreateInstallID()
	})
	return installIDValue
}

func installIDPath() string {
	if d := strings.TrimSpace(os.Getenv("OMNIRUSH_DIR")); d != "" {
		return filepath.Join(d, omniRushInstallIDFile)
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".omnirush", omniRushInstallIDFile)
	}
	return omniRushInstallIDFile
}

func loadOrCreateInstallID() string {
	if v := strings.TrimSpace(os.Getenv("OMNIRUSH_INSTALL_ID")); v != "" {
		return v
	}
	path := installIDPath()
	if data, err := os.ReadFile(path); err == nil {
		if id := strings.TrimSpace(string(data)); id != "" {
			return id
		}
	}
	id := randomUUIDv4()
	// Best effort: a read-only home just means the id is per-process instead of
	// persisted. Never block a request on this.
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err == nil {
		_ = os.WriteFile(path, []byte(id+"\n"), 0o600)
	}
	return id
}

func randomUUIDv4() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%016x%016x", time.Now().UnixNano(), time.Now().UnixNano())
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	h := hex.EncodeToString(b)
	return fmt.Sprintf("%s-%s-%s-%s-%s", h[0:8], h[8:12], h[12:16], h[16:20], h[20:32])
}

// ── Session lifecycle ───────────────────────────────────────────────────────

var (
	omniSessionMu       sync.Mutex
	omniSessionID       string
	omniSessionLastUsed time.Time
)

// omniRushSessionID returns the current logical session id, reusing it within
// the TTL and minting a fresh one after idle. Reuses the CLI's id shape
// (ses_<12-hex><14-base62>, descending-timestamp) so it is indistinguishable
// from an opencode session id on the wire. A settings/env override pins a fixed
// id when capture shows the real CLI reuses one.
func omniRushSessionID() string {
	if v := strings.TrimSpace(os.Getenv("OMNIRUSH_SESSION_ID")); v != "" {
		return v
	}
	if v := strings.TrimSpace(loadPluginSettings().Identity.SessionID); v != "" {
		return v
	}

	omniSessionMu.Lock()
	defer omniSessionMu.Unlock()
	now := time.Now()
	if omniSessionID == "" || now.Sub(omniSessionLastUsed) >= omniRushSessionTTL {
		omniSessionID = genSessionID()
	}
	omniSessionLastUsed = now
	return omniSessionID
}

// resetOmniRushSession forces the next call to mint a new session id. Signing
// in/out or disconnecting an account should not keep leaking the previous
// session handle.
func resetOmniRushSession() {
	omniSessionMu.Lock()
	omniSessionID = ""
	omniSessionLastUsed = time.Time{}
	omniSessionMu.Unlock()
}
