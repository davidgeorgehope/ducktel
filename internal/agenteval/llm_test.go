package agenteval

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// runLLM is off unless a person passes -llm. CI must not set it.
var runLLM = flag.Bool("llm", false, "run the xAI agent loop against the seeded tasks; never set in CI")

const xaiChatURL = "https://api.x.ai/v1/chat/completions"

func TestLLM(t *testing.T) {
	if !*runLLM {
		t.Skip("xAI agent loop is off")
	}
	if os.Getenv("CI") != "" || os.Getenv("GITHUB_ACTIONS") != "" {
		t.Fatal("-llm must not be set when CI or GITHUB_ACTIONS is set")
	}
	key := os.Getenv("XAI_API_KEY")
	if key == "" {
		t.Fatal("XAI_API_KEY is empty")
	}
	model := os.Getenv("XAI_MODEL")
	if model == "" {
		model = "grok-4"
	}
	if err := validateModel(model); err != nil {
		t.Fatal(err)
	}

	batch, engine := seed(t)
	o := Derive(batch)
	if err := checkOracle(o); err != nil {
		t.Fatal(err)
	}
	session := connect(t, engine, "", "")
	client := &xaiClient{
		key:   key,
		model: model,
		http:  &http.Client{Timeout: 90 * time.Second},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	text, err := runAgent(ctx, client, session, allQuestions())
	if err != nil {
		t.Fatal(err)
	}
	if err := gradeLLM(text, o); err != nil {
		t.Fatalf("%v\n%s", err, text)
	}
}

func TestRejectsForeignModel(t *testing.T) {
	for _, model := range []string{"gpt-4", "claude-3"} {
		t.Run(model, func(t *testing.T) {
			called := false
			c := &xaiClient{
				model: model,
				key:   "test-key",
				http: &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
					called = true
					return nil, errors.New("sent")
				})},
			}
			_, err := c.complete(context.Background(), []chatMessage{{Role: "user", Content: "hi"}}, nil)
			if err == nil {
				t.Fatal("model was accepted")
			}
			if called {
				t.Fatal("request was sent")
			}
		})
	}
}

func TestChatRequestTargetsXAI(t *testing.T) {
	var gotHost, gotPath, gotScheme, gotMethod string
	c := &xaiClient{
		key:   "test-key",
		model: "grok-4",
		http: &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
			gotHost = r.URL.Host
			gotPath = r.URL.Path
			gotScheme = r.URL.Scheme
			gotMethod = r.Method
			rec := httptest.NewRecorder()
			rec.Header().Set("Content-Type", "application/json")
			_, _ = rec.WriteString(`{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
			return rec.Result(), nil
		})},
	}
	turn, err := c.complete(context.Background(), []chatMessage{{Role: "user", Content: "hi"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if gotScheme != "https" || gotHost != "api.x.ai" || gotPath != "/v1/chat/completions" {
		t.Fatalf("request went to %s://%s%s", gotScheme, gotHost, gotPath)
	}
	if gotMethod != http.MethodPost {
		t.Fatalf("method %s", gotMethod)
	}
	if turn.Content != "ok" {
		t.Fatalf("content %q", turn.Content)
	}
}

func TestErrorOmitsKey(t *testing.T) {
	const key = "xai-secret-value"
	c := &xaiClient{
		key:   key,
		model: "grok-4",
		http: &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
			return nil, fmt.Errorf("dial failed %s %s", key, r.Header.Get("Authorization"))
		})},
	}
	_, err := c.complete(context.Background(), []chatMessage{{Role: "user", Content: "hi"}}, nil)
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), key) {
		t.Fatal("key leaked")
	}

	c.http = &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		rec := httptest.NewRecorder()
		rec.WriteHeader(http.StatusUnauthorized)
		_, _ = rec.WriteString(key)
		return rec.Result(), nil
	})}
	_, err = c.complete(context.Background(), []chatMessage{{Role: "user", Content: "hi"}}, nil)
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), key) {
		t.Fatal("key leaked")
	}
}

func allQuestions() string {
	var b strings.Builder
	b.WriteString("Answer every question about tenant acme. Use the tools.\n")
	for i, task := range Tasks() {
		fmt.Fprintf(&b, "%d. %s\n", i+1, task.Question)
	}
	return b.String()
}

func gradeLLM(text string, o Oracle) error {
	if !containsAny(text, o.MostErrorServices) {
		return fmt.Errorf("answer missing a most-error service %v", o.MostErrorServices)
	}
	var names []string
	for _, root := range o.SlowestRoots {
		names = append(names, root.SpanName)
	}
	if !containsAny(text, names) {
		return fmt.Errorf("answer missing a slowest root span name %v", names)
	}
	if !containsAny(text, o.FailingPayment) {
		return fmt.Errorf("answer missing a failing payment trace id")
	}
	for _, name := range o.MetricNames {
		if !strings.Contains(text, name) {
			return fmt.Errorf("answer missing metric %s", name)
		}
	}
	if !strings.Contains(text, "WARN") {
		return fmt.Errorf("answer missing WARN")
	}
	return nil
}

func containsAny(text string, parts []string) bool {
	for _, part := range parts {
		if part != "" && strings.Contains(text, part) {
			return true
		}
	}
	return false
}

func runAgent(ctx context.Context, client *xaiClient, session *sdkmcp.ClientSession, question string) (string, error) {
	tools, err := functionTools(ctx, session)
	if err != nil {
		return "", err
	}
	msgs := []chatMessage{
		{Role: "system", Content: "You answer questions about telemetry using the tools. The tenant is acme. Pass since_minutes 180 on tools that accept it. Quote the exact service names, span names, trace ids, metric names, and severities the tools return."},
		{Role: "user", Content: question},
	}
	var last string
	for round := 0; round < 8; round++ {
		turn, err := client.complete(ctx, msgs, tools)
		if err != nil {
			return "", err
		}
		last = turn.Content
		if len(turn.ToolCalls) == 0 {
			return last, nil
		}
		msgs = append(msgs, chatMessage{Role: "assistant", Content: turn.Content, ToolCalls: turn.ToolCalls})
		for _, tc := range turn.ToolCalls {
			args, err := tc.Function.Arguments.asMap()
			var text string
			if err != nil {
				text = err.Error()
			} else {
				text = execTool(ctx, session, tc.Function.Name, args)
			}
			msgs = append(msgs, chatMessage{Role: "tool", ToolCallID: tc.ID, Content: text})
		}
	}
	return last, nil
}

func functionTools(ctx context.Context, session *sdkmcp.ClientSession) ([]xaiTool, error) {
	var cursor string
	var out []xaiTool
	for {
		res, err := session.ListTools(ctx, &sdkmcp.ListToolsParams{Cursor: cursor})
		if err != nil {
			return nil, err
		}
		for _, tool := range res.Tools {
			params := tool.InputSchema
			if params == nil {
				params = map[string]any{"type": "object"}
			}
			out = append(out, xaiTool{
				Type: "function",
				Function: xaiFunction{
					Name:        tool.Name,
					Description: tool.Description,
					Parameters:  params,
				},
			})
		}
		if res.NextCursor == "" {
			return out, nil
		}
		cursor = res.NextCursor
	}
}

func execTool(ctx context.Context, session *sdkmcp.ClientSession, name string, args map[string]any) string {
	res, err := session.CallTool(ctx, &sdkmcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		return err.Error()
	}
	var b strings.Builder
	for _, block := range res.Content {
		if text, ok := block.(*sdkmcp.TextContent); ok {
			b.WriteString(text.Text)
		}
	}
	return b.String()
}

type xaiClient struct {
	key   string
	model string
	http  *http.Client
}

type chatMessage struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	ToolCalls  []toolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

type toolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function toolFunction `json:"function"`
}

type toolFunction struct {
	Name      string `json:"name"`
	Arguments argStr `json:"arguments"`
}

type xaiTool struct {
	Type     string      `json:"type"`
	Function xaiFunction `json:"function"`
}

type xaiFunction struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Parameters  any    `json:"parameters"`
}

type chatTurn struct {
	Content   string
	ToolCalls []toolCall
}

type argStr string

func (a *argStr) UnmarshalJSON(b []byte) error {
	if len(b) == 0 || string(b) == "null" {
		*a = ""
		return nil
	}
	if b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		*a = argStr(s)
		return nil
	}
	*a = argStr(b)
	return nil
}

func (a argStr) MarshalJSON() ([]byte, error) {
	return json.Marshal(string(a))
}

func (a argStr) asMap() (map[string]any, error) {
	s := strings.TrimSpace(string(a))
	if s == "" || s == "null" {
		return map[string]any{}, nil
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		return nil, err
	}
	if m == nil {
		m = map[string]any{}
	}
	return m, nil
}

func (c *xaiClient) complete(ctx context.Context, messages []chatMessage, tools []xaiTool) (chatTurn, error) {
	if err := validateModel(c.model); err != nil {
		return chatTurn{}, err
	}
	payload := map[string]any{
		"model":    c.model,
		"messages": messages,
	}
	if len(tools) > 0 {
		payload["tools"] = tools
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return chatTurn{}, c.redact(err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, xaiChatURL, bytes.NewReader(data))
	if err != nil {
		return chatTurn{}, c.redact(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.key)

	resp, err := c.client().Do(req)
	if err != nil {
		return chatTurn{}, c.redact(fmt.Errorf("xAI request failed: %v", err))
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return chatTurn{}, c.redact(fmt.Errorf("xAI response: %v", err))
	}
	if resp.StatusCode != http.StatusOK {
		return chatTurn{}, c.redact(fmt.Errorf("xAI status %s: %s", resp.Status, bytes.TrimSpace(body)))
	}
	var parsed struct {
		Choices []struct {
			Message struct {
				Content   textContent `json:"content"`
				ToolCalls []toolCall  `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
		Error json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return chatTurn{}, c.redact(fmt.Errorf("xAI response: %v", err))
	}
	if len(parsed.Error) > 0 && string(parsed.Error) != "null" {
		return chatTurn{}, c.redact(fmt.Errorf("xAI: %s", bytes.TrimSpace(parsed.Error)))
	}
	if len(parsed.Choices) == 0 {
		return chatTurn{}, c.redact(errors.New("xAI response has no choices"))
	}
	msg := parsed.Choices[0].Message
	return chatTurn{Content: string(msg.Content), ToolCalls: msg.ToolCalls}, nil
}

func (c *xaiClient) client() *http.Client {
	if c.http != nil {
		return c.http
	}
	return &http.Client{Timeout: 90 * time.Second}
}

func (c *xaiClient) redact(err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	if c.key != "" {
		msg = strings.ReplaceAll(msg, c.key, "REDACTED")
	}
	return errors.New(msg)
}

func validateModel(model string) error {
	m := strings.ToLower(model)
	if !strings.Contains(m, "grok") {
		return fmt.Errorf("model %q is not allowed", model)
	}
	for _, bad := range []string{"gpt", "openai", "claude", "gemini"} {
		if strings.Contains(m, bad) {
			return fmt.Errorf("model %q is not allowed", model)
		}
	}
	return nil
}

type textContent string

func (t *textContent) UnmarshalJSON(b []byte) error {
	if len(b) == 0 || string(b) == "null" {
		*t = ""
		return nil
	}
	if b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		*t = textContent(s)
		return nil
	}
	var parts []struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(b, &parts); err == nil {
		var bld strings.Builder
		for _, p := range parts {
			bld.WriteString(p.Text)
		}
		*t = textContent(bld.String())
		return nil
	}
	*t = textContent(string(b))
	return nil
}

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
