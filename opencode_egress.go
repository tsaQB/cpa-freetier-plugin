package main

// Per-egress free-quota memory.
//
// The OpenCode free tier meters a *network*, not an identity: once a network is
// spent, every further call from it answers
//
//	429 FreeUsageLimitError with a Retry-After of hours
//
// A proxy pool therefore only spreads quota if the plugin remembers which
// egress is spent. Without this, every request re-dials the same burnt egress
// (the pool cache picks the first reachable one), burns a retry on it, and the
// operator's whole allowance drains faster than it should.
//
// So: a quota verdict puts that egress on ice until its own Retry-After, and
// egress selection prefers anything still healthy. The map is persisted 0600 so
// a restart does not resurrect the burnt egress.

import (
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	egressCoolingFile    = "opencode-egress-cooling.json"
	defaultQuotaCooldown = 5 * time.Minute
	directEgressKey      = "direct"
)

type egressCooler struct {
	mu    sync.Mutex
	until map[string]time.Time
	path  string
}

var openCodeEgress = &egressCooler{until: map[string]time.Time{}}

func egressCoolingPath() string {
	if p := strings.TrimSpace(os.Getenv("CPA_EGRESS_COOLING_FILE")); p != "" {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return egressCoolingFile
	}
	return filepath.Join(home, ".cli-proxy-api", egressCoolingFile)
}

// loadEgressCooling reads the persisted map once per process. Failures are
// non-fatal: an empty map only costs a wasted retry, never a wrong answer.
func loadEgressCooling() {
	openCodeEgress.mu.Lock()
	defer openCodeEgress.mu.Unlock()
	if openCodeEgress.path != "" {
		return
	}
	openCodeEgress.path = egressCoolingPath()

	data, err := os.ReadFile(openCodeEgress.path)
	if err != nil || len(data) == 0 {
		return
	}
	stored := map[string]time.Time{}
	if json.Unmarshal(data, &stored) != nil {
		return
	}
	now := time.Now()
	for key, until := range stored {
		if until.After(now) {
			openCodeEgress.until[key] = until
		}
	}
}

func saveEgressCoolingLocked() {
	if openCodeEgress.path == "" {
		return
	}
	now := time.Now()
	for key, until := range openCodeEgress.until {
		if !until.After(now) {
			delete(openCodeEgress.until, key)
		}
	}
	if len(openCodeEgress.until) == 0 {
		_ = os.Remove(openCodeEgress.path)
		return
	}
	payload, err := json.Marshal(openCodeEgress.until)
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(openCodeEgress.path), 0o700); err != nil {
		return
	}
	tmp := openCodeEgress.path + ".tmp"
	if os.WriteFile(tmp, payload, 0o600) != nil {
		return
	}
	if os.Rename(tmp, openCodeEgress.path) != nil {
		_ = os.Remove(tmp)
	}
}

// coolingUntil returns the cooldown expiry for an egress key, zero when healthy.
func coolingUntil(key string) time.Time {
	loadEgressCooling()
	openCodeEgress.mu.Lock()
	defer openCodeEgress.mu.Unlock()
	until := openCodeEgress.until[key]
	if !until.After(time.Now()) {
		return time.Time{}
	}
	return until
}

// markEgressCooling benches an egress for the upstream-stated Retry-After.
func markEgressCooling(key string, d time.Duration) time.Time {
	if d <= 0 {
		d = defaultQuotaCooldown
	}
	loadEgressCooling()
	openCodeEgress.mu.Lock()
	defer openCodeEgress.mu.Unlock()
	until := time.Now().Add(d)
	if until.After(openCodeEgress.until[key]) {
		openCodeEgress.until[key] = until
	}
	saveEgressCoolingLocked()
	return until
}

// clearEgressCooling marks an egress healthy again after a served request.
func clearEgressCooling(key string) {
	openCodeEgress.mu.Lock()
	defer openCodeEgress.mu.Unlock()
	if _, ok := openCodeEgress.until[key]; !ok {
		return
	}
	delete(openCodeEgress.until, key)
	saveEgressCoolingLocked()
}

func egressKey(u *url.URL) string {
	if u == nil {
		return directEgressKey
	}
	return u.String()
}

type egressAttempt struct {
	key   string
	proxy *url.URL
}

// openCodeEgressAttempts orders the egresses to try, healthiest first:
//
//	explicit proxy (settings / OPENCODE_PROXY) -> single attempt
//	pool: healthy entries, then cooling ones by soonest recovery, then direct
//
// Direct is only appended when the pool yields nothing healthy, matching the
// previous "no reachable pool member means ambient egress" behaviour.
func openCodeEgressAttempts(isLocal bool) []egressAttempt {
	if isLocal {
		return []egressAttempt{{key: "local"}}
	}
	if u, handled := settingsOpenCodeProxy(); handled {
		return []egressAttempt{{key: egressKey(u), proxy: u}}
	}
	if p := strings.TrimSpace(os.Getenv("OPENCODE_PROXY")); p != "" {
		if u, err := url.Parse(p); err == nil {
			return []egressAttempt{{key: egressKey(u), proxy: u}}
		}
	}

	pool := proxyPool("OPENCODE_PROXY_POOL", defaultOpenCodeProxies)
	if datacenterAllowed() {
		pool = append(pool, proxyPool("OPENCODE_DATACENTER_PROXY_POOL", defaultOpenCodeDatacenterProxies)...)
	}

	type cooled struct {
		att   egressAttempt
		until time.Time
	}
	var healthy []egressAttempt
	var cooling []cooled
	seen := map[string]bool{}
	for _, u := range pool {
		key := egressKey(u)
		if seen[key] {
			continue
		}
		seen[key] = true
		if !proxyReachable(u) {
			continue
		}
		att := egressAttempt{key: key, proxy: u}
		if until := coolingUntil(key); !until.IsZero() {
			cooling = append(cooling, cooled{att: att, until: until})
			continue
		}
		healthy = append(healthy, att)
	}

	if len(healthy) > 0 {
		return healthy
	}
	sort.Slice(cooling, func(i, j int) bool { return cooling[i].until.Before(cooling[j].until) })
	out := make([]egressAttempt, 0, len(cooling)+1)
	for _, c := range cooling {
		out = append(out, c.att)
	}
	// Every pool member is spent: still try the ambient path, it may be a
	// different network than the pool.
	if until := coolingUntil(directEgressKey); until.IsZero() {
		out = append(out, egressAttempt{key: directEgressKey})
	}
	return out
}

// egressCoolingSnapshot exposes the cooldown table for the dashboard.
func egressCoolingSnapshot() map[string]any {
	loadEgressCooling()
	openCodeEgress.mu.Lock()
	defer openCodeEgress.mu.Unlock()
	out := map[string]any{}
	now := time.Now()
	for key, until := range openCodeEgress.until {
		if until.After(now) {
			out[key] = until.UTC().Format(time.RFC3339)
		}
	}
	return out
}
