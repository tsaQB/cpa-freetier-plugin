package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// PluginSettings is the runtime-editable layer behind the dashboard's settings
// panel. The file wins over environment variables so an operator can change
// egress or request identity without restarting CLIProxyAPI; environment values
// remain the fallback for headless deployments.
type PluginSettings struct {
	Proxy    ProxySettings    `json:"proxy"`
	Identity IdentitySettings `json:"identity"`
	Request  RequestSettings  `json:"request"`
	Logging  LoggingSettings  `json:"logging"`
}

// ProxySettings controls egress. Empty means "not configured here, fall back to
// the environment and then to the compiled-in defaults"; the sentinel "-" means
// "explicitly disabled".
type ProxySettings struct {
	OpenCode        string `json:"opencode"`
	OpenCodePool    string `json:"opencodePool"`
	AllowDatacenter bool   `json:"allowDatacenter"`
	DatacenterPool  string `json:"datacenterPool"`
	OmniRush        string `json:"omnirush"`
}

// IdentitySettings is the decoy layer: the free-tier gates accept requests that
// look like the official CLI, so these knobs describe that disguise.
type IdentitySettings struct {
	MimicCLI  bool   `json:"mimicCli"`
	UserAgent string `json:"userAgent"`
	Client    string `json:"client"`
	Project   string `json:"project"`
	SessionID string `json:"sessionId"`
	Sidecar   string `json:"sidecar"`
	// ClientVersion overrides the advertised OmniRush CLI version (e.g.
	// "2.7.8"). Empty means "use the compiled fallback". Keep this current with
	// the published `omnirush` npm release; a stale version is the "lookalike
	// client" signal the gateway flags.
	ClientVersion string `json:"clientVersion"`
	// ReferralCode is appended to the sign-up links so a new account credits the
	// operator. Accepts a bare code or a full console URL; "-" disables it.
	ReferralCode string `json:"referralCode"`
}

type RequestSettings struct {
	HeaderTimeoutSec int `json:"headerTimeoutSec"`
	MaxRetries       int `json:"maxRetries"`
	// OmniRushTimeoutSec bounds how long a queued free-tier generation may take
	// to send its first byte before the plugin gives up with a readable error.
	OmniRushTimeoutSec int `json:"omnirushTimeoutSec"`
	// OmniRushReasoning is sent when the caller does not ask for a level; "low"
	// is the fast path, empty means "whatever the model defaults to".
	OmniRushReasoning string `json:"omnirushReasoning"`
	StripFields       string `json:"stripFields"`
	KeepFields        string `json:"keepFields"`
}

type LoggingSettings struct {
	Debug bool `json:"debug"`
}

var (
	settingsMu    sync.RWMutex
	currentConfig *PluginSettings
	settingsStamp string
)

const (
	proxyDisabledSentinel = "-"
	defaultHeaderTimeout  = 120
	defaultMaxRetries     = 4
	// The free tier queues a generation when the account is busy, so waiting
	// much longer than this only turns a slow answer into an apparent hang.
	defaultOmniRushTimeout = 45
	minOmniRushTimeout     = 5
	// Default referral used when a new OmniRush account is created from the UI.
	defaultOmniReferralCode = "5WSC5CWX"
	defaultOmniConsoleURL   = "https://omnirush.ai/console"
)

func defaultPluginSettings() *PluginSettings {
	return &PluginSettings{
		Proxy: ProxySettings{},
		Identity: IdentitySettings{
			MimicCLI:  true,
			UserAgent: opencodeClientUA,
			Client:    "cli",
			Project:   getProjectHash(),
			// Empty means "auto": probe 127.0.0.1:8789, then fall back to env.
			Sidecar:      "",
			ReferralCode: defaultOmniReferralCode,
		},
		Request: RequestSettings{
			HeaderTimeoutSec:   defaultHeaderTimeout,
			MaxRetries:         defaultMaxRetries,
			OmniRushTimeoutSec: defaultOmniRushTimeout,
			OmniRushReasoning:  "",
		},
		Logging: LoggingSettings{},
	}
}

func settingsPath() string {
	if p := strings.TrimSpace(os.Getenv("CPA_FREETIER_SETTINGS")); p != "" {
		return p
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".cli-proxy-api", "cpa-freetier-settings.json")
	}
	return "cpa-freetier-settings.json"
}

// normalizeSettings fills gaps left by an older or hand-edited file so callers
// never have to reason about zero values.
func normalizeSettings(s *PluginSettings) {
	def := defaultPluginSettings()
	s.Proxy.OpenCode = strings.TrimSpace(s.Proxy.OpenCode)
	s.Proxy.OpenCodePool = strings.TrimSpace(s.Proxy.OpenCodePool)
	s.Proxy.DatacenterPool = strings.TrimSpace(s.Proxy.DatacenterPool)
	s.Proxy.OmniRush = strings.TrimSpace(s.Proxy.OmniRush)

	if strings.TrimSpace(s.Identity.UserAgent) == "" || s.Identity.UserAgent == "opencode/latest/2.0.32/cli" {
		s.Identity.UserAgent = def.Identity.UserAgent
	}
	if strings.TrimSpace(s.Identity.Client) == "" {
		s.Identity.Client = def.Identity.Client
	}
	if strings.TrimSpace(s.Identity.Project) == "" || strings.TrimSpace(s.Identity.Project) == "global" {
		s.Identity.Project = def.Identity.Project
	}
	s.Identity.Sidecar = strings.TrimSpace(s.Identity.Sidecar)
	if s.Request.HeaderTimeoutSec <= 0 {
		s.Request.HeaderTimeoutSec = def.Request.HeaderTimeoutSec
	}
	if strings.TrimSpace(s.Identity.ReferralCode) == "" {
		// "-" is the explicit opt-out; an empty value means "use the default".
		s.Identity.ReferralCode = def.Identity.ReferralCode
	}
	if s.Request.OmniRushTimeoutSec <= 0 {
		s.Request.OmniRushTimeoutSec = def.Request.OmniRushTimeoutSec
	}
	s.Request.OmniRushReasoning = normalizeReasoningLevel(s.Request.OmniRushReasoning)
	if s.Request.MaxRetries < 0 {
		s.Request.MaxRetries = def.Request.MaxRetries
	}
}

func loadPluginSettings() *PluginSettings {
	settingsMu.RLock()
	if currentConfig != nil {
		cfg := currentConfig
		settingsMu.RUnlock()
		return cfg
	}
	settingsMu.RUnlock()

	settingsMu.Lock()
	defer settingsMu.Unlock()
	if currentConfig != nil {
		return currentConfig
	}

	cfg := defaultPluginSettings()
	path := settingsPath()
	if data, err := os.ReadFile(path); err == nil && len(data) > 0 {
		// Start from the defaults and overlay the file on top: JSON has no
		// notion of "unset", so unmarshalling into a zero struct would turn
		// a partial file (or `{}`) into MimicCLI=false and empty timeouts —
		// silently dropping the CLI disguise and hardening on every host
		// whose settings file predates a new default field.
		parsed := *cfg
		if err := json.Unmarshal(data, &parsed); err == nil {
			cfg = &parsed
			settingsStamp = fmt.Sprintf("%d", len(data))
		}
	}
	normalizeSettings(cfg)
	currentConfig = cfg
	return cfg
}

// validateSettings rejects anything that would break egress silently: a typo in
// a proxy URL otherwise shows up much later as an opaque upstream timeout.
func validateSettings(s *PluginSettings) error {
	var checkProxy func(label, raw string) error
	checkProxy = func(label, raw string) error {
		raw = strings.TrimSpace(raw)
		if raw == "" || raw == proxyDisabledSentinel {
			return nil
		}
		if strings.Contains(raw, ",") {
			for _, part := range strings.Split(raw, ",") {
				if err := checkProxy(label, part); err != nil {
					return err
				}
			}
			return nil
		}
		if !strings.Contains(raw, "://") {
			raw = "http://" + raw
		}
		u, err := url.Parse(raw)
		if err != nil || !validProxyTarget(u) {
			return fmt.Errorf("%s: %q is not a usable proxy address", label, raw)
		}
		return nil
	}

	if err := checkProxy("opencode proxy", s.Proxy.OpenCode); err != nil {
		return err
	}
	if err := checkProxy("opencode pool", s.Proxy.OpenCodePool); err != nil {
		return err
	}
	if err := checkProxy("datacenter pool", s.Proxy.DatacenterPool); err != nil {
		return err
	}
	if err := checkProxy("omnirush proxy", s.Proxy.OmniRush); err != nil {
		return err
	}

	if raw := strings.TrimSpace(s.Request.OmniRushReasoning); raw != "" && normalizeReasoningLevel(raw) == "" {
		return fmt.Errorf("omnirush reasoning %q is not one of low, medium, high, xhigh, max", raw)
	}
	if s.Request.OmniRushTimeoutSec < 0 || s.Request.OmniRushTimeoutSec > 900 ||
		(s.Request.OmniRushTimeoutSec > 0 && s.Request.OmniRushTimeoutSec < minOmniRushTimeout) {
		return fmt.Errorf("omnirush timeout must be between %d and 900 seconds", minOmniRushTimeout)
	}

	if s.Request.HeaderTimeoutSec < 5 || s.Request.HeaderTimeoutSec > 900 {
		return errors.New("header timeout must be between 5 and 900 seconds")
	}
	if s.Request.MaxRetries < 0 || s.Request.MaxRetries > 10 {
		return errors.New("max retries must be between 0 and 10")
	}

	sidecar := strings.TrimSpace(s.Identity.Sidecar)
	if sidecar != "" && sidecar != proxyDisabledSentinel && !strings.Contains(sidecar, "://") {
		if _, _, err := splitHostPortLoose(sidecar); err != nil {
			return fmt.Errorf("sidecar: %q is not a usable host:port", sidecar)
		}
	}
	if strings.ContainsAny(s.Identity.UserAgent, "\r\n") {
		return errors.New("user agent must be a single line")
	}
	return nil
}

func savePluginSettings(s *PluginSettings) error {
	if s == nil {
		return errors.New("empty settings payload")
	}
	// Validate what the operator actually sent: normalising first would turn a
	// typo into a silent default and hide the mistake.
	if err := validateSettings(s); err != nil {
		return err
	}
	normalizeSettings(s)

	path := settingsPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create settings dir: %w", err)
	}
	payload, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	payload = append(payload, '\n')

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, payload, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}

	settingsMu.Lock()
	currentConfig = s
	settingsStamp = time.Now().UTC().Format(time.RFC3339Nano)
	settingsMu.Unlock()

	resetEgressCaches()
	return nil
}

// resetEgressCaches drops every memoised decision that depends on settings so a
// save takes effect on the next request instead of up to a minute later.
func resetEgressCaches() {
	proxyCacheMu.Lock()
	proxyCacheURL = nil
	proxyCacheAt = time.Time{}
	proxyCacheInit = false
	proxyCacheMu.Unlock()

	transportCache.Range(func(key, value any) bool {
		if tr, ok := value.(*http.Transport); ok {
			tr.CloseIdleConnections()
		}
		transportCache.Delete(key)
		return true
	})
}

func resetPluginSettings() (*PluginSettings, error) {
	cfg := defaultPluginSettings()
	if err := savePluginSettings(cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

// ── effective-value helpers used by the request paths ───────────────────────

func settingsProxyValue(raw string) (string, bool) {
	v := strings.TrimSpace(raw)
	if v == "" {
		return "", false
	}
	if v == proxyDisabledSentinel {
		return "", true
	}
	return v, true
}

func settingsOpenCodeProxy() (*url.URL, bool) {
	raw, ok := settingsProxyValue(loadPluginSettings().Proxy.OpenCode)
	if !ok || raw == "" {
		return nil, ok
	}
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || !validProxyTarget(u) {
		return nil, false
	}
	return u, true
}

func settingsOmniRushProxy() (*url.URL, bool) {
	raw, ok := settingsProxyValue(loadPluginSettings().Proxy.OmniRush)
	if !ok || raw == "" {
		return nil, ok
	}
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || !validProxyTarget(u) {
		return nil, false
	}
	return u, true
}

func settingsSidecarBase() (string, bool) {
	raw, ok := settingsProxyValue(loadPluginSettings().Identity.Sidecar)
	if !ok {
		return "", false
	}
	if raw == "" {
		// "-" means the sidecar is disabled: call the upstream directly.
		return "", true
	}
	raw = strings.TrimRight(raw, "/")
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	return raw, true
}

// settingsReferralCode returns the code to append to sign-up links. Empty or the
// "-" sentinel means "do not attach a referral".
func settingsReferralCode() string {
	raw := strings.TrimSpace(loadPluginSettings().Identity.ReferralCode)
	if raw == "" {
		raw = strings.TrimSpace(os.Getenv("OMNIRUSH_REFERRAL_CODE"))
	}
	if raw == "" {
		raw = defaultOmniReferralCode
	}
	return normalizeReferralCode(raw)
}

// normalizeReferralCode accepts a bare code or any console URL and returns just
// the code, so pasting the full invite link into settings also works.
func normalizeReferralCode(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == proxyDisabledSentinel {
		return ""
	}
	if strings.Contains(raw, "://") || strings.Contains(raw, "ref=") {
		if u, err := url.Parse(raw); err == nil {
			if code := strings.TrimSpace(u.Query().Get("ref")); code != "" {
				return code
			}
		}
		if i := strings.Index(raw, "ref="); i >= 0 {
			code := raw[i+4:]
			if j := strings.IndexAny(code, "&#"); j >= 0 {
				code = code[:j]
			}
			return strings.TrimSpace(code)
		}
		return ""
	}
	return raw
}

// withReferral appends the configured referral to a console URL, keeping any
// query parameters the gateway already put there (the device code, for example).
func withReferral(rawURL string) string {
	code := settingsReferralCode()
	if code == "" {
		return rawURL
	}
	trimmed := strings.TrimSpace(rawURL)
	if trimmed == "" {
		trimmed = defaultOmniConsoleURL
	}
	u, err := url.Parse(trimmed)
	if err != nil {
		return trimmed
	}
	q := u.Query()
	if q.Get("ref") == code {
		return u.String()
	}
	q.Set("ref", code)
	u.RawQuery = q.Encode()
	return u.String()
}

// settingsOmniRushTimeout bounds the wait for the first byte of an OmniRush
// generation: the free tier queues requests while the account is busy, and an
// unbounded wait is indistinguishable from a hang on the client side.
func settingsOmniRushTimeout() time.Duration {
	secs := loadPluginSettings().Request.OmniRushTimeoutSec
	if secs < minOmniRushTimeout {
		secs = defaultOmniRushTimeout
	}
	return time.Duration(secs) * time.Second
}

// settingsOmniRushReasoning is applied only when the caller asks for no level.
func settingsOmniRushReasoning() string {
	return normalizeReasoningLevel(loadPluginSettings().Request.OmniRushReasoning)
}

func normalizeReasoningLevel(raw string) string {
	switch level := strings.ToLower(strings.TrimSpace(raw)); level {
	case "minimal", "low", "medium", "high", "xhigh", "max":
		return level
	default:
		return ""
	}
}

func settingsHeaderTimeout() time.Duration {
	secs := loadPluginSettings().Request.HeaderTimeoutSec
	if secs <= 0 {
		secs = defaultHeaderTimeout
	}
	return time.Duration(secs) * time.Second
}

// settingsEnvHints tells the dashboard which values are currently shadowed by
// the settings file, so the UI can explain why an env var appears ignored.
func settingsEnvHints() map[string]string {
	hints := map[string]string{}
	cfg := loadPluginSettings()
	add := func(key, value, settingsValue string) {
		if strings.TrimSpace(value) == "" {
			return
		}
		if strings.TrimSpace(settingsValue) != "" {
			hints[key] = "overridden in settings"
			return
		}
		hints[key] = "env active"
	}
	add("OPENCODE_PROXY", os.Getenv("OPENCODE_PROXY"), cfg.Proxy.OpenCode)
	add("OPENCODE_PROXY_POOL", os.Getenv("OPENCODE_PROXY_POOL"), cfg.Proxy.OpenCodePool)
	add("OPENCODE_DATACENTER_PROXY_POOL", os.Getenv("OPENCODE_DATACENTER_PROXY_POOL"), cfg.Proxy.DatacenterPool)
	add("OPENCODE_ALLOW_DATACENTER", os.Getenv("OPENCODE_ALLOW_DATACENTER"), boolSetting(cfg.Proxy.AllowDatacenter))
	add("OPENCODE_BASE_URL", os.Getenv("OPENCODE_BASE_URL"), cfg.Identity.Sidecar)
	add("OMNIRUSH_PROXY", os.Getenv("OMNIRUSH_PROXY"), cfg.Proxy.OmniRush)
	return hints
}

func boolSetting(v bool) string {
	if v {
		return "true"
	}
	return ""
}

func splitHostPortLoose(v string) (string, string, error) {
	host, port, err := net.SplitHostPort(v)
	if err != nil {
		return "", "", err
	}
	if _, err := strconv.Atoi(port); err != nil {
		return "", "", err
	}
	return host, port, nil
}
