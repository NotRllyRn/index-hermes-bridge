package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
)

func testDiscord(server *httptest.Server) *discordClient {
	return &discordClient{token: "token", channelID: "123", hermesID: "456", baseURL: server.URL, http: server.Client()}
}

func TestCreateRelayMessage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/channels/123/messages" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bot token" || !strings.HasPrefix(r.Header.Get("User-Agent"), "DiscordBot (") {
			t.Error("missing headers")
		}
		var body struct {
			Content, Nonce string
			EnforceNonce   bool                            `json:"enforce_nonce"`
			Allowed        struct{ Parse, Users []string } `json:"allowed_mentions"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body.Content != "<@456> hi" || len(body.Nonce) != 24 || !body.EnforceNonce || len(body.Allowed.Parse) != 0 || !reflect.DeepEqual(body.Allowed.Users, []string{"456"}) {
			t.Errorf("bad body: %+v", body)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"789"}`))
	}))
	defer server.Close()
	message, err := testDiscord(server).createRelayMessage(context.Background(), "<@456> hi")
	if err != nil || message.ID != "789" {
		t.Fatalf("message=%+v err=%v", message, err)
	}
}

func TestDiscordRetry(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(429)
			_, _ = w.Write([]byte(`{"retry_after":0}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"789"}`))
	}))
	defer server.Close()
	if _, err := testDiscord(server).createRelayMessage(context.Background(), "x"); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Fatalf("got %d calls", calls.Load())
	}
}

func TestDiscordNoRetryOnForbidden(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(403) }))
	defer server.Close()
	_, err := testDiscord(server).createRelayMessage(context.Background(), "x")
	if err == nil || calls.Load() != 1 {
		t.Fatalf("err=%v calls=%d", err, calls.Load())
	}
}

func TestCompletionVerifiesHermes(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "/reactions/") {
			_, _ = w.Write([]byte(`[{"id":"456"}]`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"789","reactions":[{"count":1,"emoji":{"name":"✅"}}]}`))
	}))
	defer server.Close()
	state, err := testDiscord(server).completion(context.Background(), "789")
	if err != nil || state != completionSucceeded {
		t.Fatalf("state=%v err=%v", state, err)
	}
}

func TestCompletionIgnoresOtherUser(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "/reactions/") {
			_, _ = w.Write([]byte(`[{"id":"999"}]`))
			return
		}
		_, _ = w.Write([]byte(`{"reactions":[{"count":1,"emoji":{"name":"✅"}}]}`))
	}))
	defer server.Close()
	state, err := testDiscord(server).completion(context.Background(), "789")
	if err != nil || state != completionPending {
		t.Fatalf("state=%v err=%v", state, err)
	}
}

func TestThreadTextChronologicalHermesOnly(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[
		{"content":"two","author":{"id":"456"}},
		{"content":"ignore","author":{"id":"999"}},
		{"content":"one","author":{"id":"456"}},
		{"content":" ","author":{"id":"456"}}
	]`))
	}))
	defer server.Close()
	text, err := testDiscord(server).threadText(context.Background(), "789")
	if err != nil || text != "one\n\ntwo" {
		t.Fatalf("text=%q err=%v", text, err)
	}
}

func TestThreadExists404(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(404) }))
	defer server.Close()
	exists, err := testDiscord(server).threadExists(context.Background(), "789")
	if err != nil || exists {
		t.Fatalf("exists=%v err=%v", exists, err)
	}
}
