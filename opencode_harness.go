package main

import (
	"crypto/rand"
	"crypto/sha1"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

//go:embed agent_preamble.txt
var openCodeAgentPreambleRaw string

var openCodeAgentPreamble = strings.ReplaceAll(openCodeAgentPreambleRaw, "\r\n", "\n")

//go:embed agent_tools.json
var openCodeGateToolsJSON []byte

var (
	officialGateTools []map[string]any
	traceRoot         string
	traceRootOnce     sync.Once

	sessionMu      sync.Mutex
	lastSessionTs  int64
	sessionCounter int64
	cachedSessions = make(map[string]sessionEntry)
)

const (
	openCodePreambleMarker = "You are an AI agent running in OpenCode"
	sessionTTL             = 30 * time.Minute
	sessionPrefixMask      = (uint64(1) << 48) - 1
	base62Chars            = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
)

type sessionEntry struct {
	sessionID string
	lastUsed  time.Time
}

var openCodeFallbackModels = []string{
	"mimo-v2.6-flash-free",
	"big-pickle",
	"nemotron-3.5-lightning-free",
	"deepseek-v4.1-flash-free",
	"nemotron-3.5-super-vl-free",
	"glm-5.3-flash-free",
	"beam-free",
	"flint-40b-free",
}

func init() {
	if len(openCodeGateToolsJSON) > 0 {
		if err := json.Unmarshal(openCodeGateToolsJSON, &officialGateTools); err != nil {
			panic(fmt.Sprintf("failed to parse embedded agent_tools.json: %v", err))
		}
	}
}

func deepCopyMap(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	cp := make(map[string]any, len(m))
	for k, v := range m {
		switch val := v.(type) {
		case map[string]any:
			cp[k] = deepCopyMap(val)
		case []any:
			cp[k] = deepCopySlice(val)
		default:
			cp[k] = val
		}
	}
	return cp
}

func deepCopySlice(s []any) []any {
	if s == nil {
		return nil
	}
	cp := make([]any, len(s))
	for i, v := range s {
		switch val := v.(type) {
		case map[string]any:
			cp[i] = deepCopyMap(val)
		case []any:
			cp[i] = deepCopySlice(val)
		default:
			cp[i] = val
		}
	}
	return cp
}

func getOfficialGateTools() []map[string]any {
	tools := make([]map[string]any, len(officialGateTools))
	for i, t := range officialGateTools {
		tools[i] = deepCopyMap(t)
	}
	return tools
}

// injectOpenCodeHarness injects the OpenCode bypass harness into payloadMap:
//   - Injects preamble into the first system message or prepends {role: "system", content: preamble}.
//   - Appends the 6 official tools (edit, glob, grep, read, shell, write) to tools, renaming any
//     existing caller tools named read and shell to read_client and shell_client.
//   - If the caller provided NO tools originally, explicitly sets tool_choice: "none".
//   - Sets stream: true and stream_options: {"include_usage": true}.
func injectOpenCodeHarness(payloadMap map[string]any) {
	if payloadMap == nil {
		return
	}

	// 1. Injects preamble into the first system message or prepends it
	var msgs []any
	switch v := payloadMap["messages"].(type) {
	case []any:
		msgs = v
	case []map[string]any:
		for _, item := range v {
			msgs = append(msgs, item)
		}
	}

	foundSystem := false
	for i, m := range msgs {
		mm, ok := m.(map[string]any)
		if !ok {
			continue
		}
		role, _ := mm["role"].(string)
		if strings.EqualFold(role, "system") {
			foundSystem = true
			contentStr := ""
			switch c := mm["content"].(type) {
			case string:
				contentStr = c
			case []any:
				var parts []string
				for _, p := range c {
					if pm, ok := p.(map[string]any); ok {
						if text, ok := pm["text"].(string); ok && text != "" {
							parts = append(parts, text)
						}
					}
				}
				contentStr = strings.Join(parts, "\n\n")
			}

			if !strings.HasPrefix(strings.TrimSpace(contentStr), openCodePreambleMarker) {
				if strings.TrimSpace(contentStr) == "" {
					mm["content"] = openCodeAgentPreamble
				} else {
					mm["content"] = openCodeAgentPreamble + "\n\n" + contentStr
				}
			}
			msgs[i] = mm
			break
		}
	}

	if !foundSystem {
		sysMsg := map[string]any{
			"role":    "system",
			"content": openCodeAgentPreamble,
		}
		if len(msgs) == 0 {
			msgs = []any{sysMsg}
		} else {
			msgs = append([]any{sysMsg}, msgs...)
		}
	}
	payloadMap["messages"] = msgs

	// 2. Tools injection and renaming
	var callerTools []any
	switch v := payloadMap["tools"].(type) {
	case []any:
		callerTools = v
	case []map[string]any:
		for _, item := range v {
			callerTools = append(callerTools, item)
		}
	}

	callerHadTools := len(callerTools) > 0
	var finalTools []any

	for _, t := range callerTools {
		tm, ok := t.(map[string]any)
		if !ok {
			continue
		}
		toolCopy := deepCopyMap(tm)
		fn, isFn := toolCopy["function"].(map[string]any)
		name := ""
		if isFn {
			name, _ = fn["name"].(string)
		} else {
			name, _ = toolCopy["name"].(string)
		}
		name = strings.TrimSpace(name)
		if name == "read" {
			name = "read_client"
			if isFn {
				fn["name"] = name
			} else {
				toolCopy["name"] = name
			}
		} else if name == "shell" {
			name = "shell_client"
			if isFn {
				fn["name"] = name
			} else {
				toolCopy["name"] = name
			}
		}
		finalTools = append(finalTools, toolCopy)
	}

	for _, gateTool := range getOfficialGateTools() {
		finalTools = append(finalTools, deepCopyMap(gateTool))
	}
	payloadMap["tools"] = finalTools

	// 3. If caller provided NO tools originally, explicitly set tool_choice: "none"
	if !callerHadTools {
		payloadMap["tool_choice"] = "none"
	} else {
		if tc, ok := payloadMap["tool_choice"].(map[string]any); ok {
			if fn, ok := tc["function"].(map[string]any); ok {
				if n, ok := fn["name"].(string); ok {
					if n == "read" {
						fn["name"] = "read_client"
					} else if n == "shell" {
						fn["name"] = "shell_client"
					}
				}
			}
		}
	}

	// 4. Set stream: true and stream_options: {"include_usage": true}
	payloadMap["stream"] = true
	payloadMap["stream_options"] = map[string]any{"include_usage": true}
}

// Trace headers generation: W3C b3 and traceparent
func getTraceRoot() string {
	traceRootOnce.Do(func() {
		b := make([]byte, 16)
		_, _ = rand.Read(b)
		traceRoot = hex.EncodeToString(b)
	})
	return traceRoot
}

func generateTraceHeaders() (b3 string, traceparent string) {
	root := getTraceRoot()
	spanBytes := make([]byte, 8)
	parentBytes := make([]byte, 8)
	_, _ = rand.Read(spanBytes)
	_, _ = rand.Read(parentBytes)
	span := hex.EncodeToString(spanBytes)
	parent := hex.EncodeToString(parentBytes)

	b3 = fmt.Sprintf("%s-%s-1-%s", root, span, parent)
	traceparent = fmt.Sprintf("00-%s-%s-01", root, span)
	return b3, traceparent
}

// Session caching with 30-minute TTL
func descendingTimeHex(ts int64, counter int64) string {
	val := uint64(^(ts*0x1000 + counter)) & sessionPrefixMask
	return fmt.Sprintf("%012x", val)
}

func genSessionIDLocked() string {
	now := time.Now().UnixMilli()
	if now != lastSessionTs {
		lastSessionTs = now
		sessionCounter = 0
	}
	sessionCounter++

	timeHex := descendingTimeHex(now, sessionCounter)
	randBytes := make([]byte, 14)
	_, _ = rand.Read(randBytes)
	for i := range randBytes {
		randBytes[i] = base62Chars[int(randBytes[i])%len(base62Chars)]
	}
	return fmt.Sprintf("ses_%s%s", timeHex, string(randBytes))
}

func genSessionID() string {
	sessionMu.Lock()
	defer sessionMu.Unlock()
	return genSessionIDLocked()
}

func getOrCreateSession(key string) string {
	sessionMu.Lock()
	defer sessionMu.Unlock()

	if key == "" {
		key = "opencode_global"
	}

	now := time.Now()
	if entry, ok := cachedSessions[key]; ok {
		if now.Sub(entry.lastUsed) < sessionTTL {
			entry.lastUsed = now
			cachedSessions[key] = entry
			return entry.sessionID
		}
	}
	for k, entry := range cachedSessions {
		if now.Sub(entry.lastUsed) >= sessionTTL {
			delete(cachedSessions, k)
		}
	}
	id := genSessionIDLocked()
	cachedSessions[key] = sessionEntry{sessionID: id, lastUsed: now}
	return id
}

func isOpenCodeFallbackError(statusCode int, body string) bool {
	if statusCode == 429 {
		return true
	}
	if strings.Contains(body, "FreeUsageLimitError") {
		return true
	}
	if statusCode == 400 {
		lower := strings.ToLower(body)
		if strings.Contains(lower, "model is unavailable") ||
			strings.Contains(lower, "unknown model") ||
			strings.Contains(lower, "model_unavailable") ||
			strings.Contains(lower, "modelprotocolunsupported") {
			return true
		}
	}
	if statusCode == 404 || statusCode == 410 || statusCode == 503 {
		return true
	}
	return false
}

// buildOpenCodeTargetURL resolves the appropriate upstream or sidecar endpoint URL.
// When baseURL is empty, direct upstream URLs are used.
// When baseURL is set (e.g. zen-proxy or standard OpenAI proxy on 127.0.0.1:8789),
// requests are mapped to standard OpenAI routes (/v1/chat/completions or /v1/responses),
// while preserving any custom paths or direct opencode.ai overrides.
func buildOpenCodeTargetURL(baseURL string, candidate string) string {
	isResponses := candidate == "muse-spark-1.3-contributor-free" || candidate == "muse-spark-1.2-contributor-free"
	endpoint := "/chat/completions"
	if isResponses {
		endpoint = "/responses"
	}

	if baseURL == "" {
		if isResponses {
			return opencodeResponsesURL
		}
		return opencodeChatURL
	}

	trimmed := strings.TrimRight(baseURL, "/")

	// If candidate is a responses model and URL ends with chat/completions, rewrite endpoint to responses.
	if isResponses && strings.HasSuffix(trimmed, "/chat/completions") {
		return strings.TrimSuffix(trimmed, "/chat/completions") + "/responses"
	}
	// If candidate is a chat completions model and URL ends with responses, rewrite endpoint to chat/completions.
	if !isResponses && strings.HasSuffix(trimmed, "/responses") {
		return strings.TrimSuffix(trimmed, "/responses") + "/chat/completions"
	}

	// If caller passed full endpoint path already, use it verbatim
	if strings.HasSuffix(trimmed, "/chat/completions") || strings.HasSuffix(trimmed, "/responses") {
		return trimmed
	}

	if u, err := url.Parse(trimmed); err == nil {
		if (strings.HasSuffix(u.Host, "opencode.ai") || u.Host == "opencode.ai") && (u.Path == "" || u.Path == "/") {
			return trimmed + "/inference/openai/v1" + endpoint
		}
	}

	if strings.HasSuffix(trimmed, "/inference/openai/v1") {
		return trimmed + endpoint
	}

	if strings.HasSuffix(trimmed, "/v1") {
		return trimmed + endpoint
	}

	return trimmed + "/v1" + endpoint
}

var (
	cachedProjectHash     string
	cachedProjectHashOnce sync.Once
)

// getProjectHash derives a stable sha1 hex hash for the project, matching
// OpenCode CLI and zen-proxy: sha1("directory:<resolved cwd>").
func getProjectHash() string {
	cachedProjectHashOnce.Do(func() {
		cwd, err := os.Getwd()
		if err != nil || cwd == "" {
			cwd = "."
		}
		h := sha1.Sum([]byte("directory:" + cwd))
		cachedProjectHash = hex.EncodeToString(h[:])
	})
	return cachedProjectHash
}

var harnessTools = map[string]bool{
	"edit":         true,
	"glob":         true,
	"grep":         true,
	"read":         true,
	"shell":        true,
	"write":        true,
	"question":     true,
	"skill":        true,
	"subagent":     true,
	"webfetch":     true,
	"websearch":    true,
	"execute":      true,
	"bash":         true,
	"read_client":  true,
	"shell_client": true,
}

func isHarnessTool(name string) bool {
	return harnessTools[strings.ToLower(strings.TrimSpace(name))]
}

// HarnessInfo reports the embedded native harness state for the management UI.
// The harness is compiled into the plugin binary — no external sidecar is required.
func HarnessInfo() map[string]any {
	sessionMu.Lock()
	sessions := len(cachedSessions)
	sessionMu.Unlock()
	tools := 0
	for range officialGateTools {
		tools++
	}
	return map[string]any{
		"native":         true,
		"preamble_bytes": len(openCodeAgentPreamble),
		"tools":          tools,
		"sessions":       sessions,
		"session_ttl":    int(sessionTTL / time.Minute),
	}
}

func isOfferedTool(name string, offered []string) bool {
	for _, o := range offered {
		if strings.EqualFold(o, name) {
			return true
		}
	}
	return false
}

func extractCallerTools(payload []byte) ([]string, bool) {
	if len(payload) == 0 {
		return nil, false
	}
	var raw struct {
		Tools     []map[string]any `json:"tools"`
		Functions []map[string]any `json:"functions"`
	}
	if err := json.Unmarshal(payload, &raw); err != nil {
		return nil, false
	}
	if len(raw.Tools) == 0 && len(raw.Functions) == 0 {
		return nil, false
	}
	var names []string
	for _, t := range raw.Tools {
		name := ""
		if fn, ok := t["function"].(map[string]any); ok {
			name, _ = fn["name"].(string)
		} else if n, ok := t["name"].(string); ok {
			name = n
		}
		name = strings.TrimSpace(name)
		if name != "" {
			names = append(names, name)
		}
	}
	for _, f := range raw.Functions {
		if n, ok := f["name"].(string); ok && strings.TrimSpace(n) != "" {
			names = append(names, strings.TrimSpace(n))
		}
	}
	return names, len(names) > 0
}

type streamToolFilter struct {
	hasCallerTools bool
	callerTools    []string
	droppedIndices map[int]bool
	keptIndices    map[int]bool
}

func newStreamToolFilter(hasCallerTools bool, callerTools []string) *streamToolFilter {
	return &streamToolFilter{
		hasCallerTools: hasCallerTools,
		callerTools:    callerTools,
		droppedIndices: make(map[int]bool),
		keptIndices:    make(map[int]bool),
	}
}

func (f *streamToolFilter) filterChunk(cleaned string) (string, bool) {
	cleaned = restoreToolNamesInLine(cleaned)

	if !strings.Contains(cleaned, "tool_calls") && !strings.Contains(cleaned, "finish_reason") {
		return cleaned, true
	}

	var chunk map[string]any
	if err := json.Unmarshal([]byte(cleaned), &chunk); err != nil {
		return cleaned, true
	}

	choices, ok := chunk["choices"].([]any)
	if !ok || len(choices) == 0 {
		return cleaned, true
	}

	modified := false
	hasUsefulContent := false

	for _, c := range choices {
		choice, ok := c.(map[string]any)
		if !ok {
			continue
		}
		if delta, ok := choice["delta"].(map[string]any); ok {
			if content, ok := delta["content"].(string); ok && content != "" {
				hasUsefulContent = true
			}
			if rc, ok := delta["reasoning_content"].(string); ok && rc != "" {
				hasUsefulContent = true
			}
			if role, ok := delta["role"].(string); ok && role != "" {
				hasUsefulContent = true
			}

			if !f.hasCallerTools {
				if _, hasTC := delta["tool_calls"]; hasTC {
					delete(delta, "tool_calls")
					modified = true
				}
			} else if rawTC, ok := delta["tool_calls"].([]any); ok {
				var keptTC []any
				for _, item := range rawTC {
					tc, ok := item.(map[string]any)
					if !ok {
						continue
					}
					idx := 0
					if idxf, ok := tc["index"].(float64); ok {
						idx = int(idxf)
					}
					fn, _ := tc["function"].(map[string]any)
					name := ""
					if fn != nil {
						name, _ = fn["name"].(string)
					} else {
						name, _ = tc["name"].(string)
					}
					name = strings.TrimSpace(name)
					if name == "read_client" {
						name = "read"
						if fn != nil {
							fn["name"] = name
						} else {
							tc["name"] = name
						}
						modified = true
					} else if name == "shell_client" {
						name = "shell"
						if fn != nil {
							fn["name"] = name
						} else {
							tc["name"] = name
						}
						modified = true
					}

					if name != "" {
						if len(f.callerTools) > 0 && !isOfferedTool(name, f.callerTools) && isHarnessTool(name) {
							f.droppedIndices[idx] = true
							modified = true
							continue
						}
						f.keptIndices[idx] = true
						hasUsefulContent = true
					} else if f.droppedIndices[idx] {
						modified = true
						continue
					} else {
						hasUsefulContent = true
					}
					keptTC = append(keptTC, tc)
				}
				if modified {
					if len(keptTC) > 0 {
						delta["tool_calls"] = keptTC
					} else {
						delete(delta, "tool_calls")
					}
				}
			}
		}

		if fr, ok := choice["finish_reason"].(string); ok {
			if fr == "tool_calls" {
				if !f.hasCallerTools || (len(f.keptIndices) == 0 && len(f.droppedIndices) > 0) {
					choice["finish_reason"] = "stop"
					modified = true
				}
			}
			hasUsefulContent = true
		}
	}

	if modified {
		if !hasUsefulContent {
			return "", false
		}
		out, err := json.Marshal(chunk)
		if err == nil {
			return string(out), true
		}
	}
	return cleaned, true
}
