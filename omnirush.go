package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	defaultOmniRushOrigin    = "https://omnirush.ai/omnirush"
	defaultOmniRushUserAgent = "omnirush"
	// Deprecated: kept only as a last-resort literal. The advertised version is
	// resolved live via omniRushClientHeader() (env/settings override + the
	// omniRushCLIVersionFallback constant), so bumping the CLI version no longer
	// means editing this string. See omnirush_identity.go.
	defaultOmniRushClientVer = "cli/2.7.8"
)

type OmniRushAuth struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	GatewayURL   string `json:"gatewayUrl,omitempty"`
	SavedAt      string `json:"savedAt,omitempty"`
	Email        string `json:"email,omitempty"`
}

type omniDeviceAuthorizeResponse struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	ExpiresIn               int    `json:"expires_in"`
	Interval                int    `json:"interval"`
}

type omniDeviceTokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	GatewayURL   string `json:"gateway_url,omitempty"`
	Detail       string `json:"detail,omitempty"`
	Error        string `json:"error,omitempty"`
}

type omniDeviceMeResponse struct {
	Email string `json:"email"`
	Name  string `json:"name,omitempty"`
	ID    string `json:"id,omitempty"`
}

var (
	omniAuthMu        sync.RWMutex
	cachedOmniAuth    *OmniRushAuth
	lastOmniAuthCheck time.Time
	omniRefreshMu     sync.Mutex
)

// omniRushDeviceOrigin derives the host root for the device-flow endpoints.
// The login response hands back gatewayUrl pointing at the API base
// (https://omnirush.ai/omnirush/v1), while /device/* lives one level up. Calling
// /v1/device/refresh answers 401 device_token_required, which is easy to
// misread as an expired refresh token.
func omniRushDeviceOrigin(gatewayURL string) string {
	origin := strings.TrimSpace(gatewayURL)
	if origin == "" {
		return defaultOmniRushOrigin
	}
	origin = strings.TrimRight(origin, "/")
	if i := strings.Index(origin, "/v1"); i > 0 {
		origin = origin[:i]
	}
	origin = strings.TrimRight(origin, "/")
	if origin == "" {
		return defaultOmniRushOrigin
	}
	return origin
}

func getOmniRushAuthPath() string {
	if p := strings.TrimSpace(os.Getenv("OMNIRUSH_AUTH_FILE")); p != "" {
		return p
	}
	if d := strings.TrimSpace(os.Getenv("OMNIRUSH_DIR")); d != "" {
		return filepath.Join(d, "auth.json")
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".omnirush", "auth.json")
	}
	return "auth.json"
}

func getOmniRushFallbackAuthPath() string {
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".cli-proxy-api", "omnirush-auth.json")
	}
	return ""
}

func loadOmniRushAuth() (*OmniRushAuth, string, error) {
	omniAuthMu.RLock()
	if time.Since(lastOmniAuthCheck) < 2*time.Second && cachedOmniAuth != nil {
		auth := cachedOmniAuth
		omniAuthMu.RUnlock()
		return auth, getOmniRushAuthPath(), nil
	}
	omniAuthMu.RUnlock()

	omniAuthMu.Lock()
	defer omniAuthMu.Unlock()

	// 1. Environment variable override
	if token := strings.TrimSpace(os.Getenv("OMNIRUSH_ACCESS_TOKEN")); token != "" {
		refresh := strings.TrimSpace(os.Getenv("OMNIRUSH_REFRESH_TOKEN"))
		auth := &OmniRushAuth{
			AccessToken:  token,
			RefreshToken: refresh,
			GatewayURL:   defaultOmniRushOrigin,
			SavedAt:      time.Now().UTC().Format(time.RFC3339),
			Email:        "env-configured",
		}
		cachedOmniAuth = auth
		lastOmniAuthCheck = time.Now()
		return auth, "env", nil
	}

	if store, errStore := loadOmniAccounts(); errStore == nil {
		for _, acct := range store.Accounts {
			if acct == nil || acct.AccessToken == "" {
				continue
			}
			auth := accountToAuth(acct)
			cachedOmniAuth = auth
			lastOmniAuthCheck = time.Now()
			source := acct.LegacyPath
			if source == "" {
				source = omniAccountsPath()
			}
			return auth, source, nil
		}
	}

	primaryPath := getOmniRushAuthPath()
	paths := []string{primaryPath}
	if fb := getOmniRushFallbackAuthPath(); fb != "" && fb != primaryPath {
		paths = append(paths, fb)
	}

	for _, p := range paths {
		auth, _, err := loadOmniRushAuthFromFile(p)
		if err != nil || auth == nil {
			continue
		}
		cachedOmniAuth = auth
		lastOmniAuthCheck = time.Now()
		return auth, p, nil
	}

	cachedOmniAuth = nil
	lastOmniAuthCheck = time.Now()
	return nil, primaryPath, errors.New("no omnirush credentials found")
}

// loadOmniRushAuthFromFile parses one credential file, accepting both the
// camelCase shape this plugin writes and the snake_case / nested shapes other
// clients leave behind.
func loadOmniRushAuthFromFile(path string) (*OmniRushAuth, string, error) {
	{
		p := path
		data, err := os.ReadFile(p)
		if err != nil || len(data) == 0 {
			return nil, p, errors.New("no credentials file")
		}
		var raw map[string]any
		if err := json.Unmarshal(data, &raw); err != nil {
			return nil, p, err
		}

		auth := &OmniRushAuth{}
		if at, ok := raw["accessToken"].(string); ok && at != "" {
			auth.AccessToken = at
		} else if at, ok := raw["access_token"].(string); ok && at != "" {
			auth.AccessToken = at
		}

		if rt, ok := raw["refreshToken"].(string); ok && rt != "" {
			auth.RefreshToken = rt
		} else if rt, ok := raw["refresh_token"].(string); ok && rt != "" {
			auth.RefreshToken = rt
		}

		// Support nested format from some CLI configs: {"tokens": {"idToken": "...", "refreshToken": "..."}}
		if auth.AccessToken == "" {
			if tokens, ok := raw["tokens"].(map[string]any); ok {
				if idToken, ok := tokens["idToken"].(string); ok {
					auth.AccessToken = idToken
				}
				if refToken, ok := tokens["refreshToken"].(string); ok {
					auth.RefreshToken = refToken
				}
			}
		}

		if gw, ok := raw["gatewayUrl"].(string); ok && gw != "" {
			auth.GatewayURL = gw
		} else if gw, ok := raw["gateway_url"].(string); ok && gw != "" {
			auth.GatewayURL = gw
		} else {
			auth.GatewayURL = defaultOmniRushOrigin
		}

		if sa, ok := raw["savedAt"].(string); ok {
			auth.SavedAt = sa
		}
		if em, ok := raw["email"].(string); ok {
			auth.Email = em
		}

		if auth.AccessToken != "" || auth.RefreshToken != "" {
			return auth, p, nil
		}
		return nil, p, errors.New("credentials file has no usable token")
	}
}

// saveOmniRushAuth stores a freshly issued pair in the account pool and mirrors
// it to the CLI credential file so both stay on the same token.
func saveOmniRushAuth(auth *OmniRushAuth) error {
	if _, err := upsertOmniAccount(auth, getOmniRushAuthPath()); err != nil {
		return err
	}
	if err := saveOmniRushAuthTo(auth, getOmniRushAuthPath()); err != nil {
		logMsg("omnirush: account stored but CLI mirror failed: %v", err)
	}
	return nil
}

func saveOmniRushAuthTo(auth *OmniRushAuth, targetPath string) error {
	if auth == nil || (auth.AccessToken == "" && auth.RefreshToken == "") {
		return errors.New("refusing to store empty omnirush credentials")
	}
	if strings.TrimSpace(targetPath) == "" || targetPath == "env" {
		targetPath = getOmniRushAuthPath()
	}
	dir := filepath.Dir(targetPath)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("failed to create omnirush dir: %w", err)
	}

	if auth.SavedAt == "" {
		auth.SavedAt = time.Now().UTC().Format(time.RFC3339)
	}
	if auth.GatewayURL == "" {
		auth.GatewayURL = defaultOmniRushOrigin
	}

	payload, err := json.MarshalIndent(auth, "", "  ")
	if err != nil {
		return err
	}
	payload = append(payload, '\n')

	// Atomic write
	tmpFile := fmt.Sprintf("%s.tmp.%d", targetPath, time.Now().UnixNano())
	if err := os.WriteFile(tmpFile, payload, 0600); err != nil {
		return fmt.Errorf("failed to write tmp auth file: %w", err)
	}
	if err := os.Rename(tmpFile, targetPath); err != nil {
		_ = os.Remove(tmpFile)
		return fmt.Errorf("failed to rename auth file: %w", err)
	}

	omniAuthMu.Lock()
	cachedOmniAuth = auth
	lastOmniAuthCheck = time.Now()
	omniAuthMu.Unlock()

	logMsg("saved omnirush auth to %s", targetPath)
	return nil
}

func clearOmniRushAuth() error {
	// Signing out means "forget every account", including the pool entries.
	resetOmniRushSession()
	if err := saveOmniAccounts(&omniAccountStore{Version: omniAccountStoreVersion}); err != nil {
		logMsg("omnirush: failed to clear account pool: %v", err)
	}
	invalidateOmniStore()

	targetPath := getOmniRushAuthPath()
	omniAuthMu.Lock()
	cachedOmniAuth = nil
	lastOmniAuthCheck = time.Time{}
	omniAuthMu.Unlock()

	var errs []string
	if err := os.Remove(targetPath); err != nil && !os.IsNotExist(err) {
		errs = append(errs, err.Error())
	}
	if fb := getOmniRushFallbackAuthPath(); fb != "" {
		if err := os.Remove(fb); err != nil && !os.IsNotExist(err) {
			errs = append(errs, err.Error())
		}
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

func getOmniRushHTTPClient() *http.Client {
	// Dashboard setting, then OMNIRUSH_PROXY, then the ambient network. OmniRush
	// resets datacenter egress, so the proxy usually has to be explicit.
	if pu, handled := settingsOmniRushProxy(); handled {
		if pu == nil {
			return &http.Client{Transport: http.DefaultTransport, Timeout: 45 * time.Second}
		}
		return &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(pu)}, Timeout: 45 * time.Second}
	}
	if proxyURLStr := strings.TrimSpace(os.Getenv("OMNIRUSH_PROXY")); proxyURLStr != "" {
		if pu, err := url.Parse(proxyURLStr); err == nil {
			return &http.Client{
				Transport: &http.Transport{
					Proxy: http.ProxyURL(pu),
				},
				Timeout: 45 * time.Second,
			}
		}
	}
	return &http.Client{
		Transport: http.DefaultTransport,
		Timeout:   45 * time.Second,
	}
}

// omniRushAPIClient is the egress used for gateway traffic (chat/responses and
// catalog). The dashboard setting wins, then OMNIRUSH_PROXY, then the ambient
// network — the same order the device flow uses.
func omniRushAPIClient() *http.Client {
	timeout := settingsOmniRushTimeout()
	if pu, handled := settingsOmniRushProxy(); handled {
		if pu == nil {
			return &http.Client{Transport: getTransportWithTimeout("omnirush|env|"+timeout.String(), nil, timeout)}
		}
		return &http.Client{Transport: getTransportWithTimeout("omnirush|"+pu.String()+"|"+timeout.String(), pu, timeout)}
	}
	if raw := strings.TrimSpace(os.Getenv("OMNIRUSH_PROXY")); raw != "" {
		if !strings.Contains(raw, "://") {
			raw = "http://" + raw
		}
		if pu, err := url.Parse(raw); err == nil && validProxyTarget(pu) {
			return &http.Client{Transport: getTransportWithTimeout("omnirush|"+pu.String()+"|"+timeout.String(), pu, timeout)}
		}
	}
	return &http.Client{Transport: getTransportWithTimeout("omnirush|env|"+timeout.String(), nil, timeout)}
}

// describeOmniRushTransportError turns an opaque read timeout into something the
// caller can act on: the free tier queues generations while the account is busy,
// so "no answer" usually means "wait or use another model", not "broken".
func describeOmniRushTransportError(err error, timeout time.Duration) error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) && urlErr.Timeout() {
		return fmt.Errorf("omnirush upstream did not start responding within %s (free-tier queue busy - retry shortly or pick another model)", timeout)
	}
	return fmt.Errorf("omnirush upstream request error: %w", err)
}

func omniRushDeviceAuthorize() (*omniDeviceAuthorizeResponse, error) {
	client := getOmniRushHTTPClient()
	body := map[string]string{
		"device_name": "cpa-freetier-plugin",
		"platform":    "linux",
	}
	bodyBytes, _ := json.Marshal(body)

	endpoint := defaultOmniRushOrigin + "/device/authorize"
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, endpoint, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", defaultOmniRushUserAgent)
	req.Header.Set("X-OmniRush-Client", omniRushClientHeader())

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("device authorize request failed: %w", err)
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading authorize response failed: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("device authorize returned HTTP %d: %s", resp.StatusCode, string(respBytes))
	}

	var authResp omniDeviceAuthorizeResponse
	if err := json.Unmarshal(respBytes, &authResp); err != nil {
		return nil, fmt.Errorf("parsing authorize response failed: %w", err)
	}
	return &authResp, nil
}

func omniRushDevicePoll(deviceCode string) (status string, email string, err error) {
	if strings.TrimSpace(deviceCode) == "" {
		return "error", "", errors.New("device_code is required")
	}

	client := getOmniRushHTTPClient()
	body := map[string]string{
		"device_code": deviceCode,
	}
	bodyBytes, _ := json.Marshal(body)

	endpoint := defaultOmniRushOrigin + "/device/token"
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, endpoint, bytes.NewReader(bodyBytes))
	if err != nil {
		return "error", "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", defaultOmniRushUserAgent)
	req.Header.Set("X-OmniRush-Client", omniRushClientHeader())

	resp, err := client.Do(req)
	if err != nil {
		return "error", "", fmt.Errorf("token poll request failed: %w", err)
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return "error", "", fmt.Errorf("reading token response failed: %w", err)
	}

	if resp.StatusCode == 428 || strings.Contains(string(respBytes), "authorization_pending") {
		return "pending", "", nil
	}

	if resp.StatusCode == 400 && strings.Contains(string(respBytes), "expired") {
		return "expired", "", errors.New("device code expired")
	}

	if resp.StatusCode != http.StatusOK {
		return "error", "", fmt.Errorf("token poll returned HTTP %d: %s", resp.StatusCode, string(respBytes))
	}

	var tokResp omniDeviceTokenResponse
	if err := json.Unmarshal(respBytes, &tokResp); err != nil {
		return "error", "", fmt.Errorf("parsing token response failed: %w", err)
	}

	if tokResp.AccessToken == "" || tokResp.RefreshToken == "" {
		return "error", "", errors.New("token response missing access_token or refresh_token")
	}

	auth := &OmniRushAuth{
		AccessToken:  tokResp.AccessToken,
		RefreshToken: tokResp.RefreshToken,
		GatewayURL:   tokResp.GatewayURL,
		SavedAt:      time.Now().UTC().Format(time.RFC3339),
	}
	if auth.GatewayURL == "" {
		auth.GatewayURL = defaultOmniRushOrigin
	}

	// Try fetching user email from /device/me
	meEmail := fetchOmniRushUserEmail(auth.AccessToken, auth.GatewayURL)
	if meEmail != "" {
		auth.Email = meEmail
	}

	if err := saveOmniRushAuth(auth); err != nil {
		return "error", "", fmt.Errorf("failed to save tokens: %w", err)
	}

	return "approved", auth.Email, nil
}

func fetchOmniRushUserEmail(accessToken, origin string) string {
	if accessToken == "" {
		return ""
	}
	origin = omniRushDeviceOrigin(origin)
	client := getOmniRushHTTPClient()
	endpoint := origin + "/device/me"
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, endpoint, nil)
	if err != nil {
		return ""
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("User-Agent", defaultOmniRushUserAgent)
	req.Header.Set("X-OmniRush-Client", omniRushClientHeader())

	resp, err := client.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return ""
	}

	var me omniDeviceMeResponse
	if err := json.NewDecoder(resp.Body).Decode(&me); err == nil {
		return me.Email
	}
	return ""
}

// refreshOmniRushToken rotates the token pair of the account that is in use.
// staleAccess is the access token that just failed; if this account was already
// rotated by another goroutine, the fresh pair is returned untouched, because a
// refresh token can only be spent once.
func refreshOmniRushToken(staleAccess string) (*OmniRushAuth, error) {
	acct, err := selectOmniAccount(nil)
	if err != nil {
		return nil, err
	}
	return refreshOmniAccountToken(acct, staleAccess)
}

func findOmniAccount(id string) (*OmniRushAccount, error) {
	store, err := loadOmniAccounts()
	if err != nil {
		return nil, err
	}
	for _, acct := range store.Accounts {
		if acct.ID == id || strings.EqualFold(acct.Email, id) {
			copied := *acct
			return &copied, nil
		}
	}
	return nil, fmt.Errorf("unknown omnirush account %q", id)
}

func refreshOmniAccountToken(acct *OmniRushAccount, staleAccess string) (*OmniRushAuth, error) {
	if acct == nil {
		return nil, errors.New("no omnirush account to refresh")
	}
	// One mutex per account: rotating account A must not block account B.
	lock := omniAccountRefreshLock(acct.ID)
	lock.Lock()
	defer lock.Unlock()

	fresh, err := findOmniAccount(acct.ID)
	if err != nil {
		return nil, err
	}
	if fresh.RefreshToken == "" {
		return nil, errors.New("no omnirush refresh_token available")
	}
	if staleAccess != "" && fresh.AccessToken != "" && fresh.AccessToken != staleAccess {
		// Another caller rotated this account while we waited on its mutex.
		return accountToAuth(fresh), nil
	}

	client := getOmniRushHTTPClient()
	body := map[string]string{
		"refresh_token": fresh.RefreshToken,
	}
	bodyBytes, _ := json.Marshal(body)

	origin := omniRushDeviceOrigin(fresh.GatewayURL)
	endpoint := origin + "/device/refresh"

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, endpoint, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", defaultOmniRushUserAgent)
	req.Header.Set("X-OmniRush-Client", omniRushClientHeader())

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("omnirush refresh request failed: %w", err)
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading refresh response failed: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode == http.StatusUnauthorized {
			penalizeOmniAccount(fresh.ID, "refresh token expired; re-authentication required", 10*time.Minute)
			return nil, errors.New("omnirush refresh token expired or invalid; re-authentication required")
		}
		if resp.StatusCode == http.StatusConflict {
			// 409 refresh_token_already_used: a co-installed client (the OmniRush
			// CLI) spent this token first. Adopt whatever it stored.
			invalidateOmniStore()
			if latest, _, errLatest := loadOmniRushAuthFromFile(legacyOrStorePath(fresh)); errLatest == nil && latest != nil && latest.AccessToken != "" && latest.AccessToken != staleAccess {
				if _, errStore := upsertOmniAccount(latest, fresh.LegacyPath); errStore == nil {
					return latest, nil
				}
			}
			if latest, errFind := findOmniAccount(fresh.ID); errFind == nil && latest.AccessToken != "" && latest.AccessToken != staleAccess {
				return accountToAuth(latest), nil
			}
			return nil, errors.New("omnirush refresh token was already used by another client; sign in again")
		}
		return nil, fmt.Errorf("omnirush refresh returned HTTP %d: %s", resp.StatusCode, string(respBytes))
	}

	var tokResp omniDeviceTokenResponse
	if err := json.Unmarshal(respBytes, &tokResp); err != nil {
		return nil, fmt.Errorf("parsing refresh response failed: %w", err)
	}
	if tokResp.AccessToken == "" || tokResp.RefreshToken == "" {
		return nil, errors.New("omnirush refresh response missing access_token or refresh_token")
	}

	gateway := tokResp.GatewayURL
	if gateway == "" {
		gateway = fresh.GatewayURL
	}
	newAuth := &OmniRushAuth{
		AccessToken:  tokResp.AccessToken,
		RefreshToken: tokResp.RefreshToken,
		GatewayURL:   gateway,
		SavedAt:      time.Now().UTC().Format(time.RFC3339),
		Email:        fresh.Email,
	}

	if err := updateOmniAccount(fresh.ID, func(a *OmniRushAccount) {
		a.AccessToken = newAuth.AccessToken
		a.RefreshToken = newAuth.RefreshToken
		a.GatewayURL = newAuth.GatewayURL
		a.SavedAt = newAuth.SavedAt
		a.LastError = ""
		a.CooldownUntil = ""
	}); err != nil {
		return nil, fmt.Errorf("failed to persist rotated omnirush tokens: %w", err)
	}

	// Keep the CLI mirror on the same pair so the two never fight over rotation.
	if fresh.LegacyPath != "" {
		if err := saveOmniRushAuthTo(newAuth, fresh.LegacyPath); err != nil {
			logMsg("omnirush: rotated but CLI mirror failed: %v", err)
		}
	}
	logMsg("rotated omnirush tokens for %s", fresh.label())
	return newAuth, nil
}

// legacyOrStorePath prefers the CLI mirror a migration recorded, falling back to
// the conventional location so a co-installed client's tokens can be adopted.
func legacyOrStorePath(acct *OmniRushAccount) string {
	if acct.LegacyPath != "" {
		return acct.LegacyPath
	}
	return getOmniRushAuthPath()
}

func callOmniRush(model string, payload []byte, stream bool) (*upstreamResponse, error) {
	cleanModel := strings.TrimPrefix(model, "omnirush/")

	responsesPayload, err := buildOmniRushResponsesPayload(payload, cleanModel)
	if err != nil {
		return nil, err
	}

	tried := map[string]bool{}
	var lastErr error
	timeout := settingsOmniRushTimeout()

	for attempt := 0; attempt < omniAccountMaxTries; attempt++ {
		acct, errPick := selectOmniAccount(tried)
		if errPick != nil {
			if lastErr != nil {
				break
			}
			return nil, fmt.Errorf("omnirush authentication required: %w (sign in from the plugin dashboard or run omnirush login)", errPick)
		}
		tried[acct.ID] = true

		targetURL := omniRushGatewayBase(accountToAuth(acct)) + "/responses"
		logMsg("callOmniRush: account=%s model=%s targetURL=%s stream=%v attempt=%d",
			acct.label(), cleanModel, targetURL, stream, attempt+1)

		resp, errDo := doOmniRushRequest(acct.AccessToken, targetURL, responsesPayload)
		if errDo != nil {
			lastErr = describeOmniRushTransportError(errDo, timeout)
			penalizeOmniAccount(acct.ID, "no response: "+errDo.Error(), omniAccountCooldown)
			logMsg("callOmniRush: account=%s failed (%v), trying next", acct.label(), errDo)
			continue
		}

		// Expired access token: rotate this account once, then retry it.
		if resp.StatusCode == http.StatusUnauthorized && acct.RefreshToken != "" {
			resp.Body.Close()
			logMsg("callOmniRush: account=%s 401, refreshing token", acct.label())
			refreshed, errRef := refreshOmniAccountToken(acct, acct.AccessToken)
			if errRef != nil {
				lastErr = fmt.Errorf("omnirush 401 and token refresh failed: %w", errRef)
				penalizeOmniAccount(acct.ID, errRef.Error(), 5*time.Minute)
				continue
			}
			resp, errDo = doOmniRushRequest(refreshed.AccessToken, targetURL, responsesPayload)
			if errDo != nil {
				lastErr = describeOmniRushTransportError(errDo, timeout)
				penalizeOmniAccount(acct.ID, "retry failed: "+errDo.Error(), omniAccountCooldown)
				continue
			}
		}

		// Persistent 401: token expired or revoked and could not be refreshed.
		// Bench this account and fail over to the next healthy identity.
		if resp.StatusCode == http.StatusUnauthorized {
			bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			detail := omniRushErrorDetail(bodyBytes)
			lastErr = fmt.Errorf("omnirush upstream error HTTP 401: %s", detail)
			penalizeOmniAccount(acct.ID, detail, 5*time.Minute)
			logMsg("callOmniRush: account=%s persistent 401, trying next", acct.label())
			continue
		}

		// Quota and auth rejections are per account, so route around them.
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusPaymentRequired || resp.StatusCode == http.StatusForbidden {
			bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			detail := omniRushErrorDetail(bodyBytes)
			lastErr = fmt.Errorf("omnirush upstream error HTTP %d: %s", resp.StatusCode, detail)
			penalizeOmniAccount(acct.ID, detail, 2*omniAccountCooldown)
			logMsg("callOmniRush: account=%s throttled HTTP %d, trying next", acct.label(), resp.StatusCode)
			continue
		}

		if resp.StatusCode >= 500 {
			bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			lastErr = fmt.Errorf("omnirush upstream error HTTP %d: %s", resp.StatusCode, omniRushErrorDetail(bodyBytes))
			penalizeOmniAccount(acct.ID, "upstream 5xx", omniAccountCooldown)
			continue
		}

		if resp.StatusCode >= 400 {
			// A 4xx that is not quota-related means our payload is wrong; another
			// account would fail identically.
			bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
			resp.Body.Close()
			detail := omniRushErrorDetail(bodyBytes)
			logMsg("callOmniRush error HTTP %d: %s", resp.StatusCode, detail)
			return nil, fmt.Errorf("omnirush upstream error HTTP %d: %s", resp.StatusCode, detail)
		}

		touchOmniAccount(acct.ID)
		header := resp.Header.Clone()
		header.Set("Content-Type", "text/event-stream")
		return &upstreamResponse{
			status: resp.StatusCode,
			header: header,
			body:   io.NopCloser(translateOmniRushStream(resp.Body, cleanModel)),
		}, nil
	}

	if lastErr == nil {
		lastErr = errors.New("no omnirush account could serve the request")
	}
	return nil, lastErr
}

func doOmniRushRequest(token, targetURL string, payload []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, targetURL, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	applyOmniRushHeaders(req, token)
	// The gateway streams only; the body is translated back to Chat Completions
	// chunks below, so callers see the format they asked for.
	req.Header.Set("Accept", "text/event-stream")
	return omniRushAPIClient().Do(req)
}

// omniRushErrorDetail renders the gateway's JSON error in one readable line.
func omniRushErrorDetail(raw []byte) string {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return "empty error body"
	}
	var payload struct {
		Detail any `json:"detail"`
		Error  any `json:"error"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return trimmed
	}
	if s, ok := payload.Detail.(string); ok && s != "" {
		return s
	}
	if obj, ok := payload.Error.(map[string]any); ok {
		code, _ := obj["code"].(string)
		msg, _ := obj["message"].(string)
		switch {
		case code != "" && msg != "":
			return code + ": " + msg
		case msg != "":
			return msg
		case code != "":
			return code
		}
	}
	return trimmed
}
