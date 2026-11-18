package main

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/subtle"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// dashboardTokenPlaceholder is replaced with the live token in the served page.
	dashboardTokenPlaceholder = "__CPA_FREETIER_TOKEN__"
	dashboardTokenHeader      = "X-Cpa-Freetier-Token"
)

var (
	dashboardTokenOnce  sync.Once
	dashboardTokenValue string
)

// dashboardToken returns a per-process random token embedded in the dashboard
// page. Resource routes cannot see the Management key, so this is what proves a
// mutating call came from the page the operator actually loaded.
func dashboardToken() string {
	dashboardTokenOnce.Do(func() {
		buf := make([]byte, 16)
		if _, err := rand.Read(buf); err != nil {
			dashboardTokenValue = strconv.FormatInt(time.Now().UnixNano(), 16)
			return
		}
		dashboardTokenValue = hex.EncodeToString(buf)
	})
	return dashboardTokenValue
}

func dashboardTokenValid(headers map[string][]string) bool {
	if len(headers) == 0 {
		return false
	}
	for name, values := range headers {
		if !strings.EqualFold(name, dashboardTokenHeader) {
			continue
		}
		for _, value := range values {
			if subtle.ConstantTimeCompare([]byte(value), []byte(dashboardToken())) == 1 {
				return true
			}
		}
	}
	return false
}

// mutatingResourcePath reports whether a plugin resource path changes state.
func mutatingResourcePath(path string) bool {
	switch {
	case strings.HasSuffix(path, "/omnirush/disconnect"),
		strings.HasSuffix(path, "/omnirush/remove"),
		strings.HasSuffix(path, "/omnirush/toggle"),
		strings.HasSuffix(path, "/omnirush/import"),
		strings.HasSuffix(path, "/omnirush/start"),
		strings.HasSuffix(path, "/models/toggle"),
		strings.HasSuffix(path, "/settings/save"),
		strings.HasSuffix(path, "/settings/reset"),
		strings.HasSuffix(path, "/proxy/test"):
		return true
	}
	return false
}

//go:embed dashboard.html
var dashboardHTML []byte

type managementRegistrationResponse struct {
	Routes    []managementRoute `json:"routes,omitempty"`
	Resources []resourceRoute   `json:"resources,omitempty"`
}

type resourceRoute struct {
	Path        string `json:"path"`
	Menu        string `json:"menu,omitempty"`
	Description string `json:"description,omitempty"`
}

type managementRoute struct {
	Method      string `json:"method"`
	Path        string `json:"path"`
	Menu        string `json:"menu,omitempty"`
	Description string `json:"description,omitempty"`
}

type managementRequestEnvelope struct {
	Method         string              `json:"Method"`
	Path           string              `json:"Path"`
	Headers        map[string][]string `json:"Headers"`
	Query          map[string][]string `json:"Query"`
	Body           []byte              `json:"Body"`
	HostCallbackID string              `json:"host_callback_id,omitempty"`
}

type managementResponseEnvelope struct {
	StatusCode int                 `json:"StatusCode"`
	Headers    map[string][]string `json:"Headers"`
	Body       []byte              `json:"Body"`
}

func handleManagementRegister(raw []byte) ([]byte, error) {
	// Every endpoint is declared once, as a resource route: the host serves
	// /v0/resource/plugins/<id>/<path> for the iframe, and a resource route with
	// a non-empty Menu also becomes the Management Center sidebar entry.
	//
	// Declaring the page in both Routes and Resources makes the host register the
	// Routes copy first and skip the Resources copy as a "conflict with a
	// higher-priority plugin", so only one declaration per path is allowed.
	resp := managementRegistrationResponse{
		Resources: []resourceRoute{
			{
				Path:        "/dashboard",
				Menu:        "Free Tier Plugin",
				Description: "Manage free models, provider connections, and account status",
			},
			{
				Path:        "/api/status",
				Description: "Live status of models, proxy, and OmniRush integration",
			},
			{
				Path:        "/api/omnirush/start",
				Description: "Start OmniRush OAuth Device Authorization",
			},
			{
				Path:        "/api/omnirush/poll",
				Description: "Poll OmniRush Device Authorization status",
			},
			{
				Path:        "/api/omnirush/disconnect",
				Description: "Disconnect OmniRush stored credentials",
			},
			{
				Path:        "/api/omnirush/import",
				Description: "Import OmniRush credentials directly",
			},
			{
				Path:        "/api/omnirush/accounts",
				Description: "List OmniRush accounts in the rotation pool",
			},
			{
				Path:        "/api/omnirush/remove",
				Description: "Remove one OmniRush account from the pool",
			},
			{
				Path:        "/api/omnirush/toggle",
				Description: "Enable or disable one OmniRush account",
			},
			{
				Path:        "/api/models/toggle",
				Description: "Toggle model active/excluded state",
			},
			{
				Path:        "/api/models/probe",
				Description: "Live probe a model for latency and response health",
			},
			{
				Path:        "/api/proxy/ping",
				Description: "Ping egress network and sidecar proxies",
			},
			{
				Path:        "/api/settings",
				Description: "Read and save proxy, identity, and request settings",
			},
			{
				Path:        "/api/settings/save",
				Description: "Save plugin settings from the dashboard",
			},
			{
				Path:        "/api/settings/reset",
				Description: "Reset plugin settings to defaults",
			},
			{
				Path:        "/api/proxy/test",
				Description: "Test a proxy candidate against a target",
			},
		},
	}

	return ok(resp)
}

func handleManagementHandle(raw []byte) ([]byte, error) {
	var req managementRequestEnvelope
	if err := json.Unmarshal(raw, &req); err != nil {
		return errorEnvelope("invalid_request", "failed to decode management request: "+err.Error(), 400), nil
	}

	path := strings.TrimSpace(req.Path)
	q := url.Values(req.Query)

	// 1. Dashboard UI HTML
	if strings.HasSuffix(path, "/dashboard") || path == "" || path == "/" {
		page := bytes.ReplaceAll(dashboardHTML, []byte(dashboardTokenPlaceholder), []byte(dashboardToken()))
		return managementResponse(http.StatusOK, "text/html; charset=utf-8", page)
	}

	// Mutating calls carry the token embedded in the served page. Resource routes
	// are served by the host without a Management key, and the actions here are
	// GET requests, so without this a cross-site <img src="..."> on any page
	// could disconnect the account or disable every model.
	if mutatingResourcePath(path) && !dashboardTokenValid(req.Headers) {
		return jsonErrorResponse(http.StatusForbidden, "forbidden",
			"missing or invalid dashboard token: open the dashboard and retry")
	}

	// 2. Settings API
	if strings.HasSuffix(path, "/settings/reset") {
		return handleSettingsReset()
	}
	if strings.HasSuffix(path, "/settings/save") {
		return handleSettingsSavePayload(q)
	}
	if strings.HasSuffix(path, "/settings") {
		return handleSettingsGet()
	}
	if strings.HasSuffix(path, "/proxy/test") {
		return handleProxyTest(q)
	}

	// 3. Status API
	if strings.HasSuffix(path, "/status") || strings.HasSuffix(path, "/api/status") {
		return handleStatusAPI()
	}

	// 3. OmniRush Device Flow: Start
	if strings.HasSuffix(path, "/omnirush/start") || strings.HasSuffix(path, "/api/omnirush/start") {
		authResp, err := omniRushDeviceAuthorize()
		if err != nil {
			return jsonErrorResponse(http.StatusBadGateway, "device_authorize_failed", err.Error())
		}
		// Every sign-up/in link carries the referral so a brand-new account credits
		// the operator. The device code survives because the query is merged.
		verifyURL := withReferral(authResp.VerificationURI)
		verifyComplete := withReferral(authResp.VerificationURIComplete)
		data, _ := json.Marshal(map[string]any{
			"status":                    "ok",
			"device_code":               authResp.DeviceCode,
			"user_code":                 authResp.UserCode,
			"verification_uri":          verifyURL,
			"verification_uri_complete": verifyComplete,
			"referral_code":             settingsReferralCode(),
			"referral_url":              withReferral(defaultOmniConsoleURL),
			"expires_in":                authResp.ExpiresIn,
			"interval":                  authResp.Interval,
		})
		return managementResponse(http.StatusOK, "application/json", data)
	}

	// 4. OmniRush Device Flow: Poll
	if strings.HasSuffix(path, "/omnirush/poll") || strings.HasSuffix(path, "/api/omnirush/poll") {
		code := q.Get("device_code")
		if code == "" {
			var bodyMap map[string]string
			if len(req.Body) > 0 {
				_ = json.Unmarshal(req.Body, &bodyMap)
				code = bodyMap["device_code"]
			}
		}
		status, email, err := omniRushDevicePoll(code)
		if err != nil && status != "pending" && status != "expired" {
			return jsonErrorResponse(http.StatusOK, "poll_failed", err.Error())
		}
		data, _ := json.Marshal(map[string]any{
			"status": status,
			"email":  email,
		})
		return managementResponse(http.StatusOK, "application/json", data)
	}

	// 4b. OmniRush account pool
	if strings.HasSuffix(path, "/omnirush/accounts") {
		backfillOmniAccountLabels()
		store, err := loadOmniAccounts()
		if err != nil {
			return jsonErrorResponse(http.StatusInternalServerError, "accounts_failed", err.Error())
		}
		data, _ := json.Marshal(map[string]any{
			"status":   "ok",
			"accounts": omniAccountsForUI(),
			"path":     omniAccountsPath(),
			"count":    len(store.Accounts),
		})
		return managementResponse(http.StatusOK, "application/json", data)
	}

	if strings.HasSuffix(path, "/omnirush/remove") {
		id := strings.TrimSpace(q.Get("id"))
		if id == "" {
			return jsonErrorResponse(http.StatusBadRequest, "missing_id", "id or email is required")
		}
		if err := removeOmniAccount(id); err != nil {
			return jsonErrorResponse(http.StatusNotFound, "remove_failed", err.Error())
		}
		data, _ := json.Marshal(map[string]any{
			"status":   "ok",
			"message":  "account removed",
			"accounts": omniAccountsForUI(),
		})
		return managementResponse(http.StatusOK, "application/json", data)
	}

	if strings.HasSuffix(path, "/omnirush/toggle") {
		id := strings.TrimSpace(q.Get("id"))
		if id == "" {
			return jsonErrorResponse(http.StatusBadRequest, "missing_id", "id or email is required")
		}
		enabled := q.Get("enabled") != "0" && q.Get("enabled") != "false"
		if err := setOmniAccountEnabled(id, enabled); err != nil {
			return jsonErrorResponse(http.StatusNotFound, "toggle_failed", err.Error())
		}
		data, _ := json.Marshal(map[string]any{
			"status":   "ok",
			"accounts": omniAccountsForUI(),
		})
		return managementResponse(http.StatusOK, "application/json", data)
	}

	// 5. OmniRush Disconnect
	if strings.HasSuffix(path, "/omnirush/disconnect") || strings.HasSuffix(path, "/api/omnirush/disconnect") {
		if err := clearOmniRushAuth(); err != nil {
			return jsonErrorResponse(http.StatusInternalServerError, "disconnect_failed", err.Error())
		}
		data, _ := json.Marshal(map[string]any{
			"status":  "ok",
			"message": "omnirush disconnected successfully",
		})
		return managementResponse(http.StatusOK, "application/json", data)
	}

	// 6. OmniRush Direct Token Import
	if strings.HasSuffix(path, "/omnirush/import") || strings.HasSuffix(path, "/api/omnirush/import") {
		accessToken := strings.TrimSpace(q.Get("access_token"))
		refreshToken := strings.TrimSpace(q.Get("refresh_token"))
		rawJSON := strings.TrimSpace(q.Get("json"))

		if rawJSON == "" && len(req.Body) > 0 {
			rawJSON = strings.TrimSpace(string(req.Body))
		}

		if rawJSON != "" {
			var m map[string]any
			if err := json.Unmarshal([]byte(rawJSON), &m); err == nil {
				if at, ok := m["accessToken"].(string); ok && at != "" {
					accessToken = at
				} else if at, ok := m["access_token"].(string); ok && at != "" {
					accessToken = at
				}
				if rt, ok := m["refreshToken"].(string); ok && rt != "" {
					refreshToken = rt
				} else if rt, ok := m["refresh_token"].(string); ok && rt != "" {
					refreshToken = rt
				}
				if accessToken == "" {
					if tokens, ok := m["tokens"].(map[string]any); ok {
						if idTok, ok := tokens["idToken"].(string); ok {
							accessToken = idTok
						}
						if refTok, ok := tokens["refreshToken"].(string); ok {
							refreshToken = refTok
						}
					}
				}
			}
		}

		if accessToken == "" && refreshToken == "" {
			return jsonErrorResponse(http.StatusBadRequest, "invalid_credentials", "access_token or refresh_token is required")
		}

		auth := &OmniRushAuth{
			AccessToken:  accessToken,
			RefreshToken: refreshToken,
			GatewayURL:   defaultOmniRushOrigin,
			SavedAt:      time.Now().UTC().Format(time.RFC3339),
			Email:        strings.TrimSpace(q.Get("email")),
		}

		if accessToken == "" && refreshToken != "" {
			acct, errSave := upsertOmniAccount(auth, getOmniRushAuthPath())
			if errSave != nil {
				return jsonErrorResponse(http.StatusInternalServerError, "save_failed", errSave.Error())
			}
			refreshed, errRef := refreshOmniAccountToken(acct, "")
			if errRef != nil {
				return jsonErrorResponse(http.StatusBadGateway, "refresh_failed", errRef.Error())
			}
			auth = refreshed
		}

		if auth.AccessToken != "" && auth.Email == "" {
			auth.Email = fetchOmniRushUserEmail(auth.AccessToken, auth.GatewayURL)
		}

		if err := saveOmniRushAuth(auth); err != nil {
			return jsonErrorResponse(http.StatusInternalServerError, "save_failed", err.Error())
		}

		data, _ := json.Marshal(map[string]any{
			"status": "ok",
			"email":  auth.Email,
		})
		return managementResponse(http.StatusOK, "application/json", data)
	}

	// 7. Model Fleet Toggle (supports single model or comma-separated batch)
	if strings.HasSuffix(path, "/models/toggle") || strings.HasSuffix(path, "/api/models/toggle") {
		model := q.Get("model")
		enabledStr := q.Get("enabled")
		if model == "" && len(req.Body) > 0 {
			var bodyMap map[string]any
			if err := json.Unmarshal(req.Body, &bodyMap); err == nil {
				if m, ok := bodyMap["model"].(string); ok {
					model = m
				}
				if en, ok := bodyMap["enabled"].(bool); ok {
					if en {
						enabledStr = "1"
					} else {
						enabledStr = "0"
					}
				}
			}
		}

		enabled := enabledStr == "1" || strings.EqualFold(enabledStr, "true")
		if model == "" {
			return jsonErrorResponse(http.StatusBadRequest, "invalid_param", "model parameter is required")
		}

		if err := setModelExclusion(model, !enabled); err != nil {
			return jsonErrorResponse(http.StatusInternalServerError, "toggle_failed", err.Error())
		}

		data, _ := json.Marshal(map[string]any{
			"status":          "ok",
			"model":           model,
			"enabled":         enabled,
			"excluded_models": getExcludedModelRules(),
		})
		return managementResponse(http.StatusOK, "application/json", data)
	}

	// 8. Model Live Probe
	if strings.HasSuffix(path, "/models/probe") || strings.HasSuffix(path, "/api/models/probe") {
		model := strings.TrimSpace(q.Get("model"))
		return handleModelProbe(model)
	}

	// 9. Ping Diagnostics
	if strings.HasSuffix(path, "/proxy/ping") || strings.HasSuffix(path, "/api/proxy/ping") {
		_, sidecarMS, _ := probeSidecar()
		proxyMS := firstPoolProbeAddr()
		upstreamMS := pingTCP("100.119.217.33:8888")

		data, _ := json.Marshal(map[string]any{
			"sidecar_ms":  sidecarMS,
			"proxy_ms":    proxyMS,
			"upstream_ms": upstreamMS,
		})
		return managementResponse(http.StatusOK, "application/json", data)
	}

	return jsonErrorResponse(http.StatusNotFound, "route_not_found", "no management handler for "+path)
}

func handleModelProbe(model string) ([]byte, error) {
	if model == "" {
		return jsonErrorResponse(http.StatusBadRequest, "invalid_param", "model parameter is required")
	}

	start := time.Now()
	payload := []byte(fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"Reply with the single word OK."}],"max_tokens":16,"stream":false}`, model))
	resp, err := callUpstream(model, payload, false)
	latency := time.Since(start).Milliseconds()
	if err != nil {
		data, _ := json.Marshal(map[string]any{
			"status":     "error",
			"model":      model,
			"latency_ms": latency,
			"error":      err.Error(),
		})
		return managementResponse(http.StatusOK, "application/json", data)
	}
	defer resp.body.Close()

	var bodyBuf bytes.Buffer
	scanner := bufio.NewScanner(resp.body)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
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

	rawBody := bodyBuf.Bytes()
	if isSSE {
		rawBody = assembleSSE(rawBody, model)
	}

	snippet := ""
	var parsed map[string]any
	if err := json.Unmarshal(rawBody, &parsed); err == nil {
		if choices, ok := parsed["choices"].([]any); ok && len(choices) > 0 {
			if c0, ok := choices[0].(map[string]any); ok {
				if msg, ok := c0["message"].(map[string]any); ok {
					if content, ok := msg["content"].(string); ok && strings.TrimSpace(content) != "" {
						snippet = strings.TrimSpace(content)
					} else if rc, ok := msg["reasoning_content"].(string); ok && strings.TrimSpace(rc) != "" {
						snippet = strings.TrimSpace(rc)
					}
				}
			}
		}
	}
	if snippet == "" {
		snippet = "200 OK"
	}
	if len(snippet) > 120 {
		snippet = snippet[:120] + "..."
	}

	data, _ := json.Marshal(map[string]any{
		"status":     "ok",
		"model":      model,
		"latency_ms": latency,
		"reply":      snippet,
	})
	return managementResponse(http.StatusOK, "application/json", data)
}

func handleStatusAPI() ([]byte, error) {
	auth, authPath, _ := loadOmniRushAuth()
	omniConnected := auth != nil && (auth.AccessToken != "" || auth.RefreshToken != "")
	omniEmail := ""
	savedAt := ""
	if omniConnected {
		omniEmail = auth.Email
		savedAt = auth.SavedAt
	}

	sidecarAddr, sidecarMS, sidecarActive := probeSidecar()

	proxyMS := firstPoolProbeAddr()
	proxyConfigured := getOpenCodeProxy() != nil || len(proxyPool("OPENCODE_PROXY_POOL", defaultOpenCodeProxies)) > 0

	type modelEntry struct {
		ID        string `json:"id"`
		Name      string `json:"name"`
		Provider  string `json:"provider"`
		Context   int64  `json:"context"`
		Max       int64  `json:"max"`
		Reasoning bool   `json:"reasoning"`
		Enabled   bool   `json:"enabled"`
		Healthy   bool   `json:"healthy"`
	}

	mk := func(id, name, provider string, ctx, max int64, reasoning bool) modelEntry {
		return modelEntry{
			ID:        id,
			Name:      name,
			Provider:  provider,
			Context:   ctx,
			Max:       max,
			Reasoning: reasoning,
			Enabled:   !isModelExcluded(id),
			Healthy:   modelHealthy(id),
		}
	}

	curated := curatedModels()
	allList := make([]modelEntry, 0, len(curated))
	for _, c := range curated {
		allList = append(allList, mk(c.id, c.shortName, c.provider, c.context, c.max, c.thinking))
	}

	// OmniRush rows mirror the account's live gateway catalog.
	for _, m := range omniRushCatalog() {
		allList = append(allList, mk(m.ID, m.Name, "omnirush", m.Context, m.Output, m.Reasoning))
	}

	payload, err := json.Marshal(map[string]any{
		"plugin":  pluginID,
		"version": pluginVersion,
		"omnirush": map[string]any{
			"connected": omniConnected,
			"email":     omniEmail,
			"saved_at":  savedAt,
			"auth_path": authPath,
			"accounts":  omniAccountsForUI(),
			"count":     countEnabledOmniAccounts(),
			"pool_path": omniAccountsPath(),
		},
		"sidecar": map[string]any{
			"zen_active": sidecarActive,
			"native_tls": nativeTLSEnabled(),
			"mode":       openCodeEgressMode(),
			"zen_url":    getOpenCodeBaseURL(),
			"zen_addr":   sidecarAddr,
			"latency_ms": sidecarMS,
		},
		"harness": HarnessInfo(),
		"proxy_pool": map[string]any{
			"configured": proxyConfigured,
			"reachable":  proxyMS != "down",
			"latency_ms": proxyMS,
		},
		"models":          allList,
		"excluded_models": getExcludedModelRules(),
	})
	if err != nil {
		return errorEnvelope("internal_error", err.Error(), 500), nil
	}

	return managementResponse(http.StatusOK, "application/json", payload)
}

func setModelExclusion(model string, exclude bool) error {
	model = strings.TrimSpace(strings.ToLower(model))
	if model == "" {
		return errors.New("model is required")
	}

	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return errors.New("cannot determine user home directory")
	}
	targetPath := filepath.Join(home, ".cli-proxy-api", "cpa-freetier-plugin.json")

	var dataMap map[string]any
	if raw, err := os.ReadFile(targetPath); err == nil && len(raw) > 0 {
		_ = json.Unmarshal(raw, &dataMap)
	}
	if dataMap == nil {
		dataMap = map[string]any{
			"disabled": false,
			"provider": pluginID,
			"type":     pluginID,
		}
	}

	var currentExcluded []string
	if rawEx, ok := dataMap["excluded_models"]; ok {
		currentExcluded = parseExcludedModelsValue(rawEx)
	}

	set := make(map[string]bool)
	for _, item := range currentExcluded {
		set[strings.ToLower(strings.TrimSpace(item))] = true
	}

	for _, part := range strings.Split(model, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if exclude {
			set[part] = true
		} else {
			delete(set, part)
		}
	}

	newExcluded := make([]string, 0, len(set))
	for k := range set {
		if k != "" {
			newExcluded = append(newExcluded, k)
		}
	}
	sort.Strings(newExcluded)
	dataMap["excluded_models"] = newExcluded

	data, err := json.MarshalIndent(dataMap, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')

	dir := filepath.Dir(targetPath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp := fmt.Sprintf("%s.tmp.%d", targetPath, time.Now().UnixNano())
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, targetPath); err != nil {
		_ = os.Remove(tmp)
		return err
	}

	// Invalidate cache
	excludedModelsMu.Lock()
	cachedExcludedRules = nil
	lastExcludedCheck = time.Time{}
	excludedModelsMu.Unlock()

	logMsg("updated excluded_models in %s: %v", targetPath, newExcluded)
	return nil
}

// firstPoolProbeAddr returns the host:port of the first proxy in the effective
// OpenCode pool (env override, settings file, or built-in defaults) so the
// telemetry pings the egress the plugin actually uses — never a hardcoded port.
// Returns "down" when no proxy is configured (direct egress).
func probeSidecar() (string, string, bool) {
	baseURL := getOpenCodeBaseURL()
	if baseURL == "" {
		// Native in-process transport: no sidecar is in the path at all, so a
		// silent :8789 must not be reported as an active bridge.
		return "native", "off", false
	}
	addr := "127.0.0.1:8789"
	if u, err := url.Parse(baseURL); err == nil && u.Host != "" {
		addr = u.Host
		if !strings.Contains(addr, ":") {
			if u.Scheme == "https" {
				addr += ":443"
			} else {
				addr += ":80"
			}
		}
	}
	ms := pingTCP(addr)
	return addr, ms, ms != "down"
}

func firstPoolProbeAddr() string {
	pool := proxyPool("OPENCODE_PROXY_POOL", defaultOpenCodeProxies)
	if len(pool) == 0 {
		return "down"
	}
	return pingTCP(pool[0].Host)
}

func pingTCP(addr string) string {
	start := time.Now()
	conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
	if err != nil {
		return "down"
	}
	conn.Close()
	return strconv.FormatInt(time.Since(start).Milliseconds(), 10)
}

func managementResponse(statusCode int, contentType string, body []byte) ([]byte, error) {
	resp := managementResponseEnvelope{
		StatusCode: statusCode,
		Headers: map[string][]string{
			"Content-Type":  {contentType},
			"Cache-Control": {"no-store, no-cache, must-revalidate"},
		},
		Body: body,
	}
	return ok(resp)
}

func jsonErrorResponse(statusCode int, code, message string) ([]byte, error) {
	errBytes, _ := json.Marshal(map[string]any{
		"error": map[string]any{
			"code":    code,
			"message": message,
		},
	})
	return managementResponse(statusCode, "application/json", errBytes)
}
