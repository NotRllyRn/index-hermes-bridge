package main

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
	"unicode/utf16"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	listenAddress       = ":8080"
	mcpPath             = "/mcp"
	discordMessageLimit = 2000
	overallTimeout      = 50 * time.Second
	primaryThreadGrace  = 5 * time.Second
	pollInterval        = time.Second
	backgroundTimeout   = 30 * time.Minute
)

const serverInstructions = "Use `ask_hermes` for every request and pass the user's complete request unchanged. Hermes will answer asynchronously through a notification."

type config struct{ MCPBearerToken, DiscordBotToken, DiscordChannelID, HermesBotID, NtfyServerURL, NtfyTopic string }
type askHermesInput struct {
	Message string `json:"message" jsonschema:"The user's complete request to Hermes."`
}
type workflowOptions struct{ overall, thread, poll time.Duration }
type notificationSender interface {
	send(context.Context, string) error
}

type discordWorkflow interface {
	createRelayMessage(context.Context, string) (discordMessage, error)
	getThread(context.Context, string) (discordChannel, bool, error)
	createThreadFromMessage(context.Context, string, string) (discordChannel, error)
	createThreadMessage(context.Context, string, string) (discordMessage, error)
	completion(context.Context, messageRef) (completionState, error)
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
	cfg.NtfyServerURL = strings.TrimSpace(getenv("NTFY_SERVER_URL"))
	cfg.NtfyTopic = strings.Trim(strings.TrimSpace(getenv("NTFY_TOPIC")), "/")
	if (cfg.NtfyServerURL == "") != (cfg.NtfyTopic == "") {
		return config{}, errors.New("NTFY_SERVER_URL and NTFY_TOPIC must be set together")
	}
	if cfg.NtfyServerURL != "" {
		u, err := url.ParseRequestURI(cfg.NtfyServerURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return config{}, errors.New("NTFY_SERVER_URL must be an HTTP or HTTPS URL")
		}
	}
	return cfg, nil
}

func newMCPHandler(cfg config, discord discordWorkflow, notifications notificationSender, options workflowOptions) http.Handler {
	server := mcp.NewServer(&mcp.Implementation{Name: "index-hermes-bridge", Version: "1.0.0"}, &mcp.ServerOptions{Instructions: serverInstructions})
	falseValue, trueValue := false, true
	mcp.AddTool[askHermesInput, any](server, &mcp.Tool{
		Name:        "ask_hermes",
		Description: "Send the user's complete request to their Hermes agent through Discord and return Hermes's response.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: false, DestructiveHint: &falseValue, IdempotentHint: false, OpenWorldHint: &trueValue},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input askHermesInput) (*mcp.CallToolResult, any, error) {
		return askHermes(ctx, discord, notifications, cfg.DiscordChannelID, cfg.HermesBotID, input, options), nil, nil
	})
	streamable := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{
		JSONResponse:               true,
		MaxRequestBodyBytes:        64 << 10,
		DisableLocalhostProtection: true, // Bearer auth and the stricter no-Origin policy protect the Tailscale proxy path.
	})
	return securityMiddleware(cfg.MCPBearerToken, streamable)
}

func askHermes(parent context.Context, discord discordWorkflow, notifications notificationSender, parentChannelID, hermesID string, input askHermesInput, options workflowOptions) *mcp.CallToolResult {
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
	completionTarget := messageRef{ChannelID: parentChannelID, MessageID: source.ID}
	mode := "normal"
	threadDeadline := time.Now().Add(options.thread)
	var thread discordChannel
	var exists bool
	for {
		thread, exists, err = discord.getThread(ctx, source.ID)
		if err != nil {
			return pebbleFailure("Discord could not verify Hermes's thread.")
		}
		if exists || time.Now().After(threadDeadline) {
			break
		}
		if err := sleepContext(ctx, options.poll); err != nil {
			return timeoutFailure(parent)
		}
	}
	if !exists {
		mode = "fallback"
		log.Printf("Hermes thread absent; attempting relay fallback id=%s", source.ID)
		thread, err = discord.createThreadFromMessage(ctx, source.ID, fallbackThreadName(message))
		relayOwnsThread := err == nil
		if err != nil {
			log.Printf("fallback thread create failed id=%s error=%v", source.ID, err)
			thread, exists, _ = discord.getThread(ctx, source.ID)
			if !exists {
				var rate *discordRateLimitError
				if errors.As(err, &rate) {
					return pebbleFailure(rateLimitFailure(rate.RetryAfter))
				}
				return pebbleFailure("Discord could not create a thread for this request.")
			}
			relayOwnsThread = thread.OwnerID != hermesID
			if !relayOwnsThread {
				mode = "normal"
				log.Printf("fallback create raced with existing Hermes thread id=%s", source.ID)
			}
		}
		if relayOwnsThread {
			trigger, err := discord.createThreadMessage(ctx, source.ID, content)
			if err != nil {
				return pebbleFailure("The fallback thread was created, but Discord could not send the request inside it.")
			}
			completionTarget = messageRef{ChannelID: source.ID, MessageID: trigger.ID}
			log.Printf("relay fallback trigger created thread=%s message=%s", source.ID, trigger.ID)
		}
	}
	log.Printf("hermes thread detected id=%s owner=%s mode=%s", source.ID, thread.OwnerID, mode)
	for {
		state, err := discord.completion(ctx, completionTarget)
		if err != nil {
			if ctx.Err() != nil {
				return timeoutFailure(parent)
			}
			return pebbleFailure("Discord could not check whether Hermes received the request.")
		}
		if state == completionFailed {
			return pebbleFailure("Hermes reported that the request failed.")
		}
		if state == completionProcessing || state == completionSucceeded {
			log.Printf("hermes received thread=%s trigger=%s mode=%s", source.ID, completionTarget.MessageID, mode)
			if notifications != nil {
				go awaitAndNotify(discord, notifications, source.ID, completionTarget, state, options.poll)
			}
			return pebbleResponse("Hermes will reply soon")
		}
		if err := sleepContext(ctx, options.poll); err != nil {
			return timeoutFailure(parent)
		}
	}
}

func awaitAndNotify(discord discordWorkflow, notifications notificationSender, threadID string, target messageRef, state completionState, poll time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), backgroundTimeout)
	defer cancel()
	for state != completionSucceeded {
		var err error
		state, err = discord.completion(ctx, target)
		if err != nil {
			log.Printf("background completion check failed thread=%s error=%v", threadID, err)
			return
		}
		if state == completionFailed {
			log.Printf("hermes failed thread=%s", threadID)
			return
		}
		if err := sleepContext(ctx, poll); err != nil {
			log.Printf("background wait ended thread=%s error=%v", threadID, err)
			return
		}
	}
	text, err := discord.threadText(ctx, threadID)
	if err != nil || text == "" {
		log.Printf("background response read failed thread=%s error=%v", threadID, err)
		return
	}
	if err := notifications.send(ctx, text); err != nil {
		log.Printf("ntfy delivery failed thread=%s error=%v", threadID, err)
		return
	}
	log.Printf("ntfy notification sent thread=%s", threadID)
}

func fallbackThreadName(message string) string {
	message = strings.Join(strings.Fields(message), " ")
	if message == "" {
		return "Index"
	}
	units := 0
	return strings.Map(func(r rune) rune {
		units += utf16.RuneLen(r)
		if units > 80 {
			return -1
		}
		return r
	}, message)
}

func rateLimitFailure(delay time.Duration) string {
	minutes := (delay + time.Minute - 1) / time.Minute
	if minutes >= 1 {
		return fmt.Sprintf("Discord is rate-limiting thread creation. Please retry in about %d minute(s).", minutes)
	}
	return "Discord is rate-limiting thread creation. Please retry shortly."
}

func timeoutFailure(parent context.Context) *mcp.CallToolResult {
	if parent.Err() != nil {
		return pebbleFailure("The request was cancelled.")
	}
	return pebbleFailure("Hermes did not respond before the Index timeout.")
}

func pebbleResponse(text string) *mcp.CallToolResult {
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
	discord := newDiscordClient(cfg.DiscordBotToken, cfg.DiscordChannelID, cfg.HermesBotID)
	var notifications notificationSender
	if cfg.NtfyServerURL != "" {
		notifications = newNtfyClient(cfg.NtfyServerURL, cfg.NtfyTopic, &http.Client{Timeout: 10 * time.Second})
	}
	handler := newMCPHandler(cfg, discord, notifications, workflowOptions{overallTimeout, primaryThreadGrace, pollInterval})
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
