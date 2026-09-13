package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const discordAPI = "https://discord.com/api/v10"

type discordClient struct {
	token, channelID, hermesID, baseURL string
	http                                *http.Client
}

type discordUser struct {
	ID string `json:"id"`
}
type discordReaction struct {
	Count int `json:"count"`
	Emoji struct {
		Name string `json:"name"`
	} `json:"emoji"`
}
type discordMessage struct {
	ID        string            `json:"id"`
	Content   string            `json:"content"`
	Author    discordUser       `json:"author"`
	Reactions []discordReaction `json:"reactions"`
}

type completionState int

const (
	completionPending completionState = iota
	completionSucceeded
	completionFailed
)

func newDiscordClient(token, channelID, hermesID string) *discordClient {
	return &discordClient{token: token, channelID: channelID, hermesID: hermesID, baseURL: discordAPI, http: http.DefaultClient}
}

func (d *discordClient) createRelayMessage(ctx context.Context, content string) (discordMessage, error) {
	var raw [12]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return discordMessage{}, err
	}
	nonce := hex.EncodeToString(raw[:])
	body := map[string]any{
		"content":          content,
		"allowed_mentions": map[string]any{"parse": []string{}, "users": []string{d.hermesID}},
		"nonce":            nonce,
		"enforce_nonce":    true,
	}
	var message discordMessage
	err := d.doJSON(ctx, http.MethodPost, "/channels/"+d.channelID+"/messages", body, &message)
	return message, err
}

func (d *discordClient) threadExists(ctx context.Context, id string) (bool, error) {
	status, err := d.do(ctx, http.MethodGet, "/channels/"+id, nil, nil)
	if status == http.StatusNotFound {
		return false, nil
	}
	return status >= 200 && status < 300, err
}

func (d *discordClient) completion(ctx context.Context, sourceID string) (completionState, error) {
	var message discordMessage
	if err := d.doJSON(ctx, http.MethodGet, "/channels/"+d.channelID+"/messages/"+sourceID, nil, &message); err != nil {
		return completionPending, err
	}
	for _, candidate := range []struct {
		name  string
		state completionState
	}{{"❌", completionFailed}, {"✅", completionSucceeded}} {
		for _, reaction := range message.Reactions {
			if reaction.Count > 0 && reaction.Emoji.Name == candidate.name {
				ok, err := d.reactionHasHermes(ctx, sourceID, candidate.name)
				if err != nil {
					return completionPending, err
				}
				if ok {
					return candidate.state, nil
				}
			}
		}
	}
	return completionPending, nil
}

func (d *discordClient) reactionHasHermes(ctx context.Context, sourceID, emoji string) (bool, error) {
	var users []discordUser
	path := "/channels/" + d.channelID + "/messages/" + sourceID + "/reactions/" + url.PathEscape(emoji) + "?limit=100"
	if err := d.doJSON(ctx, http.MethodGet, path, nil, &users); err != nil {
		return false, err
	}
	for _, user := range users {
		if user.ID == d.hermesID {
			return true, nil
		}
	}
	return false, nil
}

func (d *discordClient) threadText(ctx context.Context, threadID string) (string, error) {
	var messages []discordMessage
	if err := d.doJSON(ctx, http.MethodGet, "/channels/"+threadID+"/messages?limit=100", nil, &messages); err != nil {
		return "", err
	}
	parts := make([]string, 0, len(messages))
	for i := len(messages) - 1; i >= 0; i-- {
		text := strings.TrimSpace(messages[i].Content)
		if messages[i].Author.ID == d.hermesID && text != "" {
			parts = append(parts, text)
		}
	}
	return strings.Join(parts, "\n\n"), nil
}

func (d *discordClient) doJSON(ctx context.Context, method, path string, in, out any) error {
	var body []byte
	var err error
	if in != nil {
		body, err = json.Marshal(in)
		if err != nil {
			return err
		}
	}
	_, err = d.do(ctx, method, path, body, out)
	return err
}

func (d *discordClient) do(ctx context.Context, method, path string, body []byte, out any) (int, error) {
	for attempt := 0; attempt < 2; attempt++ {
		req, err := http.NewRequestWithContext(ctx, method, d.baseURL+path, bytes.NewReader(body))
		if err != nil {
			return 0, err
		}
		req.Header.Set("Authorization", "Bot "+d.token)
		req.Header.Set("User-Agent", "DiscordBot (https://github.com/NotRllyRn/index-hermes-bridge, 1.0.0)")
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := d.http.Do(req)
		if err != nil {
			if attempt == 0 && ctx.Err() == nil {
				continue
			}
			return 0, err
		}
		data, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if readErr != nil {
			return resp.StatusCode, readErr
		}
		if resp.StatusCode == http.StatusTooManyRequests && attempt == 0 {
			var rate struct {
				RetryAfter float64 `json:"retry_after"`
			}
			if json.Unmarshal(data, &rate) != nil {
				return resp.StatusCode, errors.New("discord rate limit response was invalid")
			}
			if err := sleepContext(ctx, time.Duration(rate.RetryAfter*float64(time.Second))); err != nil {
				return resp.StatusCode, err
			}
			continue
		}
		if resp.StatusCode >= 500 && attempt == 0 {
			continue
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return resp.StatusCode, fmt.Errorf("discord returned HTTP %d", resp.StatusCode)
		}
		if out != nil && len(data) > 0 {
			if err := json.Unmarshal(data, out); err != nil {
				return resp.StatusCode, fmt.Errorf("decode discord response: %w", err)
			}
		}
		return resp.StatusCode, nil
	}
	return 0, errors.New("discord request failed")
}

func sleepContext(ctx context.Context, duration time.Duration) error {
	if duration <= 0 {
		return nil
	}
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func decimalID(value string) bool {
	if value == "" {
		return false
	}
	_, err := strconv.ParseUint(value, 10, 64)
	return err == nil
}
