package main

// Live end-to-end probe of the real OpenCode free tier.
//
// The unit tests stub the socket, so they prove the byte shape but never prove
// the gate accepts it. This test goes through the production call path
// (harness injection, session shaping, header order, uTLS ClientHello) and
// reports what the upstream actually answered.
//
//	CPA_LIVE_E2E=1 go test -run TestOpenCodeLiveE2E -v
//	CPA_LIVE_E2E=1 CPA_LIVE_E2E_MODEL=mimo-v2.6-flash-free \
//	  CPA_LIVE_E2E_PROXY=http://127.0.0.1:8127 go test -run TestOpenCodeLiveE2E -v
//
// Set CPA_LIVE_E2E_STRICT=1 to fail the test unless the call is served 200.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

func TestOpenCodeLiveE2E(t *testing.T) {
	if os.Getenv("CPA_LIVE_E2E") == "" {
		t.Skip("set CPA_LIVE_E2E=1 to call the real upstream")
	}

	model := strings.TrimSpace(os.Getenv("CPA_LIVE_E2E_MODEL"))
	if model == "" {
		model = "big-pickle"
	}

	t.Setenv("CPA_NATIVE_TLS", "1")
	if proxy := strings.TrimSpace(os.Getenv("CPA_LIVE_E2E_PROXY")); proxy != "" {
		t.Setenv("OPENCODE_PROXY", proxy)
	}

	payload, err := json.Marshal(map[string]any{
		"model":      model,
		"max_tokens": 48,
		"stream":     false,
		"messages": []any{
			map[string]any{"role": "user", "content": "Reply with exactly: E2E_OK"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	started := time.Now()
	resp, err := callOpenCode(model, payload, false)
	elapsed := time.Since(started)
	if err != nil {
		t.Fatalf("callOpenCode(%s) after %s: %v", model, elapsed.Round(time.Millisecond), err)
	}

	raw, err := io.ReadAll(resp.body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	resp.body.Close()
	body := strings.TrimSpace(string(raw))
	t.Logf("model=%s status=%d elapsed=%s bodyLen=%d", model, resp.status, elapsed.Round(time.Millisecond), len(body))

	interesting := []string{"cf-ray", "cf-mitigated", "server", "retry-after", "x-opencode-session", "content-type"}
	for _, name := range interesting {
		if v := resp.header.Get(name); v != "" {
			t.Logf("header %s: %s", name, v)
		}
	}

	if len(body) > 1200 {
		body = body[:1200] + "…"
	}
	t.Logf("body: %s", body)

	var decoded map[string]any
	if json.Unmarshal([]byte(body), &decoded) == nil {
		if errObj, ok := decoded["error"].(map[string]any); ok {
			t.Logf("upstream error: %v", errObj["message"])
		}
		if choices, ok := decoded["choices"].([]any); ok && len(choices) > 0 {
			if choice, ok := choices[0].(map[string]any); ok {
				if msg, ok := choice["message"].(map[string]any); ok {
					t.Logf("content: %q", fmt.Sprint(msg["content"]))
				}
			}
		}
	}

	if os.Getenv("CPA_LIVE_E2E_STRICT") == "1" && resp.status != 200 {
		t.Fatalf("strict mode: upstream answered %d", resp.status)
	}
}

func TestClassifyOpenCode429Quota(t *testing.T) {
	hdr := http.Header{}
	hdr.Set("Retry-After", "33608")
	quota, retry, suffix := classifyOpenCode429(hdr, `{"type":"error","error":{"type":"FreeUsageLimitError","message":"Free usage limit reached."}}`)
	if !quota {
		t.Fatal("quota body must classify as quota")
	}
	if retry < time.Hour {
		t.Fatalf("retry duration must come from Retry-After, got %s", retry)
	}
	if !strings.Contains(suffix, "retry in") {
		t.Fatalf("suffix must carry the reset hint, got %q", suffix)
	}
}

func TestClassifyOpenCode429Fingerprint(t *testing.T) {
	hdr := http.Header{}
	hdr.Set("Retry-After", "60")
	if quota, _, _ := classifyOpenCode429(hdr, "<html>cloudflare challenge</html>"); quota {
		t.Fatal("html challenge must not classify as quota")
	}
	hdr.Set("CF-Mitigated", "challenge")
	if quota, _, _ := classifyOpenCode429(hdr, `{"error":{"type":"FreeUsageLimitError"}}`); quota {
		t.Fatal("cf-mitigated wins over the body wording")
	}
}
