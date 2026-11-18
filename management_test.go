package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// tokenHeaders is what the served dashboard sends on mutating calls.
func tokenHeaders() map[string][]string {
	return map[string][]string{dashboardTokenHeader: {dashboardToken()}}
}

func TestManagementRegisterReturnsExpectedRoutesAndResources(t *testing.T) {
	raw, err := handleManagementRegister(nil)
	if err != nil {
		t.Fatalf("handleManagementRegister error: %v", err)
	}

	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("failed to parse envelope: %v", err)
	}
	if !env.OK {
		t.Fatalf("expected envelope OK=true, got error: %+v", env.Error)
	}

	var resp managementRegistrationResponse
	if err := json.Unmarshal(env.Result, &resp); err != nil {
		t.Fatalf("failed to parse registration response: %v", err)
	}

	if len(resp.Resources) == 0 {
		t.Fatal("expected at least one resource route")
	}

	foundDashboard := false
	for _, r := range resp.Resources {
		if r.Path == "/dashboard" && r.Menu != "" {
			foundDashboard = true
			// The host HTML-escapes the label, so an ampersand would surface in the
			// sidebar as a literal "&amp;". Keep the label plain text.
			if r.Menu != "Free Tier Plugin" {
				t.Errorf("expected menu label %q, got %q", "Free Tier Plugin", r.Menu)
			}
			if strings.ContainsAny(r.Menu, "&<>") {
				t.Errorf("menu label must not contain HTML special characters, got %q", r.Menu)
			}
			break
		}
	}
	if !foundDashboard {
		t.Error("expected /dashboard resource route with non-empty Menu label")
	}
}

func TestManagementHandleDashboardHTML(t *testing.T) {
	req := managementRequestEnvelope{
		Method: "GET",
		Path:   "/v0/resource/plugins/cpa-freetier-plugin/dashboard",
	}
	reqBytes, _ := json.Marshal(req)

	raw, err := handleManagementHandle(reqBytes)
	if err != nil {
		t.Fatalf("handleManagementHandle error: %v", err)
	}

	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("failed to parse envelope: %v", err)
	}
	if !env.OK {
		t.Fatalf("expected OK=true, got %+v", env.Error)
	}

	var mResp managementResponseEnvelope
	if err := json.Unmarshal(env.Result, &mResp); err != nil {
		t.Fatalf("failed to parse management response: %v", err)
	}

	if mResp.StatusCode != 200 {
		t.Errorf("expected status 200, got %d", mResp.StatusCode)
	}
	ct := mResp.Headers["Content-Type"]
	if len(ct) == 0 || !strings.Contains(ct[0], "text/html") {
		t.Errorf("expected Content-Type text/html, got %v", ct)
	}
	if !strings.Contains(string(mResp.Body), "Free Tier Plugin") {
		t.Errorf("expected HTML body to contain title, got: %s", string(mResp.Body[:min(200, len(mResp.Body))]))
	}
	if strings.Contains(string(mResp.Body), dashboardTokenPlaceholder) {
		t.Error("expected the dashboard token placeholder to be replaced in the served page")
	}
	if !strings.Contains(string(mResp.Body), dashboardToken()) {
		t.Error("expected the dashboard token to be embedded in the served page")
	}
}

func TestManagementHandleStatusAPI(t *testing.T) {
	req := managementRequestEnvelope{
		Method: "GET",
		Path:   "/v0/resource/plugins/cpa-freetier-plugin/api/status",
	}
	reqBytes, _ := json.Marshal(req)

	raw, err := handleManagementHandle(reqBytes)
	if err != nil {
		t.Fatalf("handleManagementHandle error: %v", err)
	}

	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("failed to parse envelope: %v", err)
	}

	var mResp managementResponseEnvelope
	if err := json.Unmarshal(env.Result, &mResp); err != nil {
		t.Fatalf("failed to parse management response: %v", err)
	}

	if mResp.StatusCode != 200 {
		t.Errorf("expected status 200, got %d", mResp.StatusCode)
	}

	var statusMap map[string]any
	if err := json.Unmarshal(mResp.Body, &statusMap); err != nil {
		t.Fatalf("failed to parse status JSON: %v", err)
	}

	models, ok := statusMap["models"].([]any)
	if !ok {
		t.Fatalf("expected models array in status API")
	}
	// Curated OpenCode/Kilo models plus whatever the OmniRush catalog serves
	// (the fallback set when no credentials are configured in tests).
	minModels := len(curatedModels()) + len(omniRushFallbackModels)
	if len(models) < minModels {
		t.Errorf("expected at least %d models in status API, got %d", minModels, len(models))
	}
}

func TestManagementHandleModelToggle(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("CLI_PROXY_AUTH_DIR", tmpDir)
	t.Setenv("HOME", tmpDir)

	// Toggle exclude gpt-6-sol
	req := managementRequestEnvelope{
		Method: "GET",
		Path:   "/v0/resource/plugins/cpa-freetier-plugin/api/models/toggle",
		Query: map[string][]string{
			"model":   {"gpt-6-sol"},
			"enabled": {"0"},
		},
		Headers: tokenHeaders(),
	}
	reqBytes, _ := json.Marshal(req)
	raw, err := handleManagementHandle(reqBytes)
	if err != nil {
		t.Fatalf("handleManagementHandle toggle error: %v", err)
	}

	var env envelope
	_ = json.Unmarshal(raw, &env)
	if !env.OK {
		t.Fatalf("toggle failed: %+v", env.Error)
	}

	if !isModelExcluded("gpt-6-sol") {
		t.Error("expected gpt-6-sol to be excluded after toggle")
	}

	// Toggle re-enable gpt-6-sol
	req2 := managementRequestEnvelope{
		Method: "GET",
		Path:   "/v0/resource/plugins/cpa-freetier-plugin/api/models/toggle",
		Query: map[string][]string{
			"model":   {"gpt-6-sol"},
			"enabled": {"1"},
		},
		Headers: tokenHeaders(),
	}
	reqBytes2, _ := json.Marshal(req2)
	raw2, _ := handleManagementHandle(reqBytes2)
	var env2 envelope
	_ = json.Unmarshal(raw2, &env2)
	if !env2.OK {
		t.Fatalf("re-enable failed: %+v", env2.Error)
	}

	if isModelExcluded("gpt-6-sol") {
		t.Error("expected gpt-6-sol to be enabled after toggle re-enable")
	}
}
