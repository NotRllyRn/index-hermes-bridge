package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestLoadConfig(t *testing.T) {
	valid := map[string]string{"MCP_BEARER_TOKEN": strings.Repeat("a", 64), "DISCORD_BOT_TOKEN": "bot", "DISCORD_CHANNEL_ID": "123", "HERMES_BOT_ID": "456"}
	if _, err := loadConfig(func(k string) string { return valid[k] }); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"MCP_BEARER_TOKEN", "DISCORD_BOT_TOKEN", "DISCORD_CHANNEL_ID", "HERMES_BOT_ID"} {
		t.Run(key, func(t *testing.T) {
			copy := mapsClone(valid)
			delete(copy, key)
			if _, err := loadConfig(func(k string) string { return copy[k] }); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func mapsClone(in map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range in {
		out[k] = v
	}
	return out
}

func TestSecurityMiddleware(t *testing.T) {
	h := securityMiddleware("secret", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	for _, tc := range []struct {
		name, auth, origin string
		want               int
	}{
		{"missing", "", "", 401}, {"wrong", "Bearer no", "", 401}, {"lowercase", "bearer secret", "", 401},
		{"space", "Bearer  secret", "", 401}, {"origin", "Bearer secret", "https://example.com", 403}, {"valid", "Bearer secret", "", 204},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/mcp", nil)
			r.Header.Set("Authorization", tc.auth)
			r.Header.Set("Origin", tc.origin)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tc.want {
				t.Fatalf("got %d want %d", w.Code, tc.want)
			}
		})
	}
}

func TestPebbleResults(t *testing.T) {
	for _, tc := range []struct {
		result   any
		contains []string
	}{
		{pebbleResponse("four", "question"), []string{`"coreSchema":1`, `"type":"Response"`, `"text":"four"`, `"question":"question"`}},
		{pebbleFailure("failed"), []string{`"isError":true`, `"type":"GenericFailure"`, `"userErrorMessage":"failed"`}},
	} {
		data, err := json.Marshal(tc.result)
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range tc.contains {
			if !strings.Contains(string(data), want) {
				t.Errorf("%s lacks %s", data, want)
			}
		}
	}
}

type fakeWorkflow struct {
	created      string
	source       discordMessage
	threadChecks int
	states       []completionState
	text         string
	ctxCancelled bool
}

func (f *fakeWorkflow) createRelayMessage(_ context.Context, s string) (discordMessage, error) {
	f.created = s
	return f.source, nil
}
func (f *fakeWorkflow) threadExists(_ context.Context, id string) (bool, error) {
	f.threadChecks++
	return f.threadChecks > 1, nil
}
func (f *fakeWorkflow) completion(ctx context.Context, id string) (completionState, error) {
	if len(f.states) == 0 {
		<-ctx.Done()
		f.ctxCancelled = true
		return completionPending, ctx.Err()
	}
	state := f.states[0]
	f.states = f.states[1:]
	return state, nil
}
func (f *fakeWorkflow) threadText(context.Context, string) (string, error) { return f.text, nil }

func TestAskHermesSuccess(t *testing.T) {
	f := &fakeWorkflow{source: discordMessage{ID: "99"}, states: []completionState{completionPending, completionSucceeded}, text: "one\n\ntwo"}
	result := askHermes(context.Background(), f, "456", askHermesInput{Message: " hello "}, workflowOptions{time.Second, time.Second, time.Millisecond})
	if result.IsError {
		t.Fatal("unexpected failure")
	}
	if f.created != "<@456> hello" || f.threadChecks != 2 {
		t.Fatalf("created=%q checks=%d", f.created, f.threadChecks)
	}
	data, _ := json.Marshal(result)
	if !strings.Contains(string(data), "one\\n\\ntwo") {
		t.Fatal(string(data))
	}
}

func TestAskHermesFailureAndCancellation(t *testing.T) {
	f := &fakeWorkflow{source: discordMessage{ID: "99"}, states: []completionState{completionFailed}}
	result := askHermes(context.Background(), f, "456", askHermesInput{Message: "hello"}, workflowOptions{time.Second, time.Second, time.Millisecond})
	if !result.IsError {
		t.Fatal("expected failure")
	}

	cancelled := &fakeWorkflow{source: discordMessage{ID: "99"}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result = askHermes(ctx, cancelled, "456", askHermesInput{Message: "hello"}, workflowOptions{time.Second, time.Second, time.Millisecond})
	if !result.IsError {
		t.Fatal("expected cancellation failure")
	}
}

func TestMCP2025InitializationAndToolList(t *testing.T) {
	cfg := config{MCPBearerToken: strings.Repeat("a", 64), HermesBotID: "456"}
	fake := &fakeWorkflow{source: discordMessage{ID: "99"}, states: []completionState{completionSucceeded}, text: "four"}
	server := httptest.NewServer(newMCPHandler(cfg, fake, workflowOptions{time.Second, time.Second, time.Millisecond}))
	defer server.Close()

	post := func(payload string, session string) (*http.Response, []byte) {
		req, err := http.NewRequest(http.MethodPost, server.URL, bytes.NewBufferString(payload))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+cfg.MCPBearerToken)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		if session != "" {
			req.Header.Set("Mcp-Session-Id", session)
		}
		resp, err := server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		return resp, body
	}

	resp, body := post(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"pebble-test","version":"1"}}}`, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("initialize: %d %s", resp.StatusCode, body)
	}
	session := resp.Header.Get("Mcp-Session-Id")
	if session == "" || !bytes.Contains(body, []byte(`"protocolVersion":"2025-11-25"`)) {
		t.Fatalf("session=%q body=%s", session, body)
	}
	resp, body = post(`{"jsonrpc":"2.0","method":"notifications/initialized"}`, session)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		t.Fatalf("initialized: %d %s", resp.StatusCode, body)
	}
	resp, body = post(`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`, session)
	if resp.StatusCode != http.StatusOK || !bytes.Contains(body, []byte(`"name":"ask_hermes"`)) || bytes.Contains(body, []byte(`"nextCursor"`)) {
		t.Fatalf("tools/list: %d %s", resp.StatusCode, body)
	}
	var envelope struct {
		Result struct {
			Tools []struct {
				InputSchema map[string]any `json:"inputSchema"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatal(err)
	}
	if len(envelope.Result.Tools) != 1 {
		t.Fatalf("got %d tools", len(envelope.Result.Tools))
	}
	if additional, ok := envelope.Result.Tools[0].InputSchema["additionalProperties"]; !ok || additional != false {
		t.Fatalf("schema allows extra properties: %s", body)
	}
	resp, body = post(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"ask_hermes","arguments":{"message":"what is two plus two?"}}}`, session)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("tools/call: %d %s", resp.StatusCode, body)
	}
	for _, want := range [][]byte{[]byte(`"coreSchema":1`), []byte(`"type":"Response"`), []byte(`"text":"four"`)} {
		if !bytes.Contains(body, want) {
			t.Fatalf("tools/call response lacks %s: %s", want, body)
		}
	}
}

func TestInputValidation(t *testing.T) {
	f := &fakeWorkflow{}
	if !askHermes(context.Background(), f, "1", askHermesInput{}, workflowOptions{}).IsError {
		t.Fatal("empty input accepted")
	}
	if !askHermes(context.Background(), f, "1", askHermesInput{Message: strings.Repeat("x", 2000)}, workflowOptions{}).IsError {
		t.Fatal("long input accepted")
	}
}
