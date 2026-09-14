package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func testDiscord(server *httptest.Server) *discordClient {
	return &discordClient{token: "token", channelID: "123", hermesID: "456", baseURL: server.URL, http: server.Client()}
}

func assertMessageRequest(t *testing.T, r *http.Request, path string) {
	t.Helper()
	if r.Method != http.MethodPost || r.URL.Path != path {
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
}

func TestCreateRelayMessage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertMessageRequest(t, r, "/channels/123/messages")
		_, _ = w.Write([]byte(`{"id":"789"}`))
	}))
	defer server.Close()
	message, err := testDiscord(server).createRelayMessage(context.Background(), "<@456> hi")
	if err != nil || message.ID != "789" {
		t.Fatalf("message=%+v err=%v", message, err)
	}
}

func TestCreateThreadMessage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertMessageRequest(t, r, "/channels/789/messages")
		_, _ = w.Write([]byte(`{"id":"101"}`))
	}))
	defer server.Close()
	message, err := testDiscord(server).createThreadMessage(context.Background(), "789", "<@456> hi")
	if err != nil || message.ID != "101" {
		t.Fatalf("message=%+v err=%v", message, err)
	}
}

func TestCreateThreadFromMessage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/channels/123/messages/789/threads" {
			t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
		}
		var body struct {
			Name    string `json:"name"`
			Archive int    `json:"auto_archive_duration"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body.Name != "testing" || body.Archive != 1440 {
			t.Fatalf("body=%+v", body)
		}
		_, _ = w.Write([]byte(`{"id":"789","owner_id":"999","parent_id":"123","type":11}`))
	}))
	defer server.Close()
	thread, err := testDiscord(server).createThreadFromMessage(context.Background(), "789", "testing")
	if err != nil || thread.OwnerID != "999" {
		t.Fatalf("thread=%+v err=%v", thread, err)
	}
}

func TestGetThread(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
		exists     bool
	}{
		{"missing", ``, 404, false},
		{"exists", `{"id":"789","owner_id":"456","parent_id":"123","type":11}`, 200, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			thread, exists, err := testDiscord(server).getThread(context.Background(), "789")
			if err != nil || exists != tc.exists || exists && thread.OwnerID != "456" {
				t.Fatalf("thread=%+v exists=%v err=%v", thread, exists, err)
			}
		})
	}
}

func TestCompletionUsesDynamicChannel(t *testing.T) {
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if strings.Contains(r.URL.Path, "/reactions/") {
			_, _ = w.Write([]byte(`[{"id":"456"}]`))
			return
		}
		_, _ = w.Write([]byte(`{"reactions":[{"count":1,"emoji":{"name":"✅"}}]}`))
	}))
	defer server.Close()
	state, err := testDiscord(server).completion(context.Background(), messageRef{ChannelID: "789", MessageID: "101"})
	if err != nil || state != completionSucceeded {
		t.Fatalf("state=%v err=%v", state, err)
	}
	if len(paths) != 2 || paths[0] != "/channels/789/messages/101" || !strings.HasPrefix(paths[1], "/channels/789/messages/101/reactions/") {
		t.Fatalf("paths=%v", paths)
	}
}

func TestLongRateLimitReturnsImmediately(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("X-RateLimit-Scope", "user")
		w.WriteHeader(429)
		_, _ = w.Write([]byte(`{"retry_after":278}`))
	}))
	defer server.Close()
	start := time.Now()
	_, err := testDiscord(server).createRelayMessage(context.Background(), "x")
	var rate *discordRateLimitError
	if !errors.As(err, &rate) || rate.RetryAfter != 278*time.Second || rate.Scope != "user" || calls.Load() != 1 || time.Since(start) > time.Second {
		t.Fatalf("err=%v calls=%d elapsed=%s", err, calls.Load(), time.Since(start))
	}
}

func TestShortRateLimitRetriesOnce(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(429)
			_, _ = w.Write([]byte(`{"retry_after":0}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"789"}`))
	}))
	defer server.Close()
	if _, err := testDiscord(server).createRelayMessage(context.Background(), "x"); err != nil || calls.Load() != 2 {
		t.Fatalf("err=%v calls=%d", err, calls.Load())
	}
}

func TestDiscordNoRetryOnForbiddenPreservesError(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(403)
		_, _ = w.Write([]byte(`{"code":50013,"message":"Missing Permissions"}`))
	}))
	defer server.Close()
	_, err := testDiscord(server).createRelayMessage(context.Background(), "x")
	var discordErr *discordHTTPError
	if !errors.As(err, &discordErr) || discordErr.Code != 50013 || discordErr.Message != "Missing Permissions" || calls.Load() != 1 {
		t.Fatalf("err=%v calls=%d", err, calls.Load())
	}
}

func TestCompletionDetectsReceipt(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/reactions/") {
			_, _ = w.Write([]byte(`[{"id":"456"}]`))
			return
		}
		_, _ = w.Write([]byte(`{"reactions":[{"count":1,"emoji":{"name":"👀"}}]}`))
	}))
	defer server.Close()
	state, err := testDiscord(server).completion(context.Background(), messageRef{ChannelID: "123", MessageID: "789"})
	if err != nil || state != completionProcessing {
		t.Fatalf("state=%v err=%v", state, err)
	}
}

func TestThreadTextChronologicalHermesOnly(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[
		{"content":"two","author":{"id":"456"}}, {"content":"ignore","author":{"id":"999"}},
		{"content":"one","author":{"id":"456"}}, {"content":" ","author":{"id":"456"}}]`))
	}))
	defer server.Close()
	text, err := testDiscord(server).threadText(context.Background(), "789")
	if err != nil || text != "one\n\ntwo" {
		t.Fatalf("text=%q err=%v", text, err)
	}
}
