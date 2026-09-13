package main

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	listenAddress       = ":8080"
	mcpPath             = "/mcp"
	discordMessageLimit = 2000
	overallTimeout      = 50 * time.Second
	threadTimeout       = 10 * time.Second
	pollInterval        = time.Second
)

const serverInstructions = "Use `ask_hermes` for every request and pass the user's complete request unchanged. After Hermes responds, summarize the answer in 1-3 concise sentences suitable for a phone notification, preserving the conclusion and any immediately important action. Ignore tool-progress/status messages. Then call `deliver_response` with only that summary."

type config struct{ MCPBearerToken, DiscordBotToken, DiscordChannelID, HermesBotID string }
type askHermesInput struct {
	Message string `json:"message" jsonschema:"The user's complete request to Hermes."`
}
type deliverResponseInput struct {
	Text string `json:"text" jsonschema:"A concise 1-3 sentence summary for the user's phone notification."`
}
type workflowOptions struct{ overall, thread, poll time.Duration }

type discordWorkflow interface {
	createRelayMessage(context.Context, string) (discordMessage, error)
	threadExists(context.Context, string) (bool, error)
	completion(context.Context, string) (completionState, error)
	threadText(context.Context, string) (string, error)
}

func loadConfig(getenv func(string) string) (config, error) {
	values := []*string{}
	cfg := config{}
	values = append(values, &cfg.MCPBearerToken, &cfg.DiscordBotToken, &cfg.DiscordChannelID, &cfg.HermesBotID)
	names := []string{"MCP_BEARER_TOKEN", "DISCORD_BOT_TOKEN", "DISCORD_CHANNEL_ID", "HERMES_BOT_ID"}
	for i, name := range names {
		*values[i] = strings.TrimSpace(getenv(name))
		if *values[i] == "" {
			return config{}, fmt.Errorf("%s is required", name)
		}
	}
	if len(cfg.MCPBearerToken) < 32 {
		return config{}, errors.New("MCP_BEARER_TOKEN must be at least 32 characters")
	}
	if !decimalID(cfg.DiscordChannelID) {
		return config{}, errors.New("DISCORD_CHANNEL_ID must be a decimal Discord ID")
	}
	if !decimalID(cfg.HermesBotID) {
		return config{}, errors.New("HERMES_BOT_ID must be a decimal Discord ID")
	}
	return cfg, nil
}

func newMCPHandler(cfg config, discord discordWorkflow, options workflowOptions) http.Handler {
	server := mcp.NewServer(&mcp.Implementation{Name: "index-hermes-bridge", Version: "1.0.0"}, &mcp.ServerOptions{Instructions: serverInstructions})
	falseValue, trueValue := false, true
	mcp.AddTool[askHermesInput, any](server, &mcp.Tool{
		Name:        "ask_hermes",
		Description: "Send the user's complete request to their Hermes agent through Discord and return Hermes's response.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: false, DestructiveHint: &falseValue, IdempotentHint: false, OpenWorldHint: &trueValue},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input askHermesInput) (*mcp.CallToolResult, any, error) {
		return askHermes(ctx, discord, cfg.HermesBotID, input, options), nil, nil
	})
	mcp.AddTool[deliverResponseInput, any](server, &mcp.Tool{
		Name:        "deliver_response",
		Description: "Deliver the concise final answer as the Pebble completion notification.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, DestructiveHint: &falseValue, IdempotentHint: true, OpenWorldHint: &falseValue},
	}, func(_ context.Context, _ *mcp.CallToolRequest, input deliverResponseInput) (*mcp.CallToolResult, any, error) {
		return deliverResponse(input.Text), nil, nil
	})
	streamable := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{
		JSONResponse:               true,
		MaxRequestBodyBytes:        64 << 10,
		DisableLocalhostProtection: true, // Bearer auth and the stricter no-Origin policy protect the Tailscale proxy path.
	})
	return securityMiddleware(cfg.MCPBearerToken, streamable)
}

func askHermes(parent context.Context, discord discordWorkflow, hermesID string, input askHermesInput, options workflowOptions) *mcp.CallToolResult {
	message := strings.TrimSpace(input.Message)
	if message == "" {
		return pebbleFailure("No message was provided.")
	}
	content := "<@" + hermesID + "> " + message
	if len(content) > discordMessageLimit {
		return pebbleFailure("The Index request is too long for Discord.")
	}
	ctx, cancel := context.WithTimeout(parent, options.overall)
	defer cancel()
	source, err := discord.createRelayMessage(ctx, content)
	if err != nil {
		return pebbleFailure("Discord could not receive the request.")
	}
	log.Printf("discord message created id=%s", source.ID)
	threadDeadline := time.Now().Add(options.thread)
	for {
		exists, err := discord.threadExists(ctx, source.ID)
		if err != nil {
			return pebbleFailure("Discord could not verify Hermes's thread.")
		}
		if exists {
			break
		}
		if time.Now().After(threadDeadline) {
			return pebbleFailure("Hermes did not create the expected Discord thread.")
		}
		if err := sleepContext(ctx, options.poll); err != nil {
			return timeoutFailure(parent)
		}
	}
	log.Printf("hermes thread detected id=%s", source.ID)
	for {
		state, err := discord.completion(ctx, source.ID)
		if err != nil {
			if ctx.Err() != nil {
				return timeoutFailure(parent)
			}
			return pebbleFailure("Discord could not check Hermes's status.")
		}
		switch state {
		case completionFailed:
			return pebbleFailure("Hermes reported that the request failed.")
		case completionSucceeded:
			text, err := discord.threadText(ctx, source.ID)
			if err != nil {
				return pebbleFailure("Discord could not read Hermes's response.")
			}
			if text == "" {
				return pebbleFailure("Hermes completed without a text response.")
			}
			log.Printf("hermes completed id=%s", source.ID)
			return textResult(text)
		}
		if err := sleepContext(ctx, options.poll); err != nil {
			return timeoutFailure(parent)
		}
	}
}

func timeoutFailure(parent context.Context) *mcp.CallToolResult {
	if parent.Err() != nil {
		return pebbleFailure("The request was cancelled.")
	}
	return pebbleFailure("Hermes did not respond before the Index timeout.")
}

func textResult(text string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}
}
func deliverResponse(text string) *mcp.CallToolResult {
	text = strings.TrimSpace(text)
	if text == "" {
		return pebbleFailure("No response was provided.")
	}
	return &mcp.CallToolResult{Meta: mcp.Meta{"coreSchema": 1}, Content: []mcp.Content{&mcp.TextContent{Text: text}}, StructuredContent: map[string]any{"output": text, "semanticResult": map[string]any{"type": "Response", "text": text}}}
}
func pebbleFailure(text string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Meta: mcp.Meta{"coreSchema": 1}, Content: []mcp.Content{&mcp.TextContent{Text: text}}, StructuredContent: map[string]any{"output": text, "semanticResult": map[string]any{"type": "GenericFailure", "userErrorMessage": text, "llmRecoverable": false, "forceFallbackTool": false}}, IsError: true}
}

func securityMiddleware(token string, next http.Handler) http.Handler {
	expected := []byte("Bearer " + token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Origin") != "" {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		provided := []byte(r.Header.Get("Authorization"))
		if len(provided) != len(expected) || subtle.ConstantTimeCompare(provided, expected) != 1 {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func main() {
	cfg, err := loadConfig(os.Getenv)
	if err != nil {
		log.Fatal(err)
	}
	handler := newMCPHandler(cfg, newDiscordClient(cfg.DiscordBotToken, cfg.DiscordChannelID, cfg.HermesBotID), workflowOptions{overallTimeout, threadTimeout, pollInterval})
	mux := http.NewServeMux()
	mux.Handle(mcpPath, handler)
	server := &http.Server{Addr: listenAddress, Handler: mux, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 2 * time.Minute}
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-stop
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
	}()
	log.Printf("listening on %s", listenAddress)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}
