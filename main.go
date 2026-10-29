//go:build cgo

package main

/*
#include <stdint.h>
#include <stdlib.h>

typedef struct {
	void* ptr;
	size_t len;
} cliproxy_buffer;

typedef int (*cliproxy_host_call_fn)(void*, const char*, const uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_host_free_fn)(void*, size_t);

typedef struct {
	uint32_t abi_version;
	void* host_ctx;
	cliproxy_host_call_fn call;
	cliproxy_host_free_fn free_buffer;
} cliproxy_host_api;

typedef int (*cliproxy_plugin_call_fn)(char*, uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_plugin_free_fn)(void*, size_t);
typedef void (*cliproxy_plugin_shutdown_fn)(void);

typedef struct {
	uint32_t abi_version;
	cliproxy_plugin_call_fn call;
	cliproxy_plugin_free_fn free_buffer;
	cliproxy_plugin_shutdown_fn shutdown;
} cliproxy_plugin_api;

extern int cliproxyPluginCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void cliproxyPluginFree(void*, size_t);
extern void cliproxyPluginShutdown(void);

static const cliproxy_host_api* stored_host;

static void store_host_api(const cliproxy_host_api* host) {
	stored_host = host;
}

static int call_host_api(const char* method, const uint8_t* request, size_t request_len, cliproxy_buffer* response) {
	if (stored_host == NULL || stored_host->call == NULL) {
		return 1;
	}
	return stored_host->call(stored_host->host_ctx, method, request, request_len, response);
}

static void free_host_buffer(void* ptr, size_t len) {
	if (stored_host != NULL && stored_host->free_buffer != NULL && ptr != NULL) {
		stored_host->free_buffer(ptr, len);
	}
}
*/
import "C"

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unsafe"
)

const (
	abiVersion = 1

	// maxSSELineBytes bounds one SSE "data:" line. Agent turns routinely carry
	// large tool-call payloads, and a bufio.Scanner that overflows its buffer
	// aborts the whole stream with ErrTooLong.
	maxSSELineBytes = 16 << 20

	pluginID = "cpa-freetier-plugin"

	// OpenCode v2 Endpoints
	opencodeChatURL      = "https://opencode.ai/inference/openai/v1/chat/completions"
	opencodeResponsesURL = "https://opencode.ai/inference/openai/v1/responses"
	opencodeClientUA     = "opencode/latest/2.1.4/cli"

	// Kilo Endpoints
	kiloChatURL = "https://api.kilo.ai/api/gateway/chat/completions"
)

// pluginVersion is stamped at build time:
// -ldflags "-X main.pluginVersion=$(VERSION)" — one source of truth instead of
// hand-editing this file, plugin.json and the README on every release.
var pluginVersion = "1.5.3"

// ── Logging ─────────────────────────────────────────────────────────────────
//
// Two hard rules learned in production:
//  1. Request bodies are NEVER written to the log. A payload can carry the whole
//     conversation, so only its length and a short fingerprint are recorded.
//  2. The file is bounded. The previous implementation appended to a fixed path
//     with no cap; on a 512 MiB tmpfs it reached 393 MiB in ~90 minutes.
//
// Destination: CPA_FREETIER_LOG, else <home>/.cli-proxy-api/logs/, else /tmp.
// Verbose request tracing: CPA_FREETIER_DEBUG=1.

const (
	maxLogBytes = 8 << 20 // rotate at 8 MiB, one "<file>.1" generation kept
	logFileName = "cpa-freetier-plugin.log"
)

// legacyLogPath is the last resort when no home directory is resolvable (or the
// data dir cannot be created). os.TempDir() keeps this portable: /tmp on Linux,
// TMPDIR elsewhere, and the same file the pre-1.2.6 build used.
func legacyLogPath() string { return filepath.Join(os.TempDir(), "cpa_plugin.log") }

var (
	logMu       sync.Mutex
	logHandle   *os.File
	logPathUsed string
	logSize     int64
	logDisabled bool
)

func debugEnabled() bool {
	if loadPluginSettings().Logging.Debug {
		return true
	}
	switch strings.ToLower(strings.TrimSpace(os.Getenv("CPA_FREETIER_DEBUG"))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// logDebug is for diagnostics that are only useful while troubleshooting.
func logDebug(format string, args ...any) {
	if debugEnabled() {
		logMsg(format, args...)
	}
}

func logFilePath() string {
	if p := strings.TrimSpace(os.Getenv("CPA_FREETIER_LOG")); p != "" {
		return p
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".cli-proxy-api", "logs", logFileName)
	}
	return legacyLogPath()
}

// openLogLocked must be called with logMu held.
func openLogLocked(path string) {
	if logHandle != nil || logDisabled {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		path = legacyLogPath()
	}
	// 0600: log lines carry model names, egress and timings — not secrets, but
	// nothing a co-tenant on a shared host needs to read.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		logDisabled = true // never fail a request because logging is impossible
		return
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		logDisabled = true
		return
	}
	logHandle = f
	logPathUsed = path
	logSize = info.Size()
}

// rotateLogLocked must be called with logMu held.
func rotateLogLocked() {
	if logHandle == nil {
		return
	}
	path := logPathUsed
	_ = logHandle.Close()
	logHandle = nil
	rotated := path + ".1"
	_ = os.Remove(rotated)
	if err := os.Rename(path, rotated); err != nil {
		_ = os.Remove(path)
	}
	logSize = 0
	openLogLocked(path)
}

func logMsg(format string, args ...any) {
	line := fmt.Sprintf("[%s] "+format+"\n", append([]any{time.Now().Format("15:04:05.000")}, args...)...)
	logMu.Lock()
	defer logMu.Unlock()
	openLogLocked(logFilePath())
	if logHandle == nil {
		return
	}
	if logSize+int64(len(line)) > maxLogBytes {
		rotateLogLocked()
		if logHandle == nil {
			return
		}
	}
	n, _ := logHandle.WriteString(line)
	logSize += int64(n)
}

// payloadFingerprint identifies a payload for correlation without exposing it.
func payloadFingerprint(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}

// ── Egress selection ────────────────────────────────────────────────────────
//
// The built-in pools are lab defaults and all point at loopback addresses. On a
// laptop or a plain VPS nothing answers there, the probe fails and the plugin
// connects directly through the ambient network — no configuration required.
//
// Override with OPENCODE_PROXY_POOL / OPENCODE_DATACENTER_PROXY_POOL
// (comma-separated host:port or full proxy URLs); an explicitly empty value
// disables probing for that class.
//
// Datacenter egress is opt-in: hosts on datacenter ASNs are answered by the
// OpenCode free tier with 403 FreeTierError, so probing them by default would
// only ever produce failures.
var (
	// Defaults are intentionally EMPTY: the plugin ships to arbitrary hosts,
	// so hardcoded tunnel ports (a host-specific convention) would only make
	// every install probe phantom ports and fall back to direct anyway.
	// Configure egress explicitly via OPENCODE_PROXY_POOL /
	// OPENCODE_DATACENTER_PROXY_POOL or the dashboard settings panel.
	defaultOpenCodeProxies           = []string{}
	defaultOpenCodeDatacenterProxies = []string{}

	proxyCacheMu   sync.Mutex
	proxyCacheURL  *url.URL
	proxyCacheAt   time.Time
	proxyCacheInit bool
)

// proxyProbeTTL keeps the reachability probe off the hot path: previously every
// single request opened a TCP connection just to test the proxy.
const proxyProbeTTL = 30 * time.Second

func dialable(addr string) bool {
	conn, err := net.DialTimeout("tcp", addr, 60*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

func datacenterAllowed() bool {
	if loadPluginSettings().Proxy.AllowDatacenter {
		return true
	}
	switch strings.ToLower(strings.TrimSpace(os.Getenv("OPENCODE_ALLOW_DATACENTER"))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// validProxyTarget rejects entries that parse as a URL but carry no usable
// host:port (for example `:::` which parses into an IPv6-ish `http://:::`).
func validProxyTarget(u *url.URL) bool {
	if u == nil || u.Host == "" {
		return false
	}
	// Bracketed IPv6 literal, e.g. [::1]:8080 — the parser already checked shape.
	if strings.HasPrefix(u.Host, "[") {
		return strings.Contains(u.Host, "]")
	}
	if !strings.Contains(u.Host, ":") {
		return true // bare hostname, default port
	}
	host, port, err := net.SplitHostPort(u.Host)
	if err != nil || host == "" {
		return false
	}
	n, err := strconv.Atoi(port)
	return err == nil && n >= 1 && n <= 65535
}

// proxyPool resolves an ordered candidate list: environment first (comma
// separated, host:port or full URL), otherwise the compiled-in default. An
// explicitly empty value means "probe nothing", which is how a user opts out.
func proxyPool(envName string, defaults []string) []*url.URL {
	// A dashboard-saved pool wins over the environment so the panel is the
	// single place to change egress without touching service config.
	cfg := loadPluginSettings()
	settingsValue := ""
	switch envName {
	case "OPENCODE_PROXY_POOL":
		settingsValue = cfg.Proxy.OpenCodePool
	case "OPENCODE_DATACENTER_PROXY_POOL":
		settingsValue = cfg.Proxy.DatacenterPool
	}
	if settingsValue == proxyDisabledSentinel {
		return nil
	}

	raw, configured := os.LookupEnv(envName)
	if strings.TrimSpace(settingsValue) != "" {
		raw, configured = settingsValue, true
	}
	if !configured {
		raw = strings.Join(defaults, ",")
	}

	var out []*url.URL
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if !strings.Contains(part, "://") {
			part = "http://" + part
		}
		u, err := url.Parse(part)
		if err != nil || !validProxyTarget(u) {
			continue
		}
		out = append(out, u)
	}
	return out
}

// proxyReachable reports whether a proxy's listener answers. It only inspects
// local reachability; whether the egress IP is accepted upstream is a separate
// question the free-tier gate decides.
func proxyReachable(u *url.URL) bool {
	if !validProxyTarget(u) {
		return false
	}
	return dialable(u.Host)
}

// getOpenCodeProxy returns the egress for OpenCode, or nil for the ambient
// HTTP(S)_PROXY / direct connection.
func getOpenCodeProxy() *url.URL {
	// Dashboard setting, then environment, then the probe order below.
	if u, handled := settingsOpenCodeProxy(); handled {
		return u
	}
	if p := strings.TrimSpace(os.Getenv("OPENCODE_PROXY")); p != "" {
		if u, err := url.Parse(p); err == nil {
			return u
		}
	}

	proxyCacheMu.Lock()
	defer proxyCacheMu.Unlock()
	if proxyCacheInit && time.Since(proxyCacheAt) < proxyProbeTTL {
		return proxyCacheURL
	}

	var chosen *url.URL
	for _, u := range proxyPool("OPENCODE_PROXY_POOL", defaultOpenCodeProxies) {
		if proxyReachable(u) {
			chosen = u
			break
		}
	}
	if chosen == nil && datacenterAllowed() {
		for _, u := range proxyPool("OPENCODE_DATACENTER_PROXY_POOL", defaultOpenCodeDatacenterProxies) {
			if proxyReachable(u) {
				chosen = u
				break
			}
		}
	}

	proxyCacheURL = chosen
	proxyCacheAt = time.Now()
	proxyCacheInit = true
	return chosen
}

// invalidateOpenCodeProxy forces a fresh probe after a transport-level failure.
func invalidateOpenCodeProxy() {
	proxyCacheMu.Lock()
	proxyCacheInit = false
	proxyCacheURL = nil
	proxyCacheMu.Unlock()
}

// ── HTTP transports ─────────────────────────────────────────────────────────
//
// Transports are process-wide and cached per egress. Rebuilding one per request
// (the previous behaviour) threw away keep-alive: every call paid a fresh TCP +
// TLS handshake and the pool tuning below never took effect.
var transportCache sync.Map // key -> *http.Transport

func newTransport(proxy *url.URL) *http.Transport {
	tr := &http.Transport{
		MaxIdleConns:          128,
		MaxIdleConnsPerHost:   32,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: settingsHeaderTimeout(), // bodies stream, headers must not hang
		ExpectContinueTimeout: 1 * time.Second,
		// Stay on HTTP/1.1 through the CONNECT proxy: that is the wire shape the
		// OpenCode free-tier gate currently accepts.
		ForceAttemptHTTP2: false,
	}
	if proxy != nil {
		tr.Proxy = http.ProxyURL(proxy)
	} else {
		tr.Proxy = http.ProxyFromEnvironment
	}
	return tr
}

func getTransport(key string, proxy *url.URL) *http.Transport {
	if v, ok := transportCache.Load(key); ok {
		return v.(*http.Transport)
	}
	tr := newTransport(proxy)
	if actual, loaded := transportCache.LoadOrStore(key, tr); loaded {
		tr.CloseIdleConnections()
		return actual.(*http.Transport)
	}
	return tr
}

func invalidateTransport(key string) {
	if v, ok := transportCache.Load(key); ok {
		v.(*http.Transport).CloseIdleConnections()
	}
	transportCache.Delete(key)
}

// getTransportWithTimeout is getTransport with a per-key header timeout, used by
// OmniRush where the free-tier queue must not look like an infinite hang.
func getTransportWithTimeout(key string, proxy *url.URL, headerTimeout time.Duration) *http.Transport {
	if v, ok := transportCache.Load(key); ok {
		return v.(*http.Transport)
	}
	tr := newTransport(proxy)
	if headerTimeout > 0 {
		tr.ResponseHeaderTimeout = headerTimeout
	}
	if actual, loaded := transportCache.LoadOrStore(key, tr); loaded {
		tr.CloseIdleConnections()
		return actual.(*http.Transport)
	}
	return tr
}

func getHTTPClient(forOpenCode bool) *http.Client {
	if !forOpenCode {
		return &http.Client{Transport: getTransport("env", nil)}
	}
	proxy := getOpenCodeProxy()
	key := "env"
	if proxy != nil {
		key = proxy.String()
	}
	// No client-wide Timeout: it would abort long SSE responses. ResponseHeaderTimeout
	// on the transport is what protects against a hung upstream instead.
	return &http.Client{Transport: getTransport(key, proxy)}
}

var openCodeModels = map[string]bool{
	"flint-40b-free":              true,
	"nemotron-3-ultra-free":       true,
	"glm-5.3-flash-free":          true,
	"big-pickle":                  true,
	"deepseek-v4.1-flash-free":    true,
	"cat-jam-free":                true,
	"mimo-v2.6-flash-free":        true,
	"beam-free":                   true,
	"nemotron-3.5-super-vl-free":  true,
	"nemotron-3.5-lightning-free": true,
}

var kiloModels = map[string]bool{
	"kilo-auto/free":                                     true,
	"nvidia/nemotron-3-ultra-550b-a55b:free":             true,
	"cohere/north-mini-code:free":                        true,
	"openrouter/free":                                    true,
	"nvidia/nemotron-3-nano-omni-30b-a3b-reasoning:free": true,
	"nvidia/nemotron-3.5-lightning:free":                 true,
	"nvidia/nemotron-3.5-super-vl:free":                  true,
	"poolside/laguna-s-2.1:free":                         true,
	"poolside/laguna-xs-2.1:free":                        true,
	"dots-studio/dots-3-note-preview:free":               true,
	"liquid/lfm-2.5-2.6b:free":                           true,
	"reflection/beam-free":                               true,
	"xingchen-agi/xing4.0-29b-a4b:free":                  true,
	"nvidia/nemotron-3.5-content-safety:free":            true,
}

type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *pluginError    `json:"error,omitempty"`
}

type pluginError struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	HTTPStatus int    `json:"http_status,omitempty"`
}

type executorRequest struct {
	Model           string `json:"Model"`
	OriginalRequest []byte `json:"OriginalRequest"`
	Payload         []byte `json:"Payload"`
	StreamID        string `json:"stream_id,omitempty"`
}

type streamEmit struct {
	StreamID string `json:"stream_id"`
	Payload  []byte `json:"payload,omitempty"`
	Error    string `json:"error,omitempty"`
}

type streamClose struct {
	StreamID string `json:"stream_id"`
	Error    string `json:"error,omitempty"`
}

type upstreamResponse struct {
	status int
	header http.Header
	body   io.ReadCloser
}

func main() {}

func ensureAuthDescriptor() {
	// Writing files is a side effect the operator must be able to switch off.
	switch strings.ToLower(strings.TrimSpace(os.Getenv("CPA_NO_AUTH_DESCRIPTOR"))) {
	case "1", "true", "yes", "on":
		return
	}
	const authFileName = "cpa-freetier-plugin.json"
	authContent := []byte(`{"disabled":false,"provider":"cpa-freetier-plugin","type":"cpa-freetier-plugin"}` + "\n")

	// 1. Target standard: ~/.cli-proxy-api/ (Linux, macOS, Windows)
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		primaryDir := filepath.Join(home, ".cli-proxy-api")
		_ = os.MkdirAll(primaryDir, 0755)
		targetPath := filepath.Join(primaryDir, authFileName)
		if _, err := os.Stat(targetPath); os.IsNotExist(err) {
			if err := os.WriteFile(targetPath, authContent, 0644); err == nil {
				logMsg("auto-created auth descriptor: %s", targetPath)
			} else {
				logMsg("failed to create auth descriptor %s: %v", targetPath, err)
			}
		}
	}

	// Only directories the operator named explicitly. Creating the descriptor in
	// the process working directory by default was a surprise side effect.
	candidates := []string{
		os.Getenv("CPA_AUTH_DIR"),
		os.Getenv("AUTH_DIR"),
	}
	for _, dir := range candidates {
		if dir == "" {
			continue
		}
		if info, err := os.Stat(dir); err == nil && info.IsDir() {
			targetPath := filepath.Join(dir, authFileName)
			if _, err := os.Stat(targetPath); os.IsNotExist(err) {
				if err := os.WriteFile(targetPath, authContent, 0644); err == nil {
					logMsg("auto-created auth descriptor in custom dir: %s", targetPath)
				}
			}
		}
	}
}

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) C.int {
	if plugin == nil {
		return 1
	}
	C.store_host_api(host)
	plugin.abi_version = C.uint32_t(abiVersion)
	plugin.call = C.cliproxy_plugin_call_fn(C.cliproxyPluginCall)
	plugin.free_buffer = C.cliproxy_plugin_free_fn(C.cliproxyPluginFree)
	plugin.shutdown = C.cliproxy_plugin_shutdown_fn(C.cliproxyPluginShutdown)
	ensureAuthDescriptor()
	logMsg("plugin initialized")
	return 0
}

//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) C.int {
	if response != nil {
		response.ptr = nil
		response.len = 0
	}
	if method == nil {
		writeResponse(response, errorEnvelope("invalid_method", "method is required", 0))
		return 1
	}
	var raw []byte
	if request != nil && requestLen > 0 {
		// requestLen is size_t and C.GoBytes takes a C.int, so the old form would
		// truncate any payload above 2 GiB. Copy through an explicit slice instead.
		raw = make([]byte, int(requestLen))
		copy(raw, unsafe.Slice((*byte)(unsafe.Pointer(request)), int(requestLen)))
	}
	methodStr := C.GoString(method)
	out, err := handleMethod(methodStr, raw)
	if err != nil {
		logMsg("handleMethod error for %s: %v", methodStr, err)
		writeResponse(response, errorEnvelope("plugin_error", err.Error(), 0))
		return 1
	}
	writeResponse(response, out)
	return 0
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, _ C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {
	logMsg("plugin shutdown")
}

func handleMethod(method string, raw []byte) ([]byte, error) {
	switch method {
	case "plugin.register", "plugin.reconfigure":
		ensureAuthDescriptor()
		return ok(map[string]any{
			"schema_version": 6,
			"metadata": map[string]any{
				"Name":             pluginID,
				"Version":          pluginVersion,
				"Author":           "tsaQB",
				"GitHubRepository": "https://github.com/tsaQB/cpa-freetier-plugin",
				"ConfigFields":     []any{},
			},
			"capabilities": map[string]any{
				"model_provider":          true,
				"executor":                true,
				"executor_model_scope":    "static",
				"executor_input_formats":  []string{"chat-completions"},
				"executor_output_formats": []string{"chat-completions"},
				"request_normalizer":      true,
				"management_api":          true,
			},
		})
	case "management.register":
		return handleManagementRegister(raw)
	case "management.handle":
		return handleManagementHandle(raw)
	case "request.normalize":
		return normalizeRequest(raw)
	case "model.static", "model.for_auth":
		return ok(map[string]any{
			"Provider": pluginID,
			"Models":   listModels(),
		})
	case "executor.identifier":
		return ok(map[string]string{
			"identifier": pluginID,
		})
	case "executor.execute":
		return execute(raw)
	case "executor.execute_stream":
		return executeStream(raw)
	case "executor.count_tokens":
		return ok(map[string]any{
			"Payload": []byte(`{"total_tokens":0}`),
		})
	default:
		return errorEnvelope("unknown_method", "unknown method: "+method, 0), nil
	}
}

type requestTransformRequest struct {
	FromFormat string          `json:"FromFormat"`
	ToFormat   string          `json:"ToFormat"`
	Model      string          `json:"Model"`
	Stream     bool            `json:"Stream"`
	Body       json.RawMessage `json:"Body"`
}

type payloadResponse struct {
	Body json.RawMessage `json:"Body"`
}

// defaultSanitizeFields are request parameters the free-tier gateways reject.
//
// reasoning_effort is deliberately NOT here: the OpenCode v2 gateway accepts it
// (verified 200 OK on every advertised model) and it measurably changes how
// much the model reasons — `low` produced ~84-133 reasoning characters,
// `high` ~271-339, unset ~523-589 on the same prompt. Agent harnesses such as
// DeepSeek Harness send it on every turn, so stripping it silently removed the
// user's reasoning-depth control.
//
// The fields below are still rejected upstream with `400 invalid_request`
// ("unknown parameter") on a per-model basis — enable_thinking and
// thinking_budget fail on glm-5.3-flash-free, cat-jam-free and beam-free — so
// they are removed before the request leaves the plugin.
//
// CPA_KEEP_FIELDS re-enables any of them for a deployment whose upstream does
// accept them, so the list is not a permanent verdict.
var defaultSanitizeFields = []string{
	"thinking",
	"enable_thinking",
	"thinking_budget",
	"thinking_config",
	"search",
	"web_search",
	"return_citations",
}

// keepFields returns the set of parameter names that must survive sanitization
// even when they appear in the strip list.
func keepFields() map[string]bool {
	keep := make(map[string]bool)
	raw := os.Getenv("CPA_KEEP_FIELDS")
	if v := strings.TrimSpace(loadPluginSettings().Request.KeepFields); v != "" {
		raw = v
	}
	for _, f := range strings.Split(raw, ",") {
		f = strings.TrimSpace(f)
		if f != "" {
			keep[f] = true
		}
	}
	return keep
}

func sanitizePayloadMap(bodyMap map[string]any) bool {
	changed := false
	fieldsToStrip := append([]string(nil), defaultSanitizeFields...)
	custom := os.Getenv("CPA_STRIP_FIELDS")
	if v := strings.TrimSpace(loadPluginSettings().Request.StripFields); v != "" {
		custom = v
	}
	if custom != "" {
		for _, f := range strings.Split(custom, ",") {
			f = strings.TrimSpace(f)
			if f != "" {
				fieldsToStrip = append(fieldsToStrip, f)
			}
		}
	}
	keep := keepFields()
	for _, f := range fieldsToStrip {
		if keep[f] {
			continue
		}
		if _, exists := bodyMap[f]; exists {
			delete(bodyMap, f)
			changed = true
		}
	}
	return changed
}

// normalizeMessages ensures standard role conventions ("user", "assistant", "system")
// and repairs malformed/missing roles (e.g. empty role "" -> "user", "developer" -> "system",
// nil content -> "") sent by clients like Synaplan, Cline, Cursor, or completions callers.
// Resolves: "Request normalizer: failed to convert messages: unexpected message at index 0: expected 'user', 'assistant' or 'system', got """
func normalizeMessages(bodyMap map[string]any) bool {
	changed := false

	// If prompt exists but no messages, convert to standard chat messages
	if _, hasMessages := bodyMap["messages"]; !hasMessages {
		if prompt, ok := bodyMap["prompt"].(string); ok && strings.TrimSpace(prompt) != "" {
			bodyMap["messages"] = []any{
				map[string]any{
					"role":    "user",
					"content": prompt,
				},
			}
			delete(bodyMap, "prompt")
			changed = true
		}
	}

	msgs, ok := bodyMap["messages"].([]any)
	if !ok {
		return changed
	}

	if len(msgs) == 0 {
		bodyMap["messages"] = []any{
			map[string]any{
				"role":    "user",
				"content": "Hello",
			},
		}
		return true
	}

	for i, m := range msgs {
		msgMap, isMap := m.(map[string]any)
		if !isMap {
			continue
		}

		roleRaw, hasRole := msgMap["role"]
		roleStr := ""
		if hasRole && roleRaw != nil {
			if s, ok := roleRaw.(string); ok {
				roleStr = strings.TrimSpace(s)
			}
		}

		if roleStr == "" {
			// At index 0 or any turn with empty/missing role: default to safe user turn
			msgMap["role"] = "user"
			changed = true
		} else if strings.EqualFold(roleStr, "developer") {
			// Upstream gateways reject developer role; map to system
			msgMap["role"] = "system"
			changed = true
		} else if strings.EqualFold(roleStr, "system") || strings.EqualFold(roleStr, "user") || strings.EqualFold(roleStr, "assistant") || strings.EqualFold(roleStr, "tool") || strings.EqualFold(roleStr, "function") {
			lower := strings.ToLower(roleStr)
			if lower != roleStr {
				msgMap["role"] = lower
				changed = true
			}
		} else {
			// Any unrecognized role: default to user so validation doesn't reject it
			msgMap["role"] = "user"
			changed = true
		}

		// Ensure content is never nil/null
		if content, hasContent := msgMap["content"]; !hasContent || content == nil {
			msgMap["content"] = ""
			changed = true
		}

		msgs[i] = msgMap
	}

	return changed
}

// sanitizeAndNormalizePayload combines gateway parameter stripping and message
// normalization so payloads sent directly to Kilo or OpenCode are always clean.
func sanitizeAndNormalizePayload(payload []byte, model string) []byte {
	if len(payload) == 0 {
		return payload
	}
	var bodyMap map[string]any
	if err := json.Unmarshal(payload, &bodyMap); err != nil {
		return payload
	}

	changed := sanitizePayloadMap(bodyMap)
	if normalizeMessages(bodyMap) {
		changed = true
	}

	if changed {
		newBody, err := json.Marshal(bodyMap)
		if err == nil {
			return newBody
		}
	}
	return payload
}

// ── Model Exclusions (Issue #5) ─────────────────────────────────────────────
//
// Supports operator-configured model exclusions so models can be hidden from
// the catalog (/v1/models) and rejected locally before contacting upstream.
// Reads from:
//  1. Environment variable CPA_EXCLUDED_MODELS (comma-separated or JSON array)
//  2. Descriptor JSON file(s) in auth-dir (e.g. cpa-freetier-plugin.json with "excluded_models": [...])
var (
	excludedModelsMu    sync.RWMutex
	cachedExcludedRules []string
	lastExcludedCheck   time.Time
)

func getAuthDirs() []string {
	var dirs []string
	if d := strings.TrimSpace(os.Getenv("CLI_PROXY_AUTH_DIR")); d != "" {
		dirs = append(dirs, d)
	}
	if d := strings.TrimSpace(os.Getenv("CPA_AUTH_DIR")); d != "" {
		dirs = append(dirs, d)
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		dirs = append(dirs, filepath.Join(home, ".cli-proxy-api"))
	}
	dirs = append(dirs, "/etc/cli-proxy-api", ".")
	return dirs
}

func parseExcludedModelsValue(raw any) []string {
	var result []string
	switch v := raw.(type) {
	case string:
		v = strings.TrimSpace(v)
		if strings.HasPrefix(v, "[") && strings.HasSuffix(v, "]") {
			var slice []string
			if err := json.Unmarshal([]byte(v), &slice); err == nil {
				for _, s := range slice {
					s = strings.TrimSpace(s)
					if s != "" {
						result = append(result, s)
					}
				}
				return result
			}
		}
		for _, part := range strings.Split(v, ",") {
			part = strings.TrimSpace(part)
			if part != "" {
				result = append(result, part)
			}
		}
	case []any:
		for _, item := range v {
			if s, ok := item.(string); ok {
				s = strings.TrimSpace(s)
				if s != "" {
					result = append(result, s)
				}
			}
		}
	case []string:
		for _, s := range v {
			s = strings.TrimSpace(s)
			if s != "" {
				result = append(result, s)
			}
		}
	}
	return result
}

func getExcludedModelRules() []string {
	excludedModelsMu.RLock()
	if time.Since(lastExcludedCheck) < 2*time.Second && cachedExcludedRules != nil {
		rules := cachedExcludedRules
		excludedModelsMu.RUnlock()
		return rules
	}
	excludedModelsMu.RUnlock()

	excludedModelsMu.Lock()
	defer excludedModelsMu.Unlock()

	if time.Since(lastExcludedCheck) < 2*time.Second && cachedExcludedRules != nil {
		return cachedExcludedRules
	}

	rulesMap := make(map[string]bool)

	// 1. Environment variable CPA_EXCLUDED_MODELS
	if envVal := strings.TrimSpace(os.Getenv("CPA_EXCLUDED_MODELS")); envVal != "" {
		for _, item := range parseExcludedModelsValue(envVal) {
			rulesMap[strings.ToLower(item)] = true
		}
	}

	// 2. Auth descriptor files in auth directories
	for _, dir := range getAuthDirs() {
		files, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, fi := range files {
			if fi.IsDir() || !strings.HasSuffix(strings.ToLower(fi.Name()), ".json") {
				continue
			}
			filePath := filepath.Join(dir, fi.Name())
			data, errRead := os.ReadFile(filePath)
			if errRead != nil || len(data) == 0 {
				continue
			}
			var m map[string]any
			if errJSON := json.Unmarshal(data, &m); errJSON != nil {
				continue
			}
			prov, _ := m["provider"].(string)
			typ, _ := m["type"].(string)
			isCPA := prov == pluginID || typ == pluginID || fi.Name() == "cpa-freetier-plugin.json"
			if !isCPA && prov != "" && typ != "" {
				continue
			}
			if exRaw, ok := m["excluded_models"]; ok {
				for _, item := range parseExcludedModelsValue(exRaw) {
					rulesMap[strings.ToLower(item)] = true
				}
			}
			if exRaw, ok := m["excluded-models"]; ok {
				for _, item := range parseExcludedModelsValue(exRaw) {
					rulesMap[strings.ToLower(item)] = true
				}
			}
		}
	}

	rules := make([]string, 0, len(rulesMap))
	for r := range rulesMap {
		rules = append(rules, r)
	}
	cachedExcludedRules = rules
	lastExcludedCheck = time.Now()
	return rules
}

func isModelExcluded(model string) bool {
	model = strings.ToLower(strings.TrimSpace(model))
	if model == "" {
		return false
	}
	cleanModel := strings.TrimPrefix(strings.TrimPrefix(model, "opencode/"), "kilo/")
	rules := getExcludedModelRules()
	for _, rule := range rules {
		rule = strings.ToLower(strings.TrimSpace(rule))
		if rule == "" {
			continue
		}
		if rule == model || rule == cleanModel {
			return true
		}
		cleanRule := strings.TrimPrefix(strings.TrimPrefix(rule, "opencode/"), "kilo/")
		if cleanRule == cleanModel || cleanRule == model {
			return true
		}
		if matched, _ := filepath.Match(rule, model); matched {
			return true
		}
		if matched, _ := filepath.Match(cleanRule, cleanModel); matched {
			return true
		}
	}
	return false
}

func normalizeRequest(payload []byte) ([]byte, error) {
	var req requestTransformRequest
	if len(payload) > 0 {
		if errDecode := json.Unmarshal(payload, &req); errDecode != nil {
			return nil, errDecode
		}
	}
	if len(req.Body) == 0 {
		return ok(payloadResponse{Body: req.Body})
	}

	var bodyMap map[string]any
	if err := json.Unmarshal(req.Body, &bodyMap); err != nil {
		return ok(payloadResponse{Body: req.Body})
	}

	changed := sanitizePayloadMap(bodyMap)
	if normalizeMessages(bodyMap) {
		changed = true
	}

	if changed {
		logDebug("normalizeRequest: sanitized payload fields and messages for model=%s", req.Model)
		newBody, err := json.Marshal(bodyMap)
		if err == nil {
			return ok(payloadResponse{Body: json.RawMessage(newBody)})
		}
	}

	return ok(payloadResponse{Body: req.Body})
}

func listModels() []map[string]any {
	type modelDef struct {
		id, name     string
		context, max int64
		thinking     bool
	}

	items := []modelDef{
		// OpenCode Free Tier (9router v2 protocol)
		{"flint-40b-free", "Flint 40B Free (OpenCode)", 262144, 65536, true},
		{"nemotron-3-ultra-free", "Nemotron 3 Ultra Free (OpenCode)", 1000000, 128000, true},
		{"glm-5.3-flash-free", "GLM 5.3 Flash Free (OpenCode)", 200000, 32768, true},
		{"big-pickle", "Big Pickle Free (OpenCode)", 200000, 32000, true},
		{"deepseek-v4.1-flash-free", "DeepSeek V4.1 Flash Free (OpenCode)", 1000000, 384000, true},
		{"mimo-v2.6-flash-free", "MiMo v2.6 Flash Free (OpenCode)", 262144, 65536, true},
		{"beam-free", "Beam Free (OpenCode)", 200000, 32768, true},
		{"nemotron-3.5-super-vl-free", "Nemotron 3.5 Super VL Free (OpenCode)", 1000000, 65536, false},
		{"cat-jam-free", "Cat Jam Free (OpenCode)", 1048576, 131072, true},
		{"nemotron-3.5-lightning-free", "Nemotron 3.5 Lightning Free (OpenCode)", 1000000, 65536, false},

		// Kilo Free Tier (Active Curated Models)
		{"kilo-auto/free", "Kilo Auto Free", 256000, 32768, false},
		{"nvidia/nemotron-3-ultra-550b-a55b:free", "Nemotron 3 Ultra 550B Free (Kilo)", 1000000, 65536, true},
		{"cohere/north-mini-code:free", "North Mini Code Free (Kilo)", 256000, 64000, false},
		{"openrouter/free", "OpenRouter Free Auto (Kilo)", 200000, 65536, false},
		{"nvidia/nemotron-3-nano-omni-30b-a3b-reasoning:free", "Nemotron 3 Nano Omni Free (Kilo)", 256000, 65536, true},
		{"nvidia/nemotron-3.5-lightning:free", "Nemotron 3.5 Lightning Free (Kilo)", 1000000, 65536, false},
		{"nvidia/nemotron-3.5-super-vl:free", "Nemotron 3.5 Super VL Free (Kilo)", 1000000, 131072, false},
		{"poolside/laguna-s-2.1:free", "Poolside Laguna S 2.1 Free", 262144, 32768, false},
		{"poolside/laguna-xs-2.1:free", "Laguna XS 2.1 Free (Kilo)", 262144, 32768, false},
		{"dots-studio/dots-3-note-preview:free", "Dots3-Note Preview Free (Kilo)", 512000, 460800, false},
		{"liquid/lfm-2.5-2.6b:free", "LiquidAI LFM2.5 2.6B Free (Kilo)", 65536, 8192, false},
		{"reflection/beam-free", "Reflection Beam Free (Kilo)", 1048576, 131072, false},
		{"xingchen-agi/xing4.0-29b-a4b:free", "Xingchen 4.0 29B Free (Kilo)", 262144, 235929, false},
		{"nvidia/nemotron-3.5-content-safety:free", "Nemotron 3.5 Content Safety Free (Kilo)", 128000, 8192, false},
	}

	// OmniRush models come from the account's live gateway catalog: the ids the
	// account is actually served, not a list baked into this binary.
	for _, m := range omniRushCatalog() {
		items = append(items, modelDef{m.ID, m.Name + " (OmniRush)", m.Context, m.Output, m.Reasoning})
	}

	out := make([]map[string]any, 0, len(items))
	for _, item := range items {
		// Respect operator model exclusions (Issue #5)
		if isModelExcluded(item.id) {
			continue
		}
		// Never advertise a model the upstream has dropped.
		if !modelHealthy(item.id) {
			continue
		}
		out = append(out, map[string]any{
			"ID":                         item.id,
			"Object":                     "model",
			"OwnedBy":                    pluginID,
			"Type":                       pluginID,
			"DisplayName":                item.name,
			"SupportedGenerationMethods": []string{"chat"},
			"ContextLength":              item.context,
			"MaxCompletionTokens":        item.max,
			"UserDefined":                true,
		})
	}
	return out
}

func execute(raw []byte) ([]byte, error) {
	req, err := decodeRequest(raw)
	if err != nil {
		return errorEnvelope("invalid_request", err.Error(), 400), nil
	}
	if isModelExcluded(req.Model) {
		return errorEnvelope("model_not_found", fmt.Sprintf("model %q is excluded by plugin configuration", req.Model), 404), nil
	}
	logMsg("execute start: model=%s", req.Model)
	callerTools, hasCallerTools := extractCallerTools(req.Payload)
	resp, err := callUpstream(req.Model, req.Payload, false)
	if err != nil {
		logMsg("execute callUpstream err: %v", err)
		return errorEnvelope("upstream_error", err.Error(), 502), nil
	}
	defer resp.body.Close()

	var bodyBuf bytes.Buffer
	scanner := bufio.NewScanner(resp.body)
	scanner.Buffer(make([]byte, 256*1024), maxSSELineBytes)
	isSSE := false

	for scanner.Scan() {
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "data:") {
			isSSE = true
		}
		bodyBuf.WriteString(line)
		bodyBuf.WriteByte('\n')

		if trimmed == "data: [DONE]" || trimmed == "[DONE]" {
			break
		}
	}

	body := bodyBuf.Bytes()
	if isSSE {
		body = assembleSSEWithTools(body, req.Model, hasCallerTools, callerTools)
	}

	logMsg("execute complete: model=%s, len=%d", req.Model, len(body))
	return ok(map[string]any{
		"Payload": body,
		"Headers": responseHeaders(resp.header),
	})
}

func executeStream(raw []byte) ([]byte, error) {
	req, err := decodeRequest(raw)
	if err != nil {
		return errorEnvelope("invalid_request", err.Error(), 400), nil
	}
	if strings.TrimSpace(req.StreamID) == "" {
		return errorEnvelope("executor_error", "stream_id is required", 0), nil
	}
	if isModelExcluded(req.Model) {
		return errorEnvelope("model_not_found", fmt.Sprintf("model %q is excluded by plugin configuration", req.Model), 404), nil
	}

	logMsg("executeStream start: model=%s, stream_id=%s", req.Model, req.StreamID)
	callerTools, hasCallerTools := extractCallerTools(req.Payload)

	// Exactly one close per stream, whichever path fails first.
	var closeOnce sync.Once
	finish := func(errStr string) {
		closeOnce.Do(func() { closeStream(req.StreamID, errStr) })
	}

	go func() {
		resp, callErr := callUpstream(req.Model, req.Payload, true)
		if callErr != nil {
			logMsg("executeStream callUpstream error: %v", callErr)
			finish(callErr.Error())
			return
		}
		defer resp.body.Close()

		filter := newStreamToolFilter(hasCallerTools, callerTools)
		scanner := bufio.NewScanner(resp.body)
		scanner.Buffer(make([]byte, 256*1024), maxSSELineBytes)

		for scanner.Scan() {
			line := scanner.Text()
			trimmed := strings.TrimSpace(line)
			if trimmed == "" || strings.HasPrefix(trimmed, ":") {
				continue
			}

			// Clean double data prefix
			cleaned := line
			if strings.HasPrefix(cleaned, "data:") {
				cleaned = strings.TrimSpace(strings.TrimPrefix(cleaned, "data:"))
			}
			if strings.HasPrefix(cleaned, "data:") {
				cleaned = strings.TrimSpace(strings.TrimPrefix(cleaned, "data:"))
			}

			if cleaned == "[DONE]" || cleaned == "data: [DONE]" {
				break
			}

			cleaned, shouldEmit := filter.filterChunk(cleaned)
			if !shouldEmit {
				continue
			}

			if errEmit := emitStream(req.StreamID, []byte(cleaned)); errEmit != nil {
				logMsg("executeStream emitStream error: %v", errEmit)
				finish(errEmit.Error())
				return
			}
		}
		if scanErr := scanner.Err(); scanErr != nil {
			logMsg("executeStream scanner error: %v", scanErr)
			finish(scanErr.Error())
			return
		}
		finish("")
		logMsg("executeStream closed successfully: stream_id=%s", req.StreamID)
	}()

	return ok(map[string]any{
		"Headers": http.Header{"Content-Type": []string{"text/event-stream"}},
	})
}

func restoreToolNamesInLine(line string) string {
	if strings.Contains(line, "read_client") {
		line = strings.ReplaceAll(line, "read_client", "read")
	}
	if strings.Contains(line, "shell_client") {
		line = strings.ReplaceAll(line, "shell_client", "shell")
	}
	return line
}

func decodeRequest(raw []byte) (executorRequest, error) {
	var req executorRequest
	if err := json.Unmarshal(raw, &req); err == nil && len(req.Payload) > 0 {
		if strings.TrimSpace(req.Model) == "" {
			return req, fmt.Errorf("model is required")
		}
		return req, nil
	}

	// Tolerant fallback: payload can be raw JSON or case-insensitive fields
	var alt struct {
		Model           string          `json:"model"`
		ModelUpper      string          `json:"Model"`
		StreamID        string          `json:"stream_id"`
		StreamIDUpper   string          `json:"StreamID"`
		Payload         json.RawMessage `json:"payload"`
		PayloadUpper    json.RawMessage `json:"Payload"`
		OriginalRequest json.RawMessage `json:"original_request"`
		OriginalUpper   json.RawMessage `json:"OriginalRequest"`
	}
	if err := json.Unmarshal(raw, &alt); err == nil {
		model := alt.Model
		if model == "" {
			model = alt.ModelUpper
		}
		streamID := alt.StreamID
		if streamID == "" {
			streamID = alt.StreamIDUpper
		}
		payload := alt.Payload
		if len(payload) == 0 {
			payload = alt.PayloadUpper
		}
		if len(payload) == 0 {
			payload = alt.OriginalRequest
		}
		if len(payload) == 0 {
			payload = alt.OriginalUpper
		}
		if strings.TrimSpace(model) == "" {
			return req, fmt.Errorf("model is required")
		}
		if len(payload) == 0 {
			return req, fmt.Errorf("request payload is required")
		}
		return executorRequest{
			Model:    model,
			StreamID: streamID,
			Payload:  []byte(payload),
		}, nil
	}

	return req, fmt.Errorf("invalid executor request payload")
}

// ── Model health ────────────────────────────────────────────────────────────
//
// Upstream catalogs rotate without notice: ids disappear and come back as 404 /
// "does not exist" / "model is unavailable". Re-advertising a dead id makes an
// agent pick it and fail, so a model that looks gone is hidden from the published
// catalog for a while rather than offered again on every request.
const modelUnhealthyTTL = 30 * time.Minute

var (
	modelHealthMu sync.Mutex
	unhealthyTill = map[string]time.Time{}
)

// modelErrorStatuses are the upstream statuses that mean "this model is gone".
var modelErrorStatuses = []string{"400", "404", "410"}

// looksModelMissing decides whether an upstream error means the model itself is
// gone. It requires a model-level client status AND catalogue wording, because a
// bare phrase match would also catch a local error such as `file does not
// exist`, hiding a model that is perfectly healthy.
func looksModelMissing(msg string) bool {
	m := strings.ToLower(strings.TrimSpace(msg))

	status := false
	for _, s := range modelErrorStatuses {
		if strings.HasPrefix(m, s+" ") {
			status = true
			break
		}
	}
	if !status {
		return false
	}
	for _, needle := range []string{
		"not found",
		"does not exist",
		"unknown model",
		"unknown provider",
		"model is unavailable",
		"model_not_found",
		"modeldeprecated",
		"deprecated",
	} {
		if strings.Contains(m, needle) {
			return true
		}
	}
	return false
}

// markModelUnhealthy records upstream availability. A success clears the mark.
func markModelUnhealthy(model string, err error) {
	if model == "" {
		return
	}
	modelHealthMu.Lock()
	defer modelHealthMu.Unlock()

	if err == nil {
		if _, known := unhealthyTill[model]; known {
			delete(unhealthyTill, model)
			logMsg("model back in service: %s", model)
		}
		return
	}
	if !looksModelMissing(err.Error()) {
		return
	}
	if _, known := unhealthyTill[model]; !known {
		logMsg("model marked unavailable for %s (upstream error: %v)", modelUnhealthyTTL, err)
	}
	unhealthyTill[model] = time.Now().Add(modelUnhealthyTTL)
}

func modelHealthy(id string) bool {
	modelHealthMu.Lock()
	defer modelHealthMu.Unlock()
	until, ok := unhealthyTill[id]
	if !ok {
		return true
	}
	if time.Now().After(until) {
		delete(unhealthyTill, id)
		return true
	}
	return false
}

func callUpstream(model string, payload []byte, stream bool) (*upstreamResponse, error) {
	cleanModel := strings.TrimPrefix(strings.TrimPrefix(strings.TrimPrefix(model, "opencode/"), "kilo/"), "omnirush/")
	payload = sanitizeAndNormalizePayload(payload, cleanModel)
	var (
		resp *upstreamResponse
		err  error
	)
	if kiloModels[model] || kiloModels[cleanModel] {
		resp, err = callKilo(model, payload, stream)
	} else if isOmniRushModelID(model) || isOmniRushModelID(cleanModel) {
		resp, err = callOmniRush(cleanModel, payload, stream)
	} else {
		resp, err = callOpenCode(cleanModel, payload, stream)
	}
	markModelUnhealthy(cleanModel, err)
	return resp, err
}

func getOpenCodeBaseURL() string {
	if base, ok := settingsSidecarBase(); ok {
		// Explicitly disabled, or a concrete base URL. Either way it wins over env.
		return base
	}
	if u := strings.TrimRight(os.Getenv("OPENCODE_BASE_URL"), "/"); u != "" {
		return u
	}
	conn, err := net.DialTimeout("tcp", "127.0.0.1:8789", 50*time.Millisecond)
	if err == nil {
		conn.Close()
		return "http://127.0.0.1:8789"
	}
	return ""
}

func callOpenCode(model string, payload []byte, stream bool) (*upstreamResponse, error) {
	baseURL := getOpenCodeBaseURL()
	cleanRequested := strings.TrimPrefix(model, "opencode/")

	candidates := []string{cleanRequested}
	seen := map[string]bool{cleanRequested: true}
	for _, fb := range openCodeFallbackModels {
		if !seen[fb] {
			seen[fb] = true
			candidates = append(candidates, fb)
		}
	}

	var payloadMap map[string]any
	if err := json.Unmarshal(payload, &payloadMap); err != nil {
		return nil, fmt.Errorf("invalid json payload: %w", err)
	}

	if sanitizePayloadMap(payloadMap) {
		logDebug("callOpenCode: stripped unsupported parameters")
	}

	injectOpenCodeHarness(payloadMap)

	sessionID := strings.TrimSpace(loadPluginSettings().Identity.SessionID)
	if sessionID == "" {
		sessionID = getOrCreateSession("opencode_global")
	}

	identity := loadPluginSettings().Identity
	if identity.MimicCLI && (strings.EqualFold(strings.TrimSpace(identity.UserAgent), "") || identity.UserAgent == "opencode/latest/2.0.32/cli") {
		identity.UserAgent = opencodeClientUA
	}
	project := strings.TrimSpace(identity.Project)
	if project == "" || project == "global" {
		project = getProjectHash()
	}

	var lastErr error

	for _, candidate := range candidates {
		targetURL := buildOpenCodeTargetURL(baseURL, candidate)

		payloadMap["model"] = candidate
		transformedPayload, err := json.Marshal(payloadMap)
		if err != nil {
			return nil, err
		}

		logMsg("callOpenCode: attempt model=%s targetURL=%s payloadLen=%d payloadHash=%s", candidate, targetURL, len(transformedPayload), payloadFingerprint(transformedPayload))
		logDebug("callOpenCode: sessionID=%s candidate=%s", sessionID, candidate)

		b3, tp := generateTraceHeaders()
		headers := http.Header{
			"Content-Type":          []string{"application/json"},
			"Authorization":         []string{"Bearer public"},
			"User-Agent":            []string{identity.UserAgent},
			"x-opencode-client":     []string{identity.Client},
			"x-opencode-project":    []string{project},
			"x-opencode-session-id": []string{sessionID},
			"x-opencode-session":    []string{sessionID},
			"x-session-affinity":    []string{sessionID},
			"x-session-id":          []string{sessionID},
			"X-Session-Id":          []string{sessionID},
			"b3":                    []string{b3},
			"traceparent":           []string{tp},
			"Accept":                []string{"text/event-stream"},
		}

		if !identity.MimicCLI {
			// Debug mode: identify honestly and drop the CLI disguise headers.
			headers.Set("User-Agent", "cpa-freetier-plugin/"+strings.TrimPrefix(pluginVersion, "v"))
			for _, h := range []string{"x-opencode-client", "x-opencode-project", "x-opencode-session-id", "x-opencode-session", "x-session-affinity", "x-session-id", "X-Session-Id", "b3", "traceparent"} {
				headers.Del(h)
			}
		}

		isLocal := strings.HasPrefix(targetURL, "http://127.0.0.1") || strings.HasPrefix(targetURL, "http://localhost")
		var client *http.Client
		proxyKey := "direct"
		if isLocal {
			client = getHTTPClient(false)
		} else {
			proxy := getOpenCodeProxy()
			if proxy != nil {
				proxyKey = proxy.String()
			}
			client = getHTTPClient(true)
		}
		logDebug("callOpenCode: egress=%s", proxyKey)

		req, err := http.NewRequest(http.MethodPost, targetURL, bytes.NewReader(transformedPayload))
		if err != nil {
			return nil, err
		}
		req.Header = headers

		started := time.Now()
		res, err := client.Do(req)
		if err != nil {
			if !isLocal {
				invalidateTransport(proxyKey)
				invalidateOpenCodeProxy()
			}
			logMsg("callOpenCode: transport error model=%s egress=%s after %s: %v", candidate, proxyKey, time.Since(started).Round(time.Millisecond), err)
			return nil, err
		}

		logMsg("callOpenCode: model=%s egress=%s status=%d in %s", candidate, proxyKey, res.StatusCode, time.Since(started).Round(time.Millisecond))

		if res.StatusCode >= 400 {
			b, _ := io.ReadAll(io.LimitReader(res.Body, 8192))
			_, _ = io.Copy(io.Discard, res.Body)
			res.Body.Close()
			bodyStr := strings.TrimSpace(string(b))
			if res.StatusCode == 429 && baseURL == "" {
				lastErr = fmt.Errorf("429 Too Many Requests: FreeUsageLimitError (Cloudflare blocks Go TLS; Bun sidecar on 127.0.0.1:8789 required for OpenCode)")
				return nil, lastErr
			}
			lastErr = fmt.Errorf("%s: %s", res.Status, bodyStr)

			if isOpenCodeFallbackError(res.StatusCode, bodyStr) {
				markModelUnhealthy(candidate, lastErr)
				logMsg("callOpenCode: candidate %s failed (%s), attempting next fallback...", candidate, res.Status)
				continue
			}
			return nil, lastErr
		}

		return &upstreamResponse{status: res.StatusCode, header: res.Header, body: res.Body}, nil
	}

	return nil, lastErr
}

// Official OpenCode v2.0.26 fingerprint tools verbatim from 9router
var officialFingerprintTools = []map[string]any{
	{
		"type": "function",
		"function": map[string]any{
			"name":        "read",
			"description": "Read the contents of a file or directory. Supports text files, images, and PDFs. Images and PDFs are presented directly to the model. Each text line is prefixed by its 1-based line number as <line>: <content>. The prefix is for reference and is not part of the file content. Directory entries are returned one per line. Use offset and limit to read large files or directories in sections. Prefer one larger read over many small slices, and use grep to find specific content in large files.",
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path":   map[string]any{"type": "string", "description": "File or directory to read"},
					"offset": map[string]any{"type": "integer", "minimum": 0, "description": "The line or directory entry to start reading from (1-based)"},
					"limit":  map[string]any{"type": "integer", "minimum": 0, "description": "The maximum number of lines or directory entries to read (defaults to and capped at 2000)"},
				},
				"required":             []string{"path"},
				"additionalProperties": false,
			},
		},
	},
	{
		"type": "function",
		"function": map[string]any{
			"name":        "shell",
			"description": "Execute a shell command and return its output. Commands run on Linux using bash. Quote file paths containing spaces or special characters. Prefer dedicated tools over shell commands when possible. When output is large, the full result is saved to a file and a truncated preview is returned. Rely on automatic truncation unless filtering the output is more useful. Commands accept an optional timeout, background commands have no timeout by default. Background commands return immediately, and you will be notified when they complete.",
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"command":    map[string]any{"type": "string", "description": "Shell command string to execute"},
					"workdir":    map[string]any{"type": "string", "description": "Working directory to execute the command in. Defaults to the current working directory. When possible, avoid changing directories in the command and set the working directory here instead."},
					"timeout":    map[string]any{"type": "integer", "minimum": 0, "description": "Timeout in milliseconds. Set to 0 to disable the timeout. Defaults to 120000 for foreground commands. Background commands have no timeout by default."},
					"background": map[string]any{"type": "boolean", "description": "Run the command in the background and return immediately (useful for dev servers and long-running builds). You do not need to use '&' at the end of the command when using this parameter. You will be notified when it completes. DO NOT poll for completion."},
				},
				"required":             []string{"command"},
				"additionalProperties": false,
			},
		},
	},
}

func cloakOpencodePayload(raw []byte, isResponses bool) []byte {
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		return raw
	}

	if sanitizePayloadMap(body) {
		logDebug("cloakOpencodePayload: stripped unsupported parameters")
	}

	if m, ok := body["model"].(string); ok {
		body["model"] = strings.TrimPrefix(m, "opencode/")
	}

	body["stream"] = true
	body["stream_options"] = map[string]any{"include_usage": true}

	existingTools, _ := body["tools"].([]any)
	var finalTools []any
	namesPresent := make(map[string]bool)

	for _, t := range existingTools {
		tm, ok := t.(map[string]any)
		if !ok {
			continue
		}
		fn, isFn := tm["function"].(map[string]any)
		name := ""
		if isFn {
			name, _ = fn["name"].(string)
		} else {
			name, _ = tm["name"].(string)
		}
		name = strings.TrimSpace(name)
		if name == "read" {
			name = "read_client"
			if isFn {
				fn["name"] = name
			} else {
				tm["name"] = name
			}
		} else if name == "shell" {
			name = "shell_client"
			if isFn {
				fn["name"] = name
			} else {
				tm["name"] = name
			}
		}
		namesPresent[name] = true
		finalTools = append(finalTools, tm)
	}

	for _, ft := range officialFingerprintTools {
		fn := ft["function"].(map[string]any)
		n := fn["name"].(string)
		if isResponses {
			finalTools = append(finalTools, map[string]any{
				"type":        "function",
				"name":        n,
				"description": fn["description"],
				"parameters":  fn["parameters"],
			})
		} else {
			finalTools = append(finalTools, deepCopyMap(ft))
		}
	}

	body["tools"] = finalTools
	body["tool_choice"] = "auto"

	note := "Do not call the `read` or `shell` tools; they are placeholders that do nothing. Answer the prompt directly or use other tools."
	if msgs, ok := body["messages"].([]any); ok && len(msgs) > 0 {
		firstMsg, _ := msgs[0].(map[string]any)
		if firstMsg != nil && firstMsg["role"] == "system" {
			if content, ok := firstMsg["content"].(string); ok {
				firstMsg["content"] = content + "\n\n" + note
			}
		} else {
			body["messages"] = append([]any{map[string]any{"role": "system", "content": note}}, msgs...)
		}
	}

	out, err := json.Marshal(body)
	if err != nil {
		return raw
	}
	return out
}

func callKilo(model string, payload []byte, stream bool) (*upstreamResponse, error) {
	headers := http.Header{
		"Content-Type": []string{"application/json"},
	}
	if stream {
		headers.Set("Accept", "text/event-stream")
	}

	client := getHTTPClient(false)
	req, err := http.NewRequest(http.MethodPost, kiloChatURL, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header = headers

	res, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	if res.StatusCode >= 400 {
		defer res.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(res.Body, 8192))
		return nil, fmt.Errorf("%s: %s", res.Status, strings.TrimSpace(string(b)))
	}
	return &upstreamResponse{status: res.StatusCode, header: res.Header, body: res.Body}, nil
}

func assembleSSE(raw []byte, fallbackModel string) []byte {
	return assembleSSEWithTools(raw, fallbackModel, true, nil)
}

func assembleSSEWithTools(raw []byte, fallbackModel string, hasCallerTools bool, callerTools []string) []byte {
	lines := strings.Split(string(raw), "\n")
	var resID string
	var created int64
	model := fallbackModel
	var contentParts []string
	var reasoningParts []string
	finishReason := "stop"
	var usage any

	type toolCallObj struct {
		ID       string `json:"id"`
		Type     string `json:"type"`
		Function struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"function"`
	}
	toolCalls := make(map[int]*toolCallObj)
	droppedToolIndices := make(map[int]bool)

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		dataStr := strings.TrimSpace(line[5:])
		if dataStr == "[DONE]" {
			break
		}
		var chunk struct {
			ID      string `json:"id"`
			Created int64  `json:"created"`
			Model   string `json:"model"`
			Usage   any    `json:"usage"`
			Choices []struct {
				FinishReason string `json:"finish_reason"`
				Delta        struct {
					Content          string `json:"content"`
					ReasoningContent string `json:"reasoning_content"`
					ToolCalls        []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(dataStr), &chunk); err == nil {
			if resID == "" && chunk.ID != "" {
				resID = chunk.ID
			}
			if created == 0 && chunk.Created != 0 {
				created = chunk.Created
			}
			if chunk.Model != "" {
				model = chunk.Model
			}
			if chunk.Usage != nil {
				usage = chunk.Usage
			}
			for _, choice := range chunk.Choices {
				if choice.FinishReason != "" {
					finishReason = choice.FinishReason
				}
				if choice.Delta.Content != "" {
					contentParts = append(contentParts, choice.Delta.Content)
				}
				if choice.Delta.ReasoningContent != "" {
					reasoningParts = append(reasoningParts, choice.Delta.ReasoningContent)
				}
				for _, tc := range choice.Delta.ToolCalls {
					if !hasCallerTools {
						continue
					}
					if droppedToolIndices[tc.Index] {
						continue
					}

					name := tc.Function.Name
					if name == "read_client" {
						name = "read"
					} else if name == "shell_client" {
						name = "shell"
					}

					if name != "" && len(callerTools) > 0 && !isOfferedTool(name, callerTools) && isHarnessTool(name) {
						droppedToolIndices[tc.Index] = true
						delete(toolCalls, tc.Index)
						continue
					}

					obj, exists := toolCalls[tc.Index]
					if !exists {
						if name == "" {
							continue
						}
						obj = &toolCallObj{Type: "function"}
						toolCalls[tc.Index] = obj
					}
					if tc.ID != "" {
						obj.ID = tc.ID
					}
					if name != "" {
						obj.Function.Name = name
					}
					if tc.Function.Arguments != "" {
						obj.Function.Arguments += tc.Function.Arguments
					}
				}
			}
		}
	}

	if !hasCallerTools || len(toolCalls) == 0 {
		if finishReason == "tool_calls" {
			finishReason = "stop"
		}
	}

	msg := map[string]any{
		"role": "assistant",
	}
	if len(contentParts) > 0 {
		msg["content"] = strings.Join(contentParts, "")
	} else if !hasCallerTools {
		msg["content"] = ""
	} else {
		msg["content"] = nil
	}
	if len(reasoningParts) > 0 {
		msg["reasoning_content"] = strings.Join(reasoningParts, "")
	}
	if hasCallerTools && len(toolCalls) > 0 {
		var list []*toolCallObj
		for i := 0; i < len(toolCalls); i++ {
			if t, ok := toolCalls[i]; ok {
				list = append(list, t)
			}
		}
		if len(list) > 0 {
			msg["tool_calls"] = list
		}
	}

	if resID == "" {
		resID = "chatcmpl-cpa-freetier"
	}
	if created == 0 {
		created = time.Now().Unix()
	}

	respObj := map[string]any{
		"id":      resID,
		"object":  "chat.completion",
		"created": created,
		"model":   model,
		"choices": []map[string]any{
			{
				"index":         0,
				"message":       msg,
				"finish_reason": finishReason,
			},
		},
	}
	if usage != nil {
		respObj["usage"] = usage
	}

	out, err := json.Marshal(respObj)
	if err != nil {
		return raw
	}
	return out
}

var (
	streamEmitHook  func(streamID string, payload []byte) error
	streamCloseHook func(streamID, errStr string)
)

func emitStream(streamID string, payload []byte) error {
	if streamEmitHook != nil {
		return streamEmitHook(streamID, payload)
	}
	req := streamEmit{StreamID: streamID, Payload: payload}
	rawPayload, err := json.Marshal(req)
	if err != nil {
		return err
	}
	cMethod := C.CString("host.stream.emit")
	defer C.free(unsafe.Pointer(cMethod))

	var response C.cliproxy_buffer
	var requestPtr *C.uint8_t
	if len(rawPayload) > 0 {
		cPayload := C.CBytes(rawPayload)
		if cPayload == nil {
			return fmt.Errorf("allocate stream emit payload")
		}
		defer C.free(cPayload)
		requestPtr = (*C.uint8_t)(cPayload)
	}

	rc := C.call_host_api(cMethod, requestPtr, C.size_t(len(rawPayload)), &response)
	if response.ptr != nil {
		C.free_host_buffer(response.ptr, response.len)
	}
	if rc != 0 {
		return fmt.Errorf("host stream_emit failed: %d", rc)
	}
	return nil
}

func closeStream(streamID, errStr string) {
	if streamCloseHook != nil {
		streamCloseHook(streamID, errStr)
		return
	}
	req := streamClose{StreamID: streamID, Error: errStr}
	rawPayload, _ := json.Marshal(req)
	cMethod := C.CString("host.stream.close")
	defer C.free(unsafe.Pointer(cMethod))

	var response C.cliproxy_buffer
	var requestPtr *C.uint8_t
	if len(rawPayload) > 0 {
		cPayload := C.CBytes(rawPayload)
		if cPayload != nil {
			defer C.free(cPayload)
			requestPtr = (*C.uint8_t)(cPayload)
		}
	}

	C.call_host_api(cMethod, requestPtr, C.size_t(len(rawPayload)), &response)
	if response.ptr != nil {
		C.free_host_buffer(response.ptr, response.len)
	}
}

func ok(v any) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return json.Marshal(envelope{OK: true, Result: b})
}

func errorEnvelope(code, message string, httpStatus int) []byte {
	b, _ := json.Marshal(envelope{
		OK: false,
		Error: &pluginError{
			Code:       code,
			Message:    message,
			HTTPStatus: httpStatus,
		},
	})
	return b
}

func responseHeaders(h http.Header) map[string][]string {
	out := make(map[string][]string)
	for k, v := range h {
		if len(v) > 0 {
			out[k] = v
		}
	}
	return out
}

func writeResponse(buf *C.cliproxy_buffer, b []byte) {
	if buf == nil || len(b) == 0 {
		return
	}
	buf.ptr = C.CBytes(b)
	buf.len = C.size_t(len(b))
}
