package main

// Live diagnostic of the raw CLI transport: shows the unmodified upstream
// status, headers and body so a 429 can be classified (Cloudflare fingerprint
// block vs. per-IP free-quota exhaustion) instead of guessed at.
//
//	CPA_LIVE_RAW=1 go test -run TestOpenCodeLiveRaw -v

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

func TestOpenCodeLiveRaw(t *testing.T) {
	if os.Getenv("CPA_LIVE_RAW") == "" {
		t.Skip("set CPA_LIVE_RAW=1 to hit the real upstream")
	}

	model := os.Getenv("CPA_LIVE_E2E_MODEL")
	if model == "" {
		model = "big-pickle"
	}

	payload, err := json.Marshal(map[string]any{
		"model":      model,
		"max_tokens": 32,
		"stream":     false,
		"messages": []any{
			map[string]any{"role": "user", "content": "hi"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	b3, tp := generateTraceHeaders()
	sessionID := genSessionID()
	hdr := http.Header{}
	hdr.Set("Content-Type", "application/json")
	hdr.Set("Authorization", "Bearer public")
	hdr.Set("User-Agent", opencodeClientUA)
	hdr.Set("x-opencode-client", "cli")
	hdr.Set("x-opencode-project", getProjectHash())
	hdr.Set("x-opencode-session-id", sessionID)
	hdr.Set("x-opencode-session", sessionID)
	hdr.Set("x-opencode-request", genRequestID())
	hdr.Set("x-session-affinity", sessionID)
	hdr.Set("x-session-id", sessionID)
	hdr.Set("X-Session-Id", sessionID)
	hdr.Set("b3", b3)
	hdr.Set("traceparent", tp)
	hdr.Set("Connection", "keep-alive")
	hdr.Set("Accept", "*/*")
	hdr.Set("Host", "opencode.ai")
	hdr.Set("Accept-Encoding", "gzip, deflate, br, zstd")

	if p := strings.TrimSpace(os.Getenv("CPA_LIVE_E2E_PROXY")); p != "" {
		t.Setenv("OPENCODE_PROXY", p)
	}
	parsedProxy := getOpenCodeProxy()
	if parsedProxy != nil {
		t.Logf("proxy: %s", parsedProxy)
	}

	started := time.Now()
	res, err := openCodeRawDo(context.Background(), opencodeChatURL, hdr, payload, parsedProxy, 15*time.Second)
	if err != nil {
		t.Fatalf("transport: %v", err)
	}
	defer res.body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.body, 4096))

	t.Logf("status=%d elapsed=%s", res.status, time.Since(started).Round(time.Millisecond))
	for _, h := range []string{"cf-ray", "cf-mitigated", "server", "retry-after", "content-type", "x-opencode-session"} {
		if v := res.header.Get(h); v != "" {
			t.Logf("header %s: %s", h, v)
		}
	}
	t.Logf("body: %s", strings.TrimSpace(string(raw)))
}
