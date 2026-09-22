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
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
	"unsafe"
)

const (
	abiVersion = 1
	pluginID   = "cpa-opencode-plugin"

	// OpenCode Zen Endpoints
	opencodeChatURL      = "https://opencode.ai/zen/v1/chat/completions"
	opencodeResponsesURL = "https://opencode.ai/zen/v1/responses"
	opencodeClientUA     = "opencode/1.18.31"

	// MiMo Endpoints
	mimoChatURL      = "https://api.xiaomimimo.com/api/free-ai/openai/chat"
	mimoBootstrapURL = "https://api.xiaomimimo.com/api/free-ai/bootstrap"
	mimoSystemMarker = "You are MiMoCode, an interactive CLI tool that helps users with software engineering tasks."

	// Kilo Endpoints
	kiloChatURL = "https://api.kilo.ai/api/gateway/chat/completions"
)

const base62Chars = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

var (
	httpClient = &http.Client{
		Timeout: 5 * time.Minute,
		Transport: &http.Transport{
			MaxIdleConns:        100,
			MaxIdleConnsPerHost: 20,
			IdleConnTimeout:     90 * time.Second,
		},
	}

	mimoMu      sync.Mutex
	mimoToken   string
	mimoExpires time.Time

	sessionMu      sync.Mutex
	sessionCache   = make(map[string]sessionEntry)
)

type sessionEntry struct {
	sessionID string
	lastUsed  time.Time
}

var openCodeModels = map[string]bool{}

var mimoModels = map[string]bool{}

var kiloModels = map[string]bool{
	"kilo-auto/free":                                    true,
	"poolside/laguna-s-2.1:free":                         true,
	"nex-agi/nex-n2.5-pro:free":                          true,
	"nex-agi/nex-n2.5-mini:free":                         true,
	"stepfun/step-3.7-flash:free":                       true,
	"nvidia/nemotron-3-ultra-550b-a55b:free":            true,
	"nvidia/nemotron-3-super-120b-a12b:free":            true,
	"cohere/north-mini-code:free":                       true,
	"poolside/laguna-xs-2.1:free":                       true,
	"nvidia/nemotron-3-nano-omni-30b-a3b-reasoning:free": true,
	"openrouter/free":                                   true,
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
	Model    string `json:"Model"`
	Payload  []byte `json:"Payload"`
	StreamID string `json:"stream_id,omitempty"`
}

type streamEmit struct {
	StreamID string `json:"stream_id"`
	Payload  []byte `json:"payload,omitempty"`
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
		raw = C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
	}
	out, err := handleMethod(C.GoString(method), raw)
	if err != nil {
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
func cliproxyPluginShutdown() {}

func handleMethod(method string, raw []byte) ([]byte, error) {
	switch method {
	case "plugin.register", "plugin.reconfigure":
		return ok(map[string]any{
			"schema_version": 1,
			"metadata": map[string]any{
				"Name":             pluginID,
				"Version":          "1.0.0",
				"Author":           "tsaQB",
				"GitHubRepository": "https://github.com/tsaQB/cpa-opencode-plugin",
				"ConfigFields":     []any{},
			},
			"capabilities": map[string]any{
				"model_provider":         true,
				"executor":               true,
				"executor_model_scope":   "static",
				"executor_input_formats":  []string{"chat-completions"},
				"executor_output_formats": []string{"chat-completions"},
				"request_normalizer":     true,
			},
		})
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

	modelLower := strings.ToLower(strings.TrimSpace(req.Model))
	changed := false

	// Untuk model Nvidia NIM (deepseek-ai, z-ai, meta): hapus parameter reasoning/thinking yang tidak didukung
	if strings.HasPrefix(modelLower, "deepseek-ai/") || strings.HasPrefix(modelLower, "z-ai/") || strings.HasPrefix(modelLower, "meta/") {
		for _, k := range []string{"reasoning", "reasoning_effort", "thinking", "thinking_config"} {
			if _, exists := bodyMap[k]; exists {
				delete(bodyMap, k)
				changed = true
			}
		}
	}

	// Untuk model Kilo: hapus reasoning_effort agar tidak konflik dengan reasoning.effort
	if strings.Contains(modelLower, ":free") || strings.HasPrefix(modelLower, "kilo-auto") {
		if _, exists := bodyMap["reasoning_effort"]; exists {
			delete(bodyMap, "reasoning_effort")
			changed = true
		}
	}

	if changed {
		newBody, err := json.Marshal(bodyMap)
		if err == nil {
			return ok(payloadResponse{Body: json.RawMessage(newBody)})
		}
	}

	return ok(payloadResponse{Body: req.Body})
}

func sanitizePayloadForUpstream(model string, payload []byte) []byte {
	var bodyMap map[string]any
	if err := json.Unmarshal(payload, &bodyMap); err != nil {
		return payload
	}
	changed := false
	if _, exists := bodyMap["reasoning_effort"]; exists {
		delete(bodyMap, "reasoning_effort")
		changed = true
	}
	if changed {
		newBytes, err := json.Marshal(bodyMap)
		if err == nil {
			return newBytes
		}
	}
	return payload
}

func listModels() []map[string]any {
	type modelDef struct {
		id, name     string
		context, max int64
		thinking     bool
	}

	items := []modelDef{
		// Kilo Free Models (Active & Verified)
		{"deepseek-ai/deepseek-v4.1-flash", "DeepSeek V4.1 Flash (Nvidia)", 1000000, 65536, true},
		{"kilo-auto/free", "Kilo Auto Free", 256000, 10000, false},
		{"poolside/laguna-s-2.1:free", "Poolside Laguna S 2.1 Free", 262144, 32768, false},
		{"nex-agi/nex-n2.5-pro:free", "Nex AGI N2.5 Pro Free", 256000, 32000, true},
		{"nex-agi/nex-n2.5-mini:free", "Nex AGI N2.5 Mini Free", 256000, 32000, false},
		{"stepfun/step-3.7-flash:free", "Step 3.7 Flash Free (Kilo)", 262144, 262144, false},
		{"nvidia/nemotron-3-ultra-550b-a55b:free", "Nemotron 3 Ultra 550B Free (Kilo)", 1000000, 65536, true},
		{"nvidia/nemotron-3-super-120b-a12b:free", "Nemotron 3 Super 120B Free (Kilo)", 1000000, 65536, true},
		{"cohere/north-mini-code:free", "North Mini Code Free (Kilo)", 256000, 64000, false},
		{"poolside/laguna-xs-2.1:free", "Laguna XS 2.1 Free (Kilo)", 262144, 32768, false},
		{"nvidia/nemotron-3-nano-omni-30b-a3b-reasoning:free", "Nemotron 3 Nano Omni Free (Kilo)", 256000, 65536, true},
		{"openrouter/free", "OpenRouter Free Auto (Kilo)", 200000, 65536, false},
	}

	out := make([]map[string]any, 0, len(items))
	for _, item := range items {
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
	resp, err := callUpstream(req.Model, req.Payload, false)
	if err != nil {
		return errorEnvelope("upstream_error", err.Error(), 502), nil
	}
	defer resp.body.Close()

	body, readErr := io.ReadAll(resp.body)
	if readErr != nil {
		return errorEnvelope("upstream_error", readErr.Error(), 502), nil
	}
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

	go func() {
		resp, callErr := callUpstream(req.Model, req.Payload, true)
		if callErr != nil {
			closeStream(req.StreamID, callErr.Error())
			return
		}
		defer resp.body.Close()

		scanner := bufio.NewScanner(resp.body)
		scanner.Buffer(make([]byte, 64*1024), 1024*1024)
		for scanner.Scan() {
			line := scanner.Text()
			line = strings.TrimSpace(line)
			if line == "" || line == "data: [DONE]" || strings.HasPrefix(line, ":") {
				continue
			}
			// Pastikan format SSE bersih tanpa double "data: data: "
			cleaned := line
			if strings.HasPrefix(cleaned, "data:") {
				cleaned = strings.TrimSpace(strings.TrimPrefix(cleaned, "data:"))
			}
			if strings.HasPrefix(cleaned, "data:") {
				cleaned = strings.TrimSpace(strings.TrimPrefix(cleaned, "data:"))
			}
			if cleaned == "" || cleaned == "[DONE]" {
				continue
			}
			if errEmit := emitStream(req.StreamID, []byte(cleaned)); errEmit != nil {
				closeStream(req.StreamID, errEmit.Error())
				return
			}
		}
		if scanErr := scanner.Err(); scanErr != nil {
			closeStream(req.StreamID, scanErr.Error())
			return
		}
		closeStream(req.StreamID, "")
	}()

	return ok(map[string]any{
		"headers": http.Header{"Content-Type": []string{"text/event-stream"}},
	})
}

func decodeRequest(raw []byte) (executorRequest, error) {
	var req executorRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return req, err
	}
	if strings.TrimSpace(req.Model) == "" {
		return req, fmt.Errorf("model is required")
	}
	if len(req.Payload) == 0 {
		return req, fmt.Errorf("request payload is required")
	}
	return req, nil
}

func callUpstream(model string, payload []byte, stream bool) (*upstreamResponse, error) {
	if strings.HasPrefix(model, "deepseek-ai/") {
		return callNvidia(model, payload, stream)
	}
	if mimoModels[model] {
		return callMimo(payload, stream)
	} else if kiloModels[model] {
		return callKilo(model, payload, stream)
	}
	// Default to OpenCode Zen
	return callOpenCode(model, payload, stream)
}

func callNvidia(model string, payload []byte, stream bool) (*upstreamResponse, error) {
	var bodyMap map[string]any
	if err := json.Unmarshal(payload, &bodyMap); err == nil {
		for _, k := range []string{"reasoning", "reasoning_effort", "thinking", "thinking_config"} {
			delete(bodyMap, k)
		}
		if newB, err := json.Marshal(bodyMap); err == nil {
			payload = newB
		}
	}

	headers := http.Header{
		"Content-Type":  []string{"application/json"},
		"Authorization": []string{"Bearer NVIDIA_API_KEY_REDACTED"},
	}
	if stream {
		headers.Set("Accept", "text/event-stream")
	}

	req, err := http.NewRequest(http.MethodPost, "https://integrate.api.nvidia.com/v1/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header = headers

	res, err := httpClient.Do(req)
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

func callOpenCode(model string, payload []byte, stream bool) (*upstreamResponse, error) {
	targetURL := opencodeChatURL
	if model == "muse-spark-1.3-contributor-free" || model == "muse-spark-1.2-contributor-free" {
		targetURL = opencodeResponsesURL
	}

	// Transform payload: force stream: true, inject decoy tool quartet, sanitize reasoning
	transformedPayload := cloakOpencodePayload(payload, targetURL == opencodeResponsesURL)

	headers := http.Header{
		"Content-Type":        []string{"application/json"},
		"Authorization":       []string{"Bearer public"},
		"User-Agent":          []string{opencodeClientUA},
		"x-opencode-client":   []string{"desktop"},
		"x-opencode-project":  []string{"global"},
		"x-opencode-session":  []string{getOrCreateSession("default")},
		"x-opencode-request":  []string{genRequestID()},
	}
	if stream {
		headers.Set("Accept", "text/event-stream")
	}

	req, err := http.NewRequest(http.MethodPost, targetURL, bytes.NewReader(transformedPayload))
	if err != nil {
		return nil, err
	}
	req.Header = headers

	res, err := httpClient.Do(req)
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

func cloakOpencodePayload(raw []byte, isResponses bool) []byte {
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		return raw
	}

	// Always enforce stream = true to prevent 403 FreeTierError
	body["stream"] = true

	// Fingerprint tool quartet required by OpenCode Zen Free tier
	decoyTools := []map[string]any{
		{
			"type": "function",
			"function": map[string]any{
				"name":        "bash",
				"description": "Execute bash commands in the workspace environment.",
				"parameters":  map[string]any{"type": "object", "properties": map[string]any{}},
			},
		},
		{
			"type": "function",
			"function": map[string]any{
				"name":        "glob",
				"description": "Find files matching pattern.",
				"parameters":  map[string]any{"type": "object", "properties": map[string]any{}},
			},
		},
		{
			"type": "function",
			"function": map[string]any{
				"name":        "grep",
				"description": "Search file contents using regex pattern.",
				"parameters":  map[string]any{"type": "object", "properties": map[string]any{}},
			},
		},
		{
			"type": "function",
			"function": map[string]any{
				"name":        "read",
				"description": "Read file contents.",
				"parameters":  map[string]any{"type": "object", "properties": map[string]any{}},
			},
		},
	}

	if isResponses {
		// Responses format expects tools with top-level name
		var respTools []any
		if existing, ok := body["tools"].([]any); ok {
			respTools = existing
		}
		names := make(map[string]bool)
		for _, t := range respTools {
			if tm, ok := t.(map[string]any); ok {
				if n, ok := tm["name"].(string); ok {
					names[n] = true
				}
			}
		}
		for _, d := range decoyTools {
			fn := d["function"].(map[string]any)
			n := fn["name"].(string)
			if !names[n] {
				respTools = append(respTools, map[string]any{
					"type":        "function",
					"name":        n,
					"description": fn["description"],
					"parameters":  fn["parameters"],
				})
			}
		}
		body["tools"] = respTools
		body["tool_choice"] = "auto"

		// Sanitize prior reasoning encrypted_content from input array
		if input, ok := body["input"].([]any); ok {
			var cleanInput []any
			for _, item := range input {
				if im, ok := item.(map[string]any); ok {
					if t, ok := im["type"].(string); ok && t == "reasoning" {
						continue // drop prior-turn reasoning
					}
					delete(im, "encrypted_content")
					delete(im, "reasoning_encrypted_content")
				}
				cleanInput = append(cleanInput, item)
			}
			body["input"] = cleanInput
		}
	} else {
		// Chat Completions format
		var chatTools []any
		if existing, ok := body["tools"].([]any); ok && len(existing) > 0 {
			chatTools = existing
			names := make(map[string]bool)
			for _, t := range chatTools {
				if tm, ok := t.(map[string]any); ok {
					if fn, ok := tm["function"].(map[string]any); ok {
						if n, ok := fn["name"].(string); ok {
							names[n] = true
						}
					}
				}
			}
			for _, d := range decoyTools {
				fn := d["function"].(map[string]any)
				n := fn["name"].(string)
				if !names[n] {
					chatTools = append(chatTools, d)
				}
			}
		} else {
			// No client tools: inject decoy quartet and set tool_choice none
			for _, d := range decoyTools {
				chatTools = append(chatTools, d)
			}
			body["tool_choice"] = "none"
		}
		body["tools"] = chatTools
	}

	out, err := json.Marshal(body)
	if err != nil {
		return raw
	}
	return out
}

func callMimo(payload []byte, stream bool) (*upstreamResponse, error) {
	token, err := getMimoToken()
	if err != nil {
		return nil, err
	}

	headers := http.Header{
		"Content-Type":       []string{"application/json"},
		"Authorization":      []string{"Bearer " + token},
		"X-Mimo-Source":      []string{"mimocode-cli-free"},
		"X-Session-Affinity": []string{"ses_cliproxybansosfree000001"},
	}
	if stream {
		headers.Set("Accept", "text/event-stream")
	}

	body := injectMimoMarker(payload)
	req, err := http.NewRequest(http.MethodPost, mimoChatURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header = headers

	res, err := httpClient.Do(req)
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

func getMimoToken() (string, error) {
	mimoMu.Lock()
	defer mimoMu.Unlock()
	if mimoToken != "" && time.Now().Before(mimoExpires) {
		return mimoToken, nil
	}

	req, _ := http.NewRequest(http.MethodPost, mimoBootstrapURL, strings.NewReader(`{"client":"cpa-opencode-plugin"}`))
	req.Header.Set("Content-Type", "application/json")
	res, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()

	if res.StatusCode >= 400 {
		return "", fmt.Errorf("MiMo bootstrap failed: %s", res.Status)
	}

	var data struct {
		JWT string `json:"jwt"`
	}
	if err := json.NewDecoder(res.Body).Decode(&data); err != nil {
		return "", err
	}
	if data.JWT == "" {
		return "", fmt.Errorf("MiMo bootstrap returned empty JWT")
	}
	mimoToken, mimoExpires = data.JWT, time.Now().Add(50*time.Minute)
	return mimoToken, nil
}

func injectMimoMarker(raw []byte) []byte {
	var body map[string]any
	if json.Unmarshal(raw, &body) != nil {
		return raw
	}
	messages, ok := body["messages"].([]any)
	if !ok {
		return raw
	}
	for _, message := range messages {
		if m, ok := message.(map[string]any); ok && m["role"] == "system" && strings.Contains(fmt.Sprint(m["content"]), mimoSystemMarker) {
			return raw
		}
	}
	body["messages"] = append([]any{map[string]any{"role": "system", "content": mimoSystemMarker}}, messages...)
	out, err := json.Marshal(body)
	if err != nil {
		return raw
	}
	return out
}

func callKilo(model string, payload []byte, stream bool) (*upstreamResponse, error) {
	payload = sanitizePayloadForUpstream(model, payload)
	headers := http.Header{
		"Content-Type":          []string{"application/json"},
		"User-Agent":            []string{"opencode-kilo-provider"},
		"X-KILOCODE-EDITORNAME": []string{"Kilo CLI"},
	}
	if stream {
		headers.Set("Accept", "text/event-stream")
	}

	req, err := http.NewRequest(http.MethodPost, kiloChatURL, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header = headers

	res, err := httpClient.Do(req)
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

func getOrCreateSession(key string) string {
	sessionMu.Lock()
	defer sessionMu.Unlock()

	now := time.Now()
	if entry, exists := sessionCache[key]; exists {
		if now.Sub(entry.lastUsed) < 30*time.Minute {
			entry.lastUsed = now
			sessionCache[key] = entry
			return entry.sessionID
		}
	}

	id := genSessionID()
	sessionCache[key] = sessionEntry{sessionID: id, lastUsed: now}
	return id
}

func genSessionID() string {
	t := time.Now().UnixMilli()
	hexTime := fmt.Sprintf("%012x", t)
	return fmt.Sprintf("ses_%s%s", hexTime, randomBase62(14))
}

func genRequestID() string {
	t := time.Now().UnixMilli()
	hexTime := fmt.Sprintf("%012x", t)
	return fmt.Sprintf("msg_%s%s", hexTime, randomBase62(14))
}

func randomBase62(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	res := make([]byte, n)
	for i := 0; i < n; i++ {
		res[i] = base62Chars[int(b[i])%len(base62Chars)]
	}
	return string(res)
}

func emitStream(streamID string, payload []byte) error {
	req := streamEmit{StreamID: streamID, Payload: payload}
	b, err := json.Marshal(req)
	if err != nil {
		return err
	}
	var buf C.cliproxy_buffer
	cMethod := C.CString("host.stream.emit")
	defer C.free(unsafe.Pointer(cMethod))
	cReq := (*C.uint8_t)(unsafe.Pointer(&b[0]))
	rc := C.call_host_api(cMethod, cReq, C.size_t(len(b)), &buf)
	if buf.ptr != nil {
		C.free_host_buffer(buf.ptr, buf.len)
	}
	if rc != 0 {
		return fmt.Errorf("host stream_emit failed: %d", rc)
	}
	return nil
}

func closeStream(streamID, errStr string) {
	req := streamClose{StreamID: streamID, Error: errStr}
	b, _ := json.Marshal(req)
	var buf C.cliproxy_buffer
	cMethod := C.CString("host.stream.close")
	defer C.free(unsafe.Pointer(cMethod))
	var cReq *C.uint8_t
	if len(b) > 0 {
		cReq = (*C.uint8_t)(unsafe.Pointer(&b[0]))
	}
	C.call_host_api(cMethod, cReq, C.size_t(len(b)), &buf)
	if buf.ptr != nil {
		C.free_host_buffer(buf.ptr, buf.len)
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
