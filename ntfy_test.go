package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNtfySend(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/my-topic" {
			t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		if string(body) != "Hermes answer" || r.Header.Get("Content-Type") != "text/plain; charset=utf-8" {
			t.Fatalf("body=%q type=%q", body, r.Header.Get("Content-Type"))
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := newNtfyClient(server.URL+"/", "my-topic", server.Client())
	if err := client.send(context.Background(), "Hermes answer"); err != nil {
		t.Fatal(err)
	}
}

func TestNtfySendRejectsFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }))
	defer server.Close()
	if err := newNtfyClient(server.URL, "topic", server.Client()).send(context.Background(), "answer"); err == nil {
		t.Fatal("expected error")
	}
}
