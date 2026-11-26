package agenteval

import (
	"context"
	"encoding/json"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/davidgeorgehope/ducktel/internal/events"
	"github.com/davidgeorgehope/ducktel/internal/feedback"
	"github.com/davidgeorgehope/ducktel/internal/mcp"
	"github.com/davidgeorgehope/ducktel/internal/query"
)

// Events are not tools. This check is the payload bound: one occurrence's
// data object stays at or under 512 bytes, and events/list stays within
// the recorded ceiling.
func TestEventPayloadBound(t *testing.T) {
	engine, dir := openEmpty(t)
	session := connectEvents(t, engine, dir)

	list, err := sdkmcp.CallCustomMethod[*eventListParams, *eventListResult](context.Background(), session, "events/list", &eventListParams{})
	if err != nil {
		t.Fatal(err)
	}
	listRaw, err := json.Marshal(list)
	if err != nil {
		t.Fatal(err)
	}

	_, err = session.CallTool(context.Background(), &sdkmcp.CallToolParams{
		Name: "submit_feedback",
		Arguments: map[string]any{
			"report_id":    "fb-bound",
			"tenant":       "acme",
			"tool_name":    "log_search",
			"category":     "confusing_output",
			"severity":     "low",
			"summary":      "rows hid the error",
			"details":      "the tool returned an empty list",
			"proposed_fix": "include the error string in the empty result",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	page, err := sdkmcp.CallCustomMethod[*eventPollParams, *eventPollResult](context.Background(), session, "events/poll", &eventPollParams{
		Name:      "feedback.submitted",
		Arguments: map[string]any{"tenant": "acme"},
		Cursor:    json.RawMessage(`"0"`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Events) != 1 {
		t.Fatalf("events = %#v", page.Events)
	}
	dataRaw, err := json.Marshal(page.Events[0].Data)
	if err != nil {
		t.Fatal(err)
	}
	if len(dataRaw) > events.MaxDataBytes {
		t.Fatalf("occurrence data is %d bytes, bound is %d: %s", len(dataRaw), events.MaxDataBytes, dataRaw)
	}
	if _, ok := page.Events[0].Data["details"]; ok {
		t.Fatal("occurrence includes details")
	}
	if _, ok := page.Events[0].Data["proposed_fix"]; ok {
		t.Fatal("occurrence includes proposed_fix")
	}

	checkBaseline(t, []Result{
		{Label: "events/list", Bytes: len(listRaw), Tokens: Tokens(len(listRaw))},
		{Label: "events/poll feedback.submitted occurrence", Bytes: len(dataRaw), Tokens: Tokens(len(dataRaw))},
	})
}

type eventListParams struct {
	sdkmcp.ParamsBase
}

type eventListResult struct {
	sdkmcp.ResultBase
	ResultType string `json:"resultType,omitempty"`
	Events     []any  `json:"events"`
}

type eventPollParams struct {
	sdkmcp.ParamsBase
	Name      string          `json:"name"`
	Arguments map[string]any  `json:"arguments"`
	Cursor    json.RawMessage `json:"cursor"`
}

type eventPollResult struct {
	sdkmcp.ResultBase
	ResultType string `json:"resultType,omitempty"`
	Events     []struct {
		EventID string         `json:"eventId"`
		Name    string         `json:"name"`
		Data    map[string]any `json:"data"`
	} `json:"events"`
	Cursor string `json:"cursor"`
}

func connectEvents(t *testing.T, engine *query.Engine, dataDir string) *sdkmcp.ClientSession {
	t.Helper()
	srv := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "ducktel-eval", Version: "eval"}, mcp.SDKOptions())
	ds := mcp.NewServer(engine, dataDir, "", "")
	fb, err := feedback.New(feedback.Options{DataDir: dataDir})
	if err != nil {
		t.Fatal(err)
	}
	ds.SetFeedback(fb)
	ds.Register(srv)
	clientT, serverT := sdkmcp.NewInMemoryTransports()
	ctx := context.Background()
	if _, err := srv.Connect(ctx, serverT, nil); err != nil {
		t.Fatal(err)
	}
	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "eval-client", Version: "eval"}, nil)
	if err := sdkmcp.AddSendingCustomMethod[*eventListParams, *eventListResult](client, "events/list"); err != nil {
		t.Fatal(err)
	}
	if err := sdkmcp.AddSendingCustomMethod[*eventPollParams, *eventPollResult](client, "events/poll"); err != nil {
		t.Fatal(err)
	}
	session, err := client.Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { session.Close() })
	return session
}
