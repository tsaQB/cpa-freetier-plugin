package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	utls "github.com/refraction-networking/utls"
)

// Native in-process TLS impersonation. The OpenCode Zen free tier sits behind
// Cloudflare, which fingerprints the TLS ClientHello (JA3/JA4). Go's stdlib
// ClientHello is distinctive, so direct calls get 429 FreeUsageLimitError and
// the plugin otherwise has to relay through an external Bun sidecar
// (127.0.0.1:8789) whose TLS stack looks like a browser.
//
// This transport forges a browser ClientHello with uTLS inside the .so, so the
// plugin passes Cloudflare without any external process. Several presets are
// tried and the one that works is remembered; a later 429 rotates to the next.

// nativeTLSEnabled gates the whole feature. Default ON. Set CPA_NATIVE_TLS=0 to
// fall back to the stdlib transport + sidecar behaviour.
func nativeTLSEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("CPA_NATIVE_TLS"))) {
	case "0", "false", "off", "no":
		return false
	}
	return true
}

// tlsPreset pairs a human label with a uTLS ClientHelloID.
type tlsPreset struct {
	name string
	id   utls.ClientHelloID
}

// presetOrder is the rotation order: most-common browsers first (likeliest to
// be on Cloudflare's allow path), then mobile. CPA_NATIVE_TLS_PRESET pins one.
var presetOrder = []tlsPreset{
	{"chrome", utls.HelloChrome_Auto},
	{"firefox", utls.HelloFirefox_Auto},
	{"safari", utls.HelloSafari_Auto},
	{"ios", utls.HelloIOS_Auto},
	{"edge", utls.HelloEdge_Auto},
	{"chrome_120", utls.HelloChrome_120},
}

// activePreset is the index into presetOrder currently in use. Atomic so a 429
// on one goroutine can rotate it for the others without a lock on the hot path.
var activePreset int32

func pinnedPresetIndex() (int, bool) {
	want := strings.ToLower(strings.TrimSpace(os.Getenv("CPA_NATIVE_TLS_PRESET")))
	if want == "" {
		return 0, false
	}
	for i, p := range presetOrder {
		if p.name == want {
			return i, true
		}
	}
	return 0, false
}

func currentPreset() tlsPreset {
	if idx, ok := pinnedPresetIndex(); ok {
		return presetOrder[idx]
	}
	i := int(atomic.LoadInt32(&activePreset)) % len(presetOrder)
	return presetOrder[i]
}

// rotateTLSPreset advances to the next preset after a TLS-fingerprint rejection
// (Cloudflare 429). A pinned preset never rotates. Returns the new preset name.
func rotateTLSPreset() string {
	if _, ok := pinnedPresetIndex(); ok {
		return currentPreset().name
	}
	next := atomic.AddInt32(&activePreset, 1) % int32(len(presetOrder))
	// Drop cached connections so the next dial uses the new ClientHello.
	utlsTransportCache.Range(func(k, v any) bool {
		if tr, ok := v.(*http.Transport); ok {
			tr.CloseIdleConnections()
		}
		utlsTransportCache.Delete(k)
		return true
	})
	name := presetOrder[int(next)%len(presetOrder)].name
	logMsg("native-tls: rotated ClientHello preset to %q after fingerprint rejection", name)
	return name
}

var utlsTransportCache sync.Map // key(proxy|preset) -> *http.Transport

// newUTLSTransport builds an http.Transport whose TLS dials go through uTLS with
// the current preset's ClientHello. HTTP/1.1 only (ALPN http/1.1), matching the
// wire shape the free-tier gate already accepts.
func newUTLSTransport(proxy *url.URL) *http.Transport {
	preset := currentPreset()
	base := newTransport(proxy) // reuse pool tuning, proxy, timeouts
	base.ForceAttemptHTTP2 = false

	dialer := &net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}

	base.DialTLSContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, _, err := net.SplitHostPort(addr)
		if err != nil {
			host = addr
		}
		rawConn, err := dialer.DialContext(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		cfg := &utls.Config{
			ServerName: host,
			MinVersion: tls.VersionTLS12,
			// ALPN http/1.1 only — keep the negotiated protocol identical to the
			// stdlib path so only the TLS layer changes.
			NextProtos: []string{"http/1.1"},
		}
		uConn := utls.UClient(rawConn, cfg, preset.id)
		if err := uConn.HandshakeContext(ctx); err != nil {
			_ = rawConn.Close()
			return nil, fmt.Errorf("utls handshake (%s) to %s failed: %w", preset.name, host, err)
		}
		return uConn, nil
	}
	// DialTLSContext replaces TLSClientConfig/handshake, so clear the stdlib one.
	base.TLSClientConfig = nil
	return base
}

// getUTLSTransport returns a cached uTLS transport for the given egress + the
// current preset. Keyed on both so a preset rotation does not reuse a stale
// handshake.
func getUTLSTransport(key string, proxy *url.URL) *http.Transport {
	cacheKey := key + "|" + currentPreset().name
	if v, ok := utlsTransportCache.Load(cacheKey); ok {
		return v.(*http.Transport)
	}
	tr := newUTLSTransport(proxy)
	if actual, loaded := utlsTransportCache.LoadOrStore(cacheKey, tr); loaded {
		tr.CloseIdleConnections()
		return actual.(*http.Transport)
	}
	return tr
}
