package main

// OmniRush transport: the gateway speaks the OpenAI *Responses* API
// (`POST <gateway>/v1/responses`) and streams SSE only. CLIProxyAPI hands the
// plugin Chat Completions requests, so this file translates both directions:
//
//	chat.completions request  -> responses request
//	responses SSE events      -> chat.completion.chunk SSE
//
// The gateway also refuses non-streaming calls ("Stream must be set to true")
// and requires `store: false`, so every request is issued as a stream and the
// non-streaming caller is served by the same translated SSE (main.go assembles
// it back into a single chat.completion object).

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

type omniRushModelSpec struct {
	ID        string
	Name      string
	Context   int64
	Output    int64
	Reasoning bool
}

// omniRushFallbackModels is what the account is served when the live catalog
// cannot be fetched. The gateway's own catalog (GET /v1/models) is the source of
// truth: ids come and go without a plugin release.
var omniRushFallbackModels = []omniRushModelSpec{
	{ID: "gpt-6-astra", Name: "GPT 6 Astra", Context: 400000, Output: 128000, Reasoning: true},
	{ID: "gpt-6.1-sol", Name: "GPT 6.1 Sol", Context: 400000, Output: 128000, Reasoning: true},
	{ID: "gpt-6-sol", Name: "GPT 6 Sol", Context: 400000, Output: 128000, Reasoning: true},
}

var (
	omniCatalogMu      sync.RWMutex
	omniCatalogCache   []omniRushModelSpec
	omniCatalogFetched time.Time
)

const omniCatalogTTL = 10 * time.Minute

// omniRushCatalog returns the account's live model list, cached briefly. It
// falls back to omniRushFallbackModels when the gateway cannot be reached, so a
// transient network error never empties the catalog.
func omniRushCatalog() []omniRushModelSpec {
	omniCatalogMu.RLock()
	if len(omniCatalogCache) > 0 && time.Since(omniCatalogFetched) < omniCatalogTTL {
		out := omniCatalogCache
		omniCatalogMu.RUnlock()
		return out
	}
	omniCatalogMu.RUnlock()

	models, err := fetchOmniRushCatalog()
	if err != nil || len(models) == 0 {
		if err != nil {
			logMsg("omnirush catalog fetch failed, using fallback: %v", err)
		}
		omniCatalogMu.Lock()
		omniCatalogCache = omniRushFallbackModels
		omniCatalogFetched = time.Now()
		omniCatalogMu.Unlock()
		return omniRushFallbackModels
	}

	omniCatalogMu.Lock()
	omniCatalogCache = models
	omniCatalogFetched = time.Now()
	omniCatalogMu.Unlock()
	return models
}

// pickOmniCatalogAuth returns the first account whose catalog request succeeds,
// so a dead token cannot hide the model list for the healthy accounts.
func pickOmniCatalogAuth() (*OmniRushAuth, string, error) {
	tried := map[string]bool{}
	var lastErr error
	for i := 0; i < omniAccountMaxTries; i++ {
		acct, err := selectOmniAccount(tried)
		if err != nil {
			if lastErr != nil {
				return nil, "", lastErr
			}
			return nil, "", err
		}
		tried[acct.ID] = true
		auth := accountToAuth(acct)
		if catalogTokenWorks(auth) {
			return auth, acct.LegacyPath, nil
		}
		lastErr = fmt.Errorf("omnirush catalog unavailable for %s", acct.label())
	}
	return nil, "", lastErr
}

func catalogTokenWorks(auth *OmniRushAuth) bool {
	base := omniRushGatewayBase(auth)
	req, err := http.NewRequest("GET", base+"/models", nil)
	if err != nil {
		return false
	}
	applyOmniRushHeaders(req, auth.AccessToken)
	req.Header.Set("Accept", "application/json")
	resp, err := omniRushAPIClient().Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	return resp.StatusCode < 400
}

func fetchOmniRushCatalog() ([]omniRushModelSpec, error) {
	// Try the pool rather than a fixed account: a benched identity still has a
	// readable catalog, but a revoked one would blank the model list.
	auth, _, err := pickOmniCatalogAuth()
	if err != nil || auth == nil || auth.AccessToken == "" {
		return nil, fmt.Errorf("omnirush authentication required")
	}

	base := omniRushGatewayBase(auth)
	req, err := http.NewRequest("GET", base+"/models", nil)
	if err != nil {
		return nil, err
	}
	applyOmniRushHeaders(req, auth.AccessToken)
	req.Header.Set("Accept", "application/json")

	resp, err := omniRushAPIClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, fmt.Errorf("catalog HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var payload struct {
		Data []struct {
			ID          string `json:"id"`
			DisplayName string `json:"display_name"`
			Status      string `json:"status"`
			Limits      struct {
				Context int64 `json:"context"`
				Output  int64 `json:"output"`
			} `json:"limits"`
			Capabilities struct {
				Reasoning bool `json:"reasoning"`
			} `json:"capabilities"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, err
	}

	out := make([]omniRushModelSpec, 0, len(payload.Data))
	for _, m := range payload.Data {
		id := strings.TrimSpace(m.ID)
		if id == "" || (m.Status != "" && !strings.EqualFold(m.Status, "active")) {
			continue
		}
		name := strings.TrimSpace(m.DisplayName)
		if name == "" {
			name = id
		}
		ctx := m.Limits.Context
		if ctx <= 0 {
			ctx = 400000
		}
		maxOut := m.Limits.Output
		if maxOut <= 0 {
			maxOut = 128000
		}
		out = append(out, omniRushModelSpec{
			ID:        id,
			Name:      name,
			Context:   ctx,
			Output:    maxOut,
			Reasoning: m.Capabilities.Reasoning,
		})
	}
	return out, nil
}

// omniRushModelIDs is the set of ids currently served by the account.
func omniRushModelIDs() map[string]bool {
	ids := make(map[string]bool)
	for _, m := range omniRushCatalog() {
		ids[m.ID] = true
	}
	return ids
}

func isOmniRushModelID(id string) bool {
	id = strings.TrimPrefix(strings.TrimSpace(id), "omnirush/")
	if id == "" {
		return false
	}
	if omniRushModelIDs()[id] {
		return true
	}
	for _, m := range omniRushFallbackModels {
		if m.ID == id {
			return true
		}
	}
	return false
}

// omniRushGatewayBase normalises the configured gateway URL to the `/v1` root.
func omniRushGatewayBase(auth *OmniRushAuth) string {
	base := defaultOmniRushOrigin
	if auth != nil && strings.TrimSpace(auth.GatewayURL) != "" {
		base = auth.GatewayURL
	}
	if env := strings.TrimSpace(os.Getenv("OMNIRUSH_BASE_URL")); env != "" {
		base = env
	}
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	base = strings.TrimSuffix(base, "/responses")
	if strings.HasSuffix(base, "/v1") {
		return base
	}
	if strings.HasSuffix(base, "/omnirush") {
		return base + "/v1"
	}
	return base + "/v1"
}

func applyOmniRushHeaders(req *http.Request, token string) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	// The gateway only serves recognised clients: a bare product name (a
	// versioned "omnirush/x" User-Agent is treated as a lookalike and refused).
	req.Header.Set("User-Agent", defaultOmniRushUserAgent)
	// Version travels in X-OmniRush-Client (not the UA), resolved live so a
	// settings/env override or a fallback bump takes effect without touching
	// call sites.
	req.Header.Set("X-OmniRush-Client", omniRushClientHeader())

	if !nativeHeadersEnabled() {
		return
	}
	// Native-client fingerprint: a stable per-install device id, a TTL-scoped
	// session id in the CLI's own id shape, and the runtime/platform hints a
	// Node CLI reports. These make a gateway call look like it came from an
	// installed CLI rather than a headless wrapper.
	req.Header.Set("X-OmniRush-Install-Id", omniRushInstallID())
	req.Header.Set("X-OmniRush-Session-Id", omniRushSessionID())
	req.Header.Set("X-OmniRush-Client-Name", "cli")
	req.Header.Set("X-OmniRush-Client-Version", omniRushClientVersion())
	req.Header.Set("X-OmniRush-Node", omniRushNodeVersion())
	req.Header.Set("X-OmniRush-Platform", omniRushPlatform())
	req.Header.Set("X-OmniRush-Arch", omniRushArch())
}

// ── Chat Completions -> Responses request ───────────────────────────────────

type chatToolFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
	Strict      *bool           `json:"strict,omitempty"`
}

type chatTool struct {
	Type     string            `json:"type"`
	Function *chatToolFunction `json:"function,omitempty"`
}

type chatContentPart struct {
	Type     string          `json:"type"`
	Text     string          `json:"text,omitempty"`
	ImageURL json.RawMessage `json:"image_url,omitempty"`
}

type chatMessage struct {
	Role      string          `json:"role"`
	Content   json.RawMessage `json:"content,omitempty"`
	ToolCalls []struct {
		ID       string `json:"id"`
		Type     string `json:"type"`
		Function struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"function"`
	} `json:"tool_calls,omitempty"`
	ToolCallID string `json:"tool_call_id,omitempty"`
	Name       string `json:"name,omitempty"`
}

type chatCompletionRequest struct {
	Model             string          `json:"model"`
	Messages          []chatMessage   `json:"messages"`
	Tools             []chatTool      `json:"tools,omitempty"`
	ToolChoice        json.RawMessage `json:"tool_choice,omitempty"`
	Temperature       *float64        `json:"temperature,omitempty"`
	TopP              *float64        `json:"top_p,omitempty"`
	MaxTokens         *int            `json:"max_tokens,omitempty"`
	MaxCompletionToks *int            `json:"max_completion_tokens,omitempty"`
	ReasoningEffort   string          `json:"reasoning_effort,omitempty"`
	Reasoning         *struct {
		Effort  string `json:"effort,omitempty"`
		Summary string `json:"summary,omitempty"`
	} `json:"reasoning,omitempty"`
	ParallelToolCalls *bool `json:"parallel_tool_calls,omitempty"`
}

type responsesInputItem map[string]any

// buildOmniRushResponsesPayload converts a Chat Completions request body into a
// Responses API body. System/developer turns become `instructions`; every other
// turn becomes an input item. Assistant tool calls and tool results keep their
// call ids so multi-turn agent loops survive the translation.
func buildOmniRushResponsesPayload(payload []byte, model string) ([]byte, error) {
	var req chatCompletionRequest
	if err := json.Unmarshal(payload, &req); err != nil {
		return nil, fmt.Errorf("omnirush: cannot parse chat request: %w", err)
	}

	var instructions []string
	input := make([]responsesInputItem, 0, len(req.Messages))

	for _, msg := range req.Messages {
		switch strings.ToLower(strings.TrimSpace(msg.Role)) {
		case "system", "developer":
			if text := rawContentText(msg.Content); text != "" {
				instructions = append(instructions, text)
			}

		case "tool", "function":
			input = append(input, responsesInputItem{
				"type":    "function_call_output",
				"call_id": firstNonEmpty(msg.ToolCallID, msg.Name),
				"output":  rawContentText(msg.Content),
			})

		case "assistant":
			if text := rawContentText(msg.Content); text != "" {
				input = append(input, responsesInputItem{
					"type":    "message",
					"role":    "assistant",
					"content": []map[string]any{{"type": "output_text", "text": text}},
				})
			}
			for _, tc := range msg.ToolCalls {
				input = append(input, responsesInputItem{
					"type":      "function_call",
					"call_id":   tc.ID,
					"name":      tc.Function.Name,
					"arguments": tc.Function.Arguments,
				})
			}

		default: // user, and anything unknown, is sent as a user turn
			parts := chatContentToResponsesParts(msg.Content)
			if len(parts) == 0 {
				continue
			}
			input = append(input, responsesInputItem{
				"type":    "message",
				"role":    "user",
				"content": parts,
			})
		}
	}

	body := map[string]any{
		"model":  model,
		"input":  input,
		"stream": true,  // the gateway refuses non-streaming calls
		"store":  false, // and requires store to be off
	}
	if len(instructions) > 0 {
		body["instructions"] = strings.Join(instructions, "\n\n")
	}

	if len(req.Tools) > 0 {
		tools := make([]map[string]any, 0, len(req.Tools))
		for _, t := range req.Tools {
			if t.Function == nil || strings.TrimSpace(t.Function.Name) == "" {
				continue
			}
			tool := map[string]any{
				"type": "function",
				"name": t.Function.Name,
			}
			if t.Function.Description != "" {
				tool["description"] = t.Function.Description
			}
			if len(t.Function.Parameters) > 0 {
				tool["parameters"] = t.Function.Parameters
			}
			if t.Function.Strict != nil {
				tool["strict"] = *t.Function.Strict
			}
			tools = append(tools, tool)
		}
		if len(tools) > 0 {
			body["tools"] = tools
		}
	}

	if choice := responsesToolChoice(req.ToolChoice); choice != nil {
		body["tool_choice"] = choice
	}
	if req.Temperature != nil {
		body["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		body["top_p"] = *req.TopP
	}
	if req.ParallelToolCalls != nil {
		body["parallel_tool_calls"] = *req.ParallelToolCalls
	}

	// OmniRush models are catalogued with supportsMaxOutputTokens=false: the
	// gateway rejects an output cap, so it is deliberately dropped.
	if effort := omniRushReasoningEffort(&req); effort != "" {
		body["reasoning"] = map[string]any{"effort": effort, "summary": "auto"}
	}

	return json.Marshal(body)
}

func omniRushReasoningEffort(req *chatCompletionRequest) string {
	effort := strings.ToLower(strings.TrimSpace(req.ReasoningEffort))
	if effort == "" && req.Reasoning != nil {
		effort = strings.ToLower(strings.TrimSpace(req.Reasoning.Effort))
	}
	switch effort {
	case "minimal", "low", "medium", "high", "xhigh", "max":
		return effort
	case "none", "off":
		return ""
	}
	// The caller did not ask for a level, so the dashboard default applies. A
	// lower effort is the difference between a snappy answer and a long wait on
	// a reasoning model.
	return settingsOmniRushReasoning()
}

func responsesToolChoice(raw json.RawMessage) any {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return nil
	}
	var asString string
	if err := json.Unmarshal(trimmed, &asString); err == nil {
		switch strings.ToLower(asString) {
		case "auto", "none", "required":
			return strings.ToLower(asString)
		}
		return nil
	}
	var asObject struct {
		Type     string `json:"type"`
		Function *struct {
			Name string `json:"name"`
		} `json:"function"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(trimmed, &asObject); err != nil {
		return nil
	}
	name := asObject.Name
	if name == "" && asObject.Function != nil {
		name = asObject.Function.Name
	}
	if name == "" {
		return nil
	}
	return map[string]any{"type": "function", "name": name}
}

// rawContentText flattens a message's content (string or part array) to text.
func rawContentText(raw json.RawMessage) string {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return ""
	}
	var asString string
	if err := json.Unmarshal(trimmed, &asString); err == nil {
		return asString
	}
	var parts []chatContentPart
	if err := json.Unmarshal(trimmed, &parts); err != nil {
		return ""
	}
	var out []string
	for _, p := range parts {
		if strings.TrimSpace(p.Text) != "" {
			out = append(out, p.Text)
		}
	}
	return strings.Join(out, "\n")
}

func chatContentToResponsesParts(raw json.RawMessage) []map[string]any {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return nil
	}

	var asString string
	if err := json.Unmarshal(trimmed, &asString); err == nil {
		if strings.TrimSpace(asString) == "" {
			return nil
		}
		return []map[string]any{{"type": "input_text", "text": asString}}
	}

	var parts []chatContentPart
	if err := json.Unmarshal(trimmed, &parts); err != nil {
		return nil
	}
	out := make([]map[string]any, 0, len(parts))
	for _, p := range parts {
		switch strings.ToLower(p.Type) {
		case "text", "input_text":
			if strings.TrimSpace(p.Text) != "" {
				out = append(out, map[string]any{"type": "input_text", "text": p.Text})
			}
		case "image_url", "input_image":
			if url := imageURLValue(p.ImageURL); url != "" {
				out = append(out, map[string]any{"type": "input_image", "image_url": url})
			}
		}
	}
	return out
}

func imageURLValue(raw json.RawMessage) string {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return ""
	}
	var asString string
	if err := json.Unmarshal(trimmed, &asString); err == nil {
		return asString
	}
	var obj struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(trimmed, &obj); err == nil {
		return obj.URL
	}
	return ""
}

// ── Responses SSE -> Chat Completions SSE ───────────────────────────────────

type omniStreamState struct {
	model      string
	responseID string
	created    int64
	content    strings.Builder
	reasoning  strings.Builder
	toolCalls  map[int]*omniToolCall
	toolOrder  []int
	usage      map[string]any
	failed     string
}

type omniToolCall struct {
	ID        string
	Name      string
	Arguments strings.Builder
}

// translateOmniRushStream reads the gateway's Responses SSE and emits Chat
// Completions chunks. The returned reader always ends with `data: [DONE]`, which
// is what both execute() and executeStream() in main.go expect.
type omniStreamCloser struct {
	io.Reader
	closeFn func() error
}

func (s *omniStreamCloser) Close() error {
	if s.closeFn != nil {
		return s.closeFn()
	}
	return nil
}

func translateOmniRushStream(src io.Reader, model string) io.ReadCloser {
	pr, pw := io.Pipe()
	var once sync.Once
	closeBoth := func() error {
		var err error
		once.Do(func() {
			_ = pr.CloseWithError(io.EOF)
			if c, ok := src.(io.Closer); ok {
				err = c.Close()
			}
		})
		return err
	}

	go func() {
		defer func() {
			_ = pw.Close()
			if c, ok := src.(io.Closer); ok {
				_ = c.Close()
			}
		}()
		state := &omniStreamState{model: model, toolCalls: map[int]*omniToolCall{}}

		scanner := bufio.NewScanner(src)
		scanner.Buffer(make([]byte, 64*1024), maxSSELineBytes)

		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" || strings.HasPrefix(line, ":") || strings.HasPrefix(line, "event:") {
				continue
			}
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if data == "" || data == "[DONE]" {
				continue
			}

			var event map[string]any
			if err := json.Unmarshal([]byte(data), &event); err != nil {
				continue
			}

			chunks := state.handle(event)
			for _, chunk := range chunks {
				if _, err := io.WriteString(pw, chunk); err != nil {
					return
				}
			}
			if state.failed != "" {
				break
			}
		}

		if state.failed != "" {
			errChunk := state.errorChunk(state.failed)
			_, _ = io.WriteString(pw, errChunk)
		}
		_, _ = io.WriteString(pw, "data: [DONE]\n\n")
	}()

	return &omniStreamCloser{
		Reader:  pr,
		closeFn: closeBoth,
	}
}

func (s *omniStreamState) handle(event map[string]any) []string {
	eventType, _ := event["type"].(string)

	switch eventType {
	case "response.created", "response.in_progress":
		if resp, ok := event["response"].(map[string]any); ok {
			s.absorbResponse(resp)
		}

	case "response.output_text.delta":
		delta, _ := event["delta"].(string)
		if delta == "" {
			return nil
		}
		s.content.WriteString(delta)
		return []string{s.chunk(map[string]any{"content": delta})}

	case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
		delta, _ := event["delta"].(string)
		if delta == "" {
			return nil
		}
		s.reasoning.WriteString(delta)
		return []string{s.chunk(map[string]any{"reasoning_content": delta})}

	case "response.output_item.added":
		item, ok := event["item"].(map[string]any)
		if !ok {
			return nil
		}
		if itemType, _ := item["type"].(string); itemType == "function_call" {
			idx := len(s.toolOrder)
			call := &omniToolCall{}
			call.ID, _ = item["call_id"].(string)
			if call.ID == "" {
				call.ID, _ = item["id"].(string)
			}
			call.Name, _ = item["name"].(string)
			s.toolCalls[idx] = call
			s.toolOrder = append(s.toolOrder, idx)
			return []string{s.chunk(map[string]any{
				"tool_calls": []map[string]any{{
					"index": idx,
					"id":    call.ID,
					"type":  "function",
					"function": map[string]any{
						"name":      call.Name,
						"arguments": "",
					},
				}},
			})}
		}

	case "response.function_call_arguments.delta":
		delta, _ := event["delta"].(string)
		if delta == "" {
			return nil
		}
		idx := s.toolIndex(event)
		if idx < 0 {
			return nil
		}
		s.toolCalls[idx].Arguments.WriteString(delta)
		return []string{s.chunk(map[string]any{
			"tool_calls": []map[string]any{{
				"index":    idx,
				"function": map[string]any{"arguments": delta},
			}},
		})}

	case "response.output_item.done":
		item, ok := event["item"].(map[string]any)
		if !ok {
			return nil
		}
		if itemType, _ := item["type"].(string); itemType == "function_call" {
			idx := s.toolIndex(event)
			if idx < 0 {
				return nil
			}
			call := s.toolCalls[idx]
			// Some routes send the whole argument string only here.
			if call.Arguments.Len() == 0 {
				if args, _ := item["arguments"].(string); args != "" {
					call.Arguments.WriteString(args)
					return []string{s.chunk(map[string]any{
						"tool_calls": []map[string]any{{
							"index":    idx,
							"function": map[string]any{"arguments": args},
						}},
					})}
				}
			}
		}

	case "response.completed":
		if resp, ok := event["response"].(map[string]any); ok {
			s.absorbResponse(resp)
			if usage, ok := resp["usage"].(map[string]any); ok {
				s.usage = translateOmniUsage(usage)
			}
		}
		return []string{s.finishChunk()}

	case "response.failed", "response.error", "error":
		message := "omnirush stream failed"
		if resp, ok := event["response"].(map[string]any); ok {
			if errObj, ok := resp["error"].(map[string]any); ok {
				if msg, _ := errObj["message"].(string); msg != "" {
					message = msg
				}
			}
		}
		if errObj, ok := event["error"].(map[string]any); ok {
			if msg, _ := errObj["message"].(string); msg != "" {
				message = msg
			}
		} else if msg, ok := event["error"].(string); ok && msg != "" {
			message = msg
		}
		s.failed = message
	}

	return nil
}

func (s *omniStreamState) absorbResponse(resp map[string]any) {
	if id, _ := resp["id"].(string); id != "" && s.responseID == "" {
		s.responseID = id
	}
	if created, ok := resp["created_at"].(float64); ok && created > 0 && s.created == 0 {
		s.created = int64(created)
	}
	if model, _ := resp["model"].(string); model != "" {
		s.model = model
	}
}

func (s *omniStreamState) toolIndex(event map[string]any) int {
	// Deltas name the call by item_id; map it back to the index we announced.
	itemID, _ := event["item_id"].(string)
	if itemID != "" {
		for idx, call := range s.toolCalls {
			if call.ID == itemID {
				return idx
			}
		}
	}
	if outIdx, ok := event["output_index"].(float64); ok {
		if idx := int(outIdx); idx >= 0 && idx < len(s.toolOrder) {
			return idx
		}
	}
	if len(s.toolOrder) == 1 {
		return s.toolOrder[0]
	}
	if len(s.toolOrder) > 0 {
		return s.toolOrder[len(s.toolOrder)-1]
	}
	return -1
}

func (s *omniStreamState) id() string {
	if s.responseID != "" {
		return s.responseID
	}
	return fmt.Sprintf("chatcmpl-omnirush-%d", time.Now().UnixNano())
}

func (s *omniStreamState) createdUnix() int64 {
	if s.created > 0 {
		return s.created
	}
	return time.Now().Unix()
}

func (s *omniStreamState) chunk(delta map[string]any) string {
	obj := map[string]any{
		"id":      s.id(),
		"object":  "chat.completion.chunk",
		"created": s.createdUnix(),
		"model":   s.model,
		"choices": []map[string]any{{
			"index":         0,
			"delta":         delta,
			"finish_reason": nil,
		}},
	}
	raw, err := json.Marshal(obj)
	if err != nil {
		return ""
	}
	return "data: " + string(raw) + "\n\n"
}

func (s *omniStreamState) finishChunk() string {
	reason := "stop"
	if len(s.toolOrder) > 0 {
		reason = "tool_calls"
	}
	obj := map[string]any{
		"id":      s.id(),
		"object":  "chat.completion.chunk",
		"created": s.createdUnix(),
		"model":   s.model,
		"choices": []map[string]any{{
			"index":         0,
			"delta":         map[string]any{},
			"finish_reason": reason,
		}},
	}
	if s.usage != nil {
		obj["usage"] = s.usage
	}
	raw, err := json.Marshal(obj)
	if err != nil {
		return ""
	}
	return "data: " + string(raw) + "\n\n"
}

func (s *omniStreamState) errorChunk(message string) string {
	obj := map[string]any{
		"id":      s.id(),
		"object":  "chat.completion.chunk",
		"created": s.createdUnix(),
		"model":   s.model,
		"choices": []map[string]any{{
			"index":         0,
			"delta":         map[string]any{},
			"finish_reason": "error",
		}},
		"error": map[string]any{"message": message, "type": "omnirush_upstream_error"},
	}
	raw, err := json.Marshal(obj)
	if err != nil {
		return ""
	}
	return "data: " + string(raw) + "\n\n"
}

func translateOmniUsage(usage map[string]any) map[string]any {
	num := func(v any) int64 {
		if f, ok := v.(float64); ok {
			return int64(f)
		}
		return 0
	}
	prompt := num(usage["input_tokens"])
	completion := num(usage["output_tokens"])
	total := num(usage["total_tokens"])
	if total == 0 {
		total = prompt + completion
	}
	out := map[string]any{
		"prompt_tokens":     prompt,
		"completion_tokens": completion,
		"total_tokens":      total,
	}
	if details, ok := usage["input_tokens_details"].(map[string]any); ok {
		if cached := num(details["cached_tokens"]); cached > 0 {
			out["prompt_tokens_details"] = map[string]any{"cached_tokens": cached}
		}
	}
	if details, ok := usage["output_tokens_details"].(map[string]any); ok {
		if reasoning := num(details["reasoning_tokens"]); reasoning > 0 {
			out["completion_tokens_details"] = map[string]any{"reasoning_tokens": reasoning}
		}
	}
	return out
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// sortedOmniModelIDs is a stable ordering helper for diagnostics and tests.
func sortedOmniModelIDs() []string {
	ids := make([]string, 0, len(omniRushModelIDs()))
	for id := range omniRushModelIDs() {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}
