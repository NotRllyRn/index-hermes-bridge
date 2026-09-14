package main

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

type ntfyClient struct {
	endpoint string
	http     *http.Client
}

func newNtfyClient(serverURL, topic string, client *http.Client) *ntfyClient {
	return &ntfyClient{endpoint: strings.TrimRight(serverURL, "/") + "/" + url.PathEscape(topic), http: client}
}

func (n *ntfyClient) send(ctx context.Context, text string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.endpoint, bytes.NewBufferString(text))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "text/plain; charset=utf-8")
	resp, err := n.http.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("ntfy returned HTTP %d", resp.StatusCode)
	}
	return nil
}
