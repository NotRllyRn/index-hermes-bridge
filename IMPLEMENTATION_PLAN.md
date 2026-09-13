# Index Hermes Bridge — Software Design & Implementation Plan

> **Implementation update:** The notification flow now uses two tools. `ask_hermes(message)` returns ordinary MCP text so Index AI can summarize Hermes's result; `deliver_response(text)` emits the Pebble `SemanticResult.Response` containing that summary. This supersedes the original one-tool/direct-response requirements below.

## 1. Repository identity

**Repository name:** `index-hermes-bridge`

**Short GitHub description:**

> A lightweight MCP bridge that sends Pebble Index requests to Hermes through Discord and returns Hermes responses to Index.

**Suggested repository topics:**

```text
pebble
pebble-index
index-01
mcp
hermes-agent
discord
golang
tailscale
```

**Initial visibility:** private by default. If the owner explicitly wants the project open-sourced, switch it to public and retain the MIT license.

The implementation agent should create a **new repository under the currently authenticated user's GitHub account**. Do not fork `jcrabapple/pebble-index-mcp` or any other repository.

---

# 2. Executive design

The project has one purpose:

> Let a Pebble Index 01 user invoke Hermes from the ring, preserve Discord as the canonical Hermes conversation surface, wait for Hermes to finish, and return Hermes's Discord response to the same Index MCP tool call so the Pebble app can surface it as a notification.

The complete path is:

```text
Pebble Index 01
      |
      | voice recording
      v
Pebble mobile app
      |
      | transcription + Index cloud-agent reasoning
      v
Custom MCP Sandbox
      |
      | Streamable HTTP
      | Authorization: Bearer <MCP_BEARER_TOKEN>
      v
+----------------------------------------------------+
| index-hermes-bridge                               |
|                                                    |
| MCP tool: ask_hermes(message)                     |
|                                                    |
| 1. Post @Hermes + message to Discord #index       |
| 2. Record Discord source-message ID               |
| 3. Wait for Hermes-created thread with same ID    |
| 4. Wait for Hermes completion reaction            |
| 5. Read Hermes messages from the thread           |
| 6. Return them as Pebble SemanticResult.Response  |
+----------------------+-----------------------------+
                       |
                       | Discord REST API only
                       v
                 Discord #index
                       |
                       | @Hermes ...
                       v
                 Hermes Gateway
                       |
                       | DISCORD_AUTO_THREAD=true
                       v
                Discord thread M
                       |
                       | normal Hermes run:
                       | tools, skills, memory, etc.
                       v
                 Hermes response
                       |
                  Discord REST
                       |
                       v
              index-hermes-bridge
                       |
                 MCP tool result
                       |
                       v
                Pebble mobile app
                       |
                       v
                phone notification
```

Discord remains the source of truth. The bridge never calls Hermes's HTTP API directly and never reimplements Hermes session logic.

---

# 3. Product goals

Version 1 must satisfy all of the following.

1. Run as one lightweight Docker container.
2. Expose one MCP endpoint on port `8080`.
3. Use **Streamable HTTP MCP**, not legacy HTTP+SSE.
4. Authenticate Index-to-bridge calls with one bearer token.
5. Receive the user's transcribed request through one MCP tool.
6. Send exactly one top-level Discord message into the configured `#index` channel.
7. Explicitly mention Hermes.
8. Let Hermes's existing Discord gateway create and own the conversation thread.
9. Wait for Hermes to finish.
10. Read Hermes's final text from that exact Discord thread.
11. Return the response through the same MCP call.
12. Shape the MCP result so the Pebble app treats it as a direct response suitable for the completion notification.
13. Keep the application stateless.
14. Require no database, queue, Redis, filesystem volume, WebSocket gateway, Discord SDK, or Hermes API integration.
15. Be simple enough that the complete application should remain understandable in a few source files.

---

# 4. Explicit non-goals

Do **not** implement any of these in version 1:

- direct Hermes HTTP API calls
- a Discord Gateway/WebSocket connection
- Discord slash commands
- an interactive Discord bot
- a database
- persistent request state
- Redis
- a work queue
- background jobs
- a web UI
- an admin UI
- metrics infrastructure
- Prometheus
- OpenTelemetry
- OAuth
- user accounts
- multiple Discord servers
- multiple Hermes agents
- dynamic routing
- audio transcription
- audio storage
- ring-audio forwarding through a second ingress
- webhook ingestion
- webhook-to-Discord mode from the earlier design
- MCP resources
- MCP prompts unless later proven necessary
- more than one MCP tool
- configurable HTTP ports
- configurable polling strategies
- configurable Discord API base URL for production
- automatic creation of Discord channels
- automatic creation/configuration of the Hermes bot
- automatic Tailscale configuration
- CI/CD in the first version
- Kubernetes manifests
- Helm charts
- release automation

YAGNI applies aggressively. If a feature is not required to complete the exact ring → Discord → Hermes → Index round trip, it should not exist.

---

# 5. Important research findings

## 5.1 Pebble's remote MCP transport runs in the mobile app

Pebble's current open-source mobile application constructs its remote MCP transport in `HttpMcpIntegration`. It supports both `StreamableHttpClientTransport` and the older `SseClientTransport`, and attaches the user-configured `Authorization` header directly to HTTP requests.[^1]

This is important because the network path is effectively:

```text
Index -> phone -> MCP server
```

with the cloud agent participating in tool selection/reasoning, rather than the Pebble cloud directly opening the MCP TCP connection.

This strongly supports using a Tailscale-only MCP URL reachable from the phone.

## 5.2 Streamable HTTP is the correct transport

MCP's current transport specification says Streamable HTTP replaces the older HTTP+SSE transport. Streamable HTTP uses one MCP endpoint for HTTP POST/GET and is the modern transport intended for remote MCP servers.[^2]

Pebble explicitly supports it in its current source.[^1]

Therefore use:

```text
https://<tailscale-host>.ts.net/mcp
```

with **Streamable HTTP** selected in the Pebble MCP configuration.

Do not implement the deprecated standalone SSE transport.

## 5.3 Pebble currently pins MCP Kotlin SDK 0.15.0

The Pebble mobile repository currently pins:

```text
mcp = "0.15.0"
```

for the official Kotlin MCP SDK.[^3]

That client supports the 2025-era MCP protocol lifecycle. The official Go MCP SDK is backward compatible with protocol versions including `2025-11-25`, `2025-06-18`, `2025-03-26`, and `2024-11-05`. Current Go SDK v1.7.x also supports the newer 2026 protocol while preserving those older versions.[^4]

The bridge must be tested specifically against a **2025 protocol initialization flow**, because Pebble is the client that matters.

Do not accidentally configure the Go SDK in a 2026-only stateless mode.

## 5.4 Pebble tool calls have a 60-second default timeout

The exact MCP Kotlin SDK version used by Pebble defines:

```text
DEFAULT_REQUEST_TIMEOUT = 60 seconds
```

and the request implementation wraps the outbound request plus the wait for the final response in that timeout.[^5]

Pebble's `HttpMcpIntegration.callTool()` calls `client.callTool(...)` without supplying a custom timeout.[^1]

Therefore:

> The complete `ask_hermes` tool call must finish in less than 60 seconds.

The bridge should impose its own **50-second end-to-end deadline**. This leaves roughly 10 seconds of safety margin for network latency, serialization, Index-agent work, and client-side processing.

Do not attempt to support multi-minute Hermes tasks through the synchronous v1 design.

---

# 6. Audio feasibility decision

The user requested that audio and text be sent through MCP if possible.

## 6.1 MCP itself supports audio

MCP as a protocol supports audio content types. The official Go MCP SDK can represent audio content in tool results and other MCP content structures.[^6]

However, protocol capability alone is not enough.

## 6.2 Stock Pebble remote MCP does not forward Index recording audio

Pebble's current `HttpMcpIntegration.callTool()` contains the explicit comment:

```text
Session context is not forwarded to remote servers
```

and forwards only the JSON tool arguments selected by the agent.[^1]

The original recording lives in Pebble's local session/recording context. That context is not handed to remote MCP servers.

Additionally, Pebble's current remote MCP result adapter only extracts `TextContent` from ordinary tool results; other content types are logged as unsupported.[^1]

Therefore there is currently no clean supported route:

```text
Index raw audio -> custom remote MCP tool argument
```

using the stock app.

## 6.3 Version 1 decision: text only

Version 1 must use:

```text
ask_hermes(message: string)
```

where `message` is the transcription/user request produced by Index.

Do **not** add fake optional fields such as:

```text
audio_base64
audio_url
audio_bytes
```

because Pebble has no source value to populate them.

That would add dead API surface and violate YAGNI.

## 6.4 Future audio option

If Pebble later exposes recording audio in remote MCP tool arguments, extend the tool at that time.

If raw audio becomes important before Pebble adds that feature, the technically viable workaround is a **second Index webhook** configured to send audio or both audio and transcription. The bridge would then need to correlate the webhook event with a later MCP invocation.

That requires:

- event IDs
- temporary storage
- correlation state
- expiration
- failure cleanup
- race handling

It is intentionally excluded from v1 because it materially changes the architecture.

---

# 7. Why Discord remains the canonical Hermes interface

This design deliberately does not call Hermes through its API.

The desired behavioral contract is:

```text
Every Index request appears in #index.
Every Index request gets its own normal Hermes Discord thread.
Hermes behaves exactly as it does when manually mentioned in Discord.
The thread remains available as history after the Index notification is gone.
```

Calling Hermes's HTTP API directly would bypass that interaction model.

The bridge therefore behaves like a user/relay entering Discord, not like a Hermes backend client.

---

# 8. Discord integration model

Use a dedicated Discord application/bot identity but communicate with Discord through the **REST API only**.

Do not open a Discord Gateway connection.

## 8.1 Why no Gateway/WebSocket

A Gateway bot would introduce:

- long-lived WebSocket state
- heartbeats
- reconnect handling
- intents at runtime
- event dispatch
- more memory
- more code
- another failure mode

None of that is required.

The bridge can:

1. POST a message.
2. Poll for the thread.
3. Poll the source message for Hermes's completion reaction.
4. GET the thread's messages.

Discord's REST API provides every operation needed.

---

# 9. Deterministic Discord correlation

This is the most important simplification in the design.

When Discord creates a public thread from an existing guild text-channel message:

> the created thread has the **same ID as the source message**.[^7]

Therefore:

```text
relay message ID = M
Hermes thread ID = M
```

That makes `M` the correlation key for the entire request.

No database or in-memory correlation map is necessary.

Example:

```text
POST #index
    |
    v
Discord returns:
message.id = 1461234567890123456
    |
    v
Hermes creates thread from that message
    |
    v
thread.id = 1461234567890123456
```

Each MCP handler keeps `M` in a local variable for the lifetime of that request.

Concurrent requests remain naturally isolated.

---

# 10. Hermes configuration contract

The deployment assumes a current Hermes version with its Discord gateway configured approximately as follows:

```dotenv
DISCORD_REQUIRE_MENTION=true
DISCORD_ALLOW_BOTS=mentions
DISCORD_AUTO_THREAD=true
DISCORD_REACTIONS=true
```

Hermes documents:

- `DISCORD_ALLOW_BOTS=mentions` for trusted relay/bot messages that explicitly mention Hermes.
- `DISCORD_AUTO_THREAD=true` for creating a thread from a top-level mention.
- `DISCORD_REACTIONS=true` for lifecycle reactions such as processing/success/failure.[^8]

The configured `#index` channel must **not** appear in:

```text
DISCORD_FREE_RESPONSE_CHANNELS
```

or:

```text
DISCORD_NO_THREAD_CHANNELS
```

because those modes bypass the auto-thread behavior the bridge relies upon.[^8]

The bridge should fail closed rather than attempting to imitate Hermes's fallback behavior if auto-thread creation fails.

---

# 11. Discord bot permissions

Create one dedicated relay bot.

Grant only the permissions needed in `#index`:

```text
View Channel
Send Messages
Read Message History
```

The bot does not need to:

- manage channels
- manage messages
- manage threads
- send messages inside the thread
- moderate members
- manage roles
- use slash commands

## Message Content access

The bridge must read Hermes's message bodies from the thread.

Discord documents that applications without Message Content access may receive empty message `content`, `embeds`, `attachments`, and related fields.[^9]

Therefore enable **Message Content Intent** for the relay application in Discord's Developer Portal.

The bridge still does not connect to the Gateway; enabling Message Content is necessary so REST message reads contain content.

---

# 12. MCP server surface

Expose exactly one endpoint:

```text
/mcp
```

on:

```text
:8080
```

Do not expose a generic API.

Do not expose a REST `/ask` endpoint.

Do not add `/healthz` initially.

If operational experience later proves a health endpoint useful, add it then.

---

# 13. MCP tool definition

Expose exactly one tool:

```text
ask_hermes
```

Input:

```json
{
  "message": "string"
}
```

Recommended tool description:

> Send the user's complete request to their Hermes agent through Discord and return Hermes's response.

Recommended JSON schema:

```json
{
  "type": "object",
  "properties": {
    "message": {
      "type": "string",
      "description": "The user's complete request to Hermes."
    }
  },
  "required": ["message"],
  "additionalProperties": false
}
```

Do not expose:

```text
channel_id
hermes_id
timeout
audio
thread_name
discord_server
```

as tool inputs.

Those are deployment configuration or unsupported features, not decisions for the Index cloud model.

---

# 14. MCP server instructions

Pebble's remote MCP adapter exposes the MCP server's `serverInstructions` to its agent context.[^1]

Use a short server instruction such as:

> Use `ask_hermes` for every request in this sandbox. Pass the user's complete request in `message` without summarizing, rewriting, or omitting details. Return the tool result to the user as the answer.

Do not add a separate MCP Prompt object unless testing proves server instructions are insufficient.

Pebble currently has limitations around prompt handling, while server instructions already solve this need.[^1]

---

# 15. Tool annotations

Semantically, `ask_hermes`:

- has an external side effect: it sends a Discord message;
- is not destructive;
- is not guaranteed idempotent across separate MCP invocations;
- interacts with an external system.

Use annotations equivalent to:

```text
readOnlyHint: false
destructiveHint: false
idempotentHint: false
openWorldHint: true
```

Do not overbuild behavior around annotations. They are metadata.

---

# 16. Pebble-specific result shape

A normal generic MCP text result is not the best response for this project.

Pebble has a custom extension keyed by:

```json
"_meta": {
  "coreSchema": 1
}
```

Its remote MCP adapter recognizes this extension, reads `structuredContent.semanticResult`, and converts it into Pebble's internal `SemanticResult` model.[^1]

Pebble defines:

```text
SemanticResult.Response(text, question)
```

as:

> The agent replied with a message as its main action; surfaced directly, for example, in the completion notification.[^10]

Pebble's notification manager explicitly renders `SemanticResult.Response.text` as the completion-notification body.[^11]

Therefore the bridge should return success in this form.

## 16.1 Success result

Conceptual MCP result:

```json
{
  "content": [
    {
      "type": "text",
      "text": "Hermes's full response"
    }
  ],
  "structuredContent": {
    "output": "Hermes's full response",
    "semanticResult": {
      "type": "Response",
      "text": "Hermes's full response",
      "question": "Original Index request"
    }
  },
  "_meta": {
    "coreSchema": 1
  }
}
```

The exact discriminator field produced by Kotlin serialization must be verified by the integration test against the current Pebble app. The currently expected serialized subtype name is `Response` based on `@SerialName("Response")` in Pebble's source.[^10]

The implementation should preserve ordinary MCP `TextContent` as well, so non-Pebble clients still receive a sensible answer.

---

# 17. Error result shape

Expected runtime failures should be returned as **tool results marked as errors**, not as arbitrary HTTP failures.

Examples:

- Hermes never created a thread.
- Discord rejected the relay message.
- Hermes reacted with failure.
- Hermes did not complete within the deadline.
- Hermes completed without any readable text.
- the input exceeds Discord's message length.

Use a Pebble `GenericFailure` semantic result:

```json
{
  "content": [
    {
      "type": "text",
      "text": "Hermes did not respond before the Index timeout."
    }
  ],
  "structuredContent": {
    "output": "Hermes did not respond before the Index timeout.",
    "semanticResult": {
      "type": "GenericFailure",
      "userErrorMessage": "Hermes did not respond before the Index timeout.",
      "llmRecoverable": false,
      "forceFallbackTool": false
    }
  },
  "_meta": {
    "coreSchema": 1
  },
  "isError": true
}
```

Pebble's notification manager knows how to display `GenericFailure.userErrorMessage` directly.[^11]

For expected operational errors, return:

```text
CallToolResult, nil
```

rather than a Go handler error if the SDK otherwise converts the error into a generic protocol error that loses the Pebble-specific metadata.

Unexpected programming/protocol errors may still be actual MCP errors.

---

# 18. Why this Pebble result format matters

Pebble has a currently open issue where generic read-only/supporting-data MCP results can result in a notification that displays the original question rather than the returned answer.[^12]

The project should not try to patch the Pebble client.

Instead it should use the semantic contract Pebble already provides for direct responses:

```text
SemanticResult.Response
```

This is both simpler and more correct.

Another open Pebble issue describes successful third-party MCP calls being represented as “No action taken” when they do not use Pebble's semantic result extension.[^13]

Returning `coreSchema: 1` with `Response` is therefore part of the v1 protocol contract, not optional polish.

---

# 19. Authentication

The MCP endpoint must require a pre-shared bearer token.

Generate it with:

```bash
openssl rand -hex 32
```

This produces 256 random bits represented as 64 hexadecimal characters.

Pebble MCP configuration:

```text
Authorization:
Bearer <secret>
```

Environment:

```dotenv
MCP_BEARER_TOKEN="<same-secret>"
```

Validate the complete header:

```text
Authorization: Bearer <secret>
```

using constant-time comparison.

In Go:

```text
crypto/subtle.ConstantTimeCompare
```

is sufficient.

Never log the token.

Never return it in error responses.

There is no need for OAuth, API-key databases, users, refresh tokens, or login pages.

---

# 20. Origin validation

The MCP Streamable HTTP specification requires servers to validate `Origin` when present to protect against DNS rebinding.[^2]

This service has no legitimate browser client.

The simplest policy is:

```text
Origin header absent:
    continue

Origin header present:
    403 Forbidden
```

This avoids:

- hostname allowlists
- browser CORS configuration
- `MCP_ALLOWED_HOSTS`
- dynamic local-vs-Tailscale hostname problems

Pebble's native HTTP client is not a browser and should not require an `Origin` header.

This behavior must be verified in the P0 real-device integration test.

If Pebble unexpectedly sends an Origin, update the rule to accept exactly the observed trusted origin rather than introducing a wildcard.

---

# 21. No `MCP_ALLOWED_HOSTS`

Do not copy `MCP_ALLOWED_HOSTS` from `jcrabapple/pebble-index-mcp`.

That project uses framework-specific host/DNS-rebinding configuration.

This implementation does not need an environment variable for changing client IPs or local/Tailscale routing.

The host the phone uses should be stable:

```text
https://<tailscale-node>.<tailnet>.ts.net/mcp
```

whether the phone is at home or away.

The user's physical location does not change the HTTP Host header configured in Pebble.

---

# 22. Tailscale architecture

Prefer **Tailscale Serve**, not Funnel.

Tailscale Serve makes the service available inside the tailnet and automatically proxies a local HTTP port over HTTPS. Tailscale documents:

```bash
tailscale serve 3000
```

as a way to expose a local server at a stable `*.ts.net` HTTPS URL, and `--bg` to persist it in the background.[^14]

For this project:

```bash
tailscale serve --bg 8080
```

should proxy:

```text
https://<node>.<tailnet>.ts.net/
        ->
http://127.0.0.1:8080/
```

Then Pebble uses:

```text
https://<node>.<tailnet>.ts.net/mcp
```

Tailscale ACLs still apply.[^14]

Do not use Tailscale Funnel unless a public endpoint becomes necessary.

---

# 23. Docker host binding

Inside the container, the Go process listens on:

```text
0.0.0.0:8080
```

because it must accept traffic arriving through Docker networking.

On the Docker host, publish it only to loopback:

```yaml
ports:
  - "127.0.0.1:8080:8080"
```

This means:

- the LAN cannot directly hit the service on the Docker host's LAN IP;
- Tailscale Serve on the host can proxy to it;
- the MCP bearer token still provides application-level authentication.

This is both simpler and safer than exposing `0.0.0.0:8080` on the host.

---

# 24. Discord outbound message

Use:

```http
POST /api/v10/channels/{DISCORD_CHANNEL_ID}/messages
Authorization: Bot <DISCORD_BOT_TOKEN>
Content-Type: application/json
```

The content is:

```text
<@HERMES_BOT_ID> <message>
```

Discord limits normal message content to 2,000 characters.[^9]

If the complete mention + transcription exceeds that limit, return a tool error.

Do not split the input into multiple messages because that would break:

```text
one Index invocation
=
one top-level message
=
one Hermes thread
```

---

# 25. Allowed mentions

The transcription is user-controlled text.

A transcription could contain text resembling:

```text
@everyone
@here
<@other-user>
<@&role>
```

The bridge must not allow arbitrary mentions.

Set Discord `allowed_mentions` to permit only the configured Hermes user ID.

Conceptually:

```json
{
  "content": "<@1234> user text",
  "allowed_mentions": {
    "users": ["1234"]
  }
}
```

Do not use a broad `parse` rule.

This guarantees the relay intentionally pings Hermes but does not accidentally ping everyone else.

---

# 26. Discord request idempotency

When the bridge creates the top-level relay message, use Discord's message nonce support.

Generate one random 24-character hex nonce:

```text
12 random bytes -> hex -> 24 characters
```

using Go's:

```text
crypto/rand
encoding/hex
```

Send:

```json
{
  "nonce": "<24-char-hex>",
  "enforce_nonce": true
}
```

This gives a simple defense against the classic ambiguous HTTP failure:

```text
Did Discord create the message before the connection failed?
```

If the bridge makes one bounded retry using the same nonce, Discord can return the existing message instead of creating another one.

This is preferable to implementing a database-backed idempotency system.

---

# 27. Discord REST user agent

Discord's HTTP API documentation requires applications to send a valid descriptive `User-Agent` and warns that requests without one may be blocked.[^15]

Use a constant such as:

```text
DiscordBot (https://github.com/<owner>/index-hermes-bridge, <version>)
```

If the repository URL is not known at build time, a minimal stable value can be used initially and replaced once the repository exists.

Do not include secrets.

---

# 28. Discord retry policy

Keep retries intentionally bounded.

For each Discord REST request:

### Retry once when:

- HTTP `429`
- transient network error
- selected `5xx` responses

For `429`, honor Discord's returned `retry_after` delay before retrying.[^16]

### Do not retry:

- `400`
- `401`
- `403`
- `404`, except where `404` has explicit polling semantics for a thread that does not exist yet

Maximum:

```text
2 total attempts per individual REST operation
```

No retry library.

No exponential-backoff package.

No unbounded loop.

All retries must remain inside the overall 50-second MCP deadline.

---

# 29. Waiting for the Hermes thread

After Discord returns source message ID `M`, the bridge knows the expected thread ID is also `M`.[^7]

Poll:

```text
GET /channels/M
```

or an equivalent thread-read endpoint once per second.

Treat:

```text
404
```

as:

```text
thread not created yet
```

during this phase.

Thread creation budget:

```text
10 seconds maximum
```

If the canonical thread does not exist after 10 seconds:

```text
return GenericFailure
```

Do not:

- scan all server threads
- search by thread title
- inspect other parent-channel messages
- guess that an inline response belongs to this request
- create the thread yourself

Hermes has documented edge cases where Discord failures can cause auto-thread fallback behavior.[^17]

The bridge should prefer correctness over clever recovery.

---

# 30. Completion signal

With:

```dotenv
DISCORD_REACTIONS=true
```

Hermes uses message reactions as execution state.[^8]

The bridge should rely on the source message's final Hermes reaction:

```text
✅ -> Hermes completed successfully
❌ -> Hermes failed
```

The bridge should not consider “some Hermes text appeared” sufficient completion because Hermes may emit more than one Discord message.

## Efficient reaction verification

Every second:

1. GET the source message.
2. Inspect its reaction summaries.
3. If neither `✅` nor `❌` exists, continue.
4. If a final reaction appears, optionally call Discord's reaction-user endpoint once to confirm that `HERMES_BOT_ID` is one of the users who added that emoji.
5. Act on the result.

This avoids an extra reaction-users call on every poll.

It also prevents a manual user-added `✅` from prematurely completing the request.

---

# 31. Success response collection

Once Hermes completion is verified:

```http
GET /channels/{THREAD_ID}/messages?limit=100
```

Discord's message endpoint supports reading channel/thread history.[^9]

Filter:

```text
message.author.id == HERMES_BOT_ID
```

and:

```text
strings.TrimSpace(message.content) != ""
```

Discord returns recent messages newest-first. Reverse the filtered results into chronological order.

Join them with:

```text
"\n\n"
```

This preserves multi-message Hermes responses instead of returning only the final chunk.

Do not include:

- relay-bot text
- thread-starter system message
- other users' replies
- empty Hermes messages
- embeds
- attachments

in version 1.

If Hermes completed but produced no non-empty text:

```text
return GenericFailure:
"Hermes completed without a text response."
```

---

# 32. Why not parse embeds and attachments

The Index notification ultimately requires text.

Supporting Discord embeds, images, files, voice messages, or other rich response types would require conversion semantics that are not currently needed.

YAGNI:

```text
Hermes text content -> supported
everything else -> ignored
```

If Hermes commonly answers only through embeds in real usage, add embed text later based on observed data.

---

# 33. End-to-end timeout state machine

Hard application deadline:

```text
50 seconds
```

Pebble's MCP client currently times requests out at 60 seconds.[^5]

Suggested state machine:

```text
T+0s
  |
  | validate message
  v
POST Discord relay message
  |
  | receive M
  v
WAIT_FOR_THREAD
  |
  | poll every 1s
  | max 10s
  |
  +--> thread M appears
          |
          v
WAIT_FOR_HERMES
          |
          | poll parent message every 1s
          |
          +--> ❌ -> failure
          |
          +--> ✅
          |      |
          |      v
          |   read thread messages
          |      |
          |      v
          |   success
          |
          +--> T+50s -> timeout failure
```

Use one parent:

```go
ctx, cancel := context.WithTimeout(requestContext, 50*time.Second)
```

All Discord requests derive from this context.

Do not start independent background work after the MCP caller disconnects.

If the request context is cancelled, stop polling and return.

---

# 34. Why no asynchronous queue

An asynchronous job queue is a poor fit because the desired UX is:

```text
one MCP request
    ->
wait
    ->
same MCP response
    ->
Index notification
```

A queue would force a second polling/subscription channel back to Pebble that does not exist in the current UX.

If Hermes work regularly takes longer than 50 seconds, that is a product constraint that requires revisiting the interaction model rather than adding a hidden queue.

---

# 35. Concurrency model

Use normal Go HTTP concurrency.

Each MCP request owns:

```text
one goroutine
one 50-second context
one Discord message ID
one polling loop
```

There is no global mutable request map.

Multiple ring requests can run concurrently:

```text
request A -> message/thread A
request B -> message/thread B
```

The Discord IDs keep them separated.

No semaphore is needed for personal use.

If concurrency later becomes abusive, enforce a limit then.

---

# 36. Application dependencies

Use:

```text
Go standard library
github.com/modelcontextprotocol/go-sdk/mcp
```

and nothing else unless implementation proves the official SDK needs a direct supporting module through Go's dependency graph.

Do not add:

- Gin
- Echo
- Fiber
- Chi
- Cobra
- Viper
- DiscordGo
- GORM
- Redis clients
- logging frameworks
- retry frameworks

The official MCP Go SDK is justified because reimplementing MCP/JSON-RPC/Streamable HTTP manually would be less simple and less reliable.

The official SDK is maintained by the MCP project in collaboration with Google and documents compatibility with Pebble's supported older protocol versions.[^4]

---

# 37. Go version

At the time of this design, Go `1.27.1` is the current stable minor release.[^18]

Use:

```text
go 1.27
```

in `go.mod` unless the selected stable MCP SDK version requires something else.

Docker builder:

```dockerfile
FROM golang:1.27-alpine AS build
```

Pinning an exact patch version in Docker is optional; major/minor pinning is an acceptable balance for this small project.

---

# 38. MCP Go SDK version

At implementation time:

1. inspect the current stable official `modelcontextprotocol/go-sdk` release;
2. pin a specific stable release in `go.mod`;
3. confirm its compatibility table still includes `2025-11-25`;
4. run the Pebble-protocol integration tests before proceeding.

As of this design, v1.7.x supports:

```text
2026-07-28
2025-11-25
2025-06-18
2025-03-26
2024-11-05
```

[^4]

## Critical configuration

Do **not** enable `StreamableHTTPOptions.Stateless=true` for v1 unless real-device compatibility has been proven.

The current Go SDK documents that its new stateless mode is required for the new 2026 lifecycle, whereas older protocol versions use the legacy initialized session lifecycle.[^19]

Pebble currently uses the older initialization model.

Use stateful Streamable HTTP compatibility.

---

# 39. MCP request-body limit

The Go SDK's default Streamable HTTP request-body limit is currently 4 MiB.[^20]

This bridge only expects:

```json
{"message":"text"}
```

Set a much smaller limit if the SDK API makes that clean:

```text
64 KiB
```

This is far beyond realistic Index transcription sizes.

If configuring this requires invasive work around the SDK, leave the SDK default rather than adding custom body-buffering middleware.

YAGNI favors using the SDK capability directly or doing nothing.

---

# 40. Long-running Streamable HTTP behavior

There is a current Go MCP SDK issue noting that long-running Streamable HTTP POST tool calls may remain quiet until the request completes, which can make some intermediaries perceive a dead connection.[^21]

For this project:

- the maximum server wait is only 50 seconds;
- Tailscale Serve is the expected proxy;
- the Pebble MCP client itself waits up to 60 seconds.

Do not add synthetic keepalive machinery initially.

The P0 real-device test must specifically prove that a ~30–50 second tool call survives the actual Pebble → Tailscale Serve → container path.

If it does not, revisit transport/session handling based on the observed failure.

---

# 41. Configuration

Commit:

```text
example.env
```

Do not commit:

```text
.env
```

Use exactly four required application settings:

```dotenv
MCP_BEARER_TOKEN="replace-with-openssl-rand-hex-32"
DISCORD_BOT_TOKEN="replace-with-relay-bot-token"
DISCORD_CHANNEL_ID="123456789012345678"
HERMES_BOT_ID="123456789012345678"
```

Do not add configuration for:

```text
PORT
MCP_ALLOWED_HOSTS
MCP_PATH
DISCORD_API_URL
DISCORD_GUILD_ID
HERMES_MENTION
POLL_INTERVAL
THREAD_TIMEOUT
RESPONSE_TIMEOUT
LOG_LEVEL
```

unless real deployment requires it.

Port/path/timeouts are stable implementation constants.

The mention is constructed from:

```text
HERMES_BOT_ID
```

because the same ID is also needed to identify Hermes responses.

---

# 42. Startup validation

On startup:

1. load all four required env vars;
2. `TrimSpace`;
3. reject empty values;
4. optionally validate Discord IDs contain only decimal digits;
5. require the MCP token to be at least a sensible length;
6. start the server.

Do not make startup dependent on Discord being reachable.

Do not send a test Discord message at startup.

Do not call Discord `GET /users/@me` unless later needed for diagnosis.

Fail configuration errors immediately with clear logs.

---

# 43. Repository structure

Keep the repository flat:

```text
index-hermes-bridge/
├── main.go
├── discord.go
├── main_test.go
├── discord_test.go
├── go.mod
├── go.sum
├── Dockerfile
├── compose.example.yaml
├── example.env
├── .gitignore
├── .dockerignore
├── README.md
├── LICENSE
└── IMPLEMENTATION_PLAN.md
```

Avoid:

```text
cmd/
internal/
pkg/
handlers/
controllers/
services/
repositories/
models/
config/
```

This application is not large enough to justify them.

If `main.go` + `discord.go` together remain comfortably under ~500 lines, the flat layout is preferable.

---

# 44. Suggested code responsibilities

## `main.go`

Own:

- constants
- config loading
- MCP server construction
- bearer/origin middleware
- `ask_hermes` handler
- Pebble success/error result builders
- HTTP server startup

## `discord.go`

Own:

- minimal Discord REST structs
- request helper
- relay-message creation
- thread existence check
- source-message reaction check
- reaction-user verification
- thread-message retrieval
- bounded retry logic

Do not create generic interfaces merely for “clean architecture.”

For tests, inject:

```text
*http.Client
Discord API base URL
```

through an internal constructor/struct.

Production uses the constant Discord API URL.

---

# 45. Minimal application types

Something roughly this small is enough:

```go
type Config struct {
    MCPBearerToken string
    DiscordBotToken string
    DiscordChannelID string
    HermesBotID string
}

type DiscordClient struct {
    token string
    channelID string
    hermesID string
    baseURL string
    http *http.Client
}

type askHermesInput struct {
    Message string `json:"message"`
}
```

Avoid interface types until tests demonstrate a concrete benefit.

`httptest.Server` plus an injectable `baseURL` provides enough testability.

---

# 46. Core `ask_hermes` algorithm

Pseudocode:

```text
askHermes(ctx, input):

    message = trim(input.message)

    if message == "":
        return pebbleFailure("No message was provided.")

    discordText = "<@" + HERMES_BOT_ID + "> " + message

    if discordText exceeds Discord content limit:
        return pebbleFailure("The Index request is too long for Discord.")

    ctx = withTimeout(ctx, 50 seconds)

    sourceMessage = discord.sendRelay(ctx, discordText)

    threadID = sourceMessage.ID

    if !waitForThread(ctx, threadID, 10 seconds):
        return pebbleFailure("Hermes did not create the expected Discord thread.")

    loop until ctx deadline:
        state = discord.getHermesCompletionState(sourceMessage.ID)

        if state == FAILED:
            return pebbleFailure("Hermes reported that the request failed.")

        if state == COMPLETE:
            messages = discord.getThreadMessages(threadID)
            answer = collectHermesText(messages)

            if answer == "":
                return pebbleFailure("Hermes completed without a text response.")

            return pebbleResponse(answer, originalQuestion)

        sleep 1 second

    return pebbleFailure("Hermes did not respond before the Index timeout.")
```

This should remain visibly linear in the actual code.

---

# 47. Discord REST structures

Define only fields the program reads.

Example conceptual structures:

```go
type discordMessage struct {
    ID        string             `json:"id"`
    Content   string             `json:"content"`
    Author    discordUser        `json:"author"`
    Reactions []discordReaction  `json:"reactions"`
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
```

Do not model the full Discord API.

---

# 48. REST operations required

The production client needs only a handful of operations:

```text
CreateRelayMessage
ThreadExists
GetSourceMessage
GetReactionUsers
GetThreadMessages
```

A small private helper handles:

```text
HTTP request
Authorization header
User-Agent
JSON marshal/unmarshal
429 / transient retry
status validation
```

No Discord SDK is warranted.

---

# 49. Discord rate-limit handling

Discord expects clients to honor rate limits and `retry_after` after HTTP 429.[^16]

Implement the narrow policy:

```text
if status == 429:
    parse retry_after
    wait if context budget permits
    retry once
```

Do not implement Discord bucket tracking.

This is a personal low-volume application, so full distributed rate-limit machinery is unnecessary.

---

# 50. Hermes auto-thread failure behavior

Real Hermes issue reports show that auto-thread creation can occasionally fail because of Discord/network errors and Hermes may fall back to inline behavior or potentially create ambiguous fallback state.[^17]

The bridge must not chase those cases.

Canonical success condition:

```text
thread ID == source message ID
```

If that thread never exists:

```text
tool failure
```

This makes response correlation deterministic and prevents the wrong Discord answer from being returned to Index.

---

# 51. Logging

Use the Go standard library.

Either:

```text
log
```

or:

```text
log/slog
```

with no external logger.

Recommended operational logs:

```text
listening on :8080
received ask_hermes request
discord message created id=...
hermes thread detected id=...
hermes completed id=...
request failed id=... reason=timeout
discord request failed status=...
```

Do not log:

- MCP bearer token
- Discord bot token
- Authorization headers
- full user transcription by default
- full Hermes answer by default

Discord message IDs are safe and useful correlation identifiers.

---

# 52. HTTP server configuration

Use standard `net/http`.

Conceptually:

```go
srv := &http.Server{
    Addr:              ":8080",
    Handler:           mux,
    ReadHeaderTimeout: 5 * time.Second,
    IdleTimeout:       2 * time.Minute,
}
```

Be careful with `WriteTimeout`.

A 30-second write timeout would break valid 50-second MCP calls.

Either:

- omit `WriteTimeout`, or
- set it comfortably above the MCP client timeout.

Simplest v1: omit it.

---

# 53. Graceful shutdown

Implement standard graceful shutdown for:

```text
SIGINT
SIGTERM
```

with a short shutdown window such as:

```text
5 seconds
```

Do not wait 50 seconds for active Hermes jobs during container shutdown.

Container shutdown is exceptional; terminate cleanly enough for Docker.

This is small and idiomatic rather than architectural complexity.

---

# 54. Docker design

Use a multi-stage build.

Recommended:

```dockerfile
FROM golang:1.27-alpine AS build

WORKDIR /src

RUN apk add --no-cache ca-certificates

COPY go.mod go.sum ./
RUN go mod download

COPY *.go ./

RUN CGO_ENABLED=0 GOOS=linux \
    go build -trimpath -ldflags="-s -w" \
    -o /out/index-hermes-bridge .

FROM scratch

COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/index-hermes-bridge /index-hermes-bridge

USER 65532:65532

EXPOSE 8080

ENTRYPOINT ["/index-hermes-bridge"]
```

The CA bundle is required for HTTPS calls to Discord.

The final image contains:

- one static Go binary
- trusted CA certificates

and nothing else.

No shell.

No package manager.

No interpreter.

No writable volume.

---

# 55. Docker resource philosophy

Do not add artificial CPU/memory limits to the repository's example Compose file.

Measure first.

Expected behavior:

- near-zero CPU while idle;
- memory dominated by Go runtime + MCP server state;
- no disk growth;
- short CPU/network bursts while processing a ring request.

Use:

```bash
docker stats index-hermes-bridge
```

during acceptance testing and record observed values in the implementation PR/notes if desired.

Do not optimize below measured need.

---

# 56. `compose.example.yaml`

The repository must contain:

```text
compose.example.yaml
```

and must **not** commit a real:

```text
compose.yaml
```

Recommended complete content:

```yaml
services:
  index-hermes-bridge:
    build: .
    container_name: index-hermes-bridge
    env_file:
      - .env
    ports:
      - "127.0.0.1:8080:8080"
    restart: unless-stopped
```

No:

- volumes
- privileged mode
- extra capabilities
- custom networks
- resource reservations
- dependency containers
- database
- healthcheck command requiring a shell

Setup:

```bash
cp compose.example.yaml compose.yaml
cp example.env .env
docker compose up -d --build
```

---

# 57. `.gitignore`

Minimal:

```gitignore
.env
compose.yaml
index-hermes-bridge
```

Optionally include editor artifacts only if they actually occur.

Do not paste a huge generic Go `.gitignore`.

---

# 58. `.dockerignore`

Recommended:

```dockerignore
.git
.gitignore
.env
compose.yaml
README.md
IMPLEMENTATION_PLAN.md
LICENSE
```

Only build inputs should be sent to the Docker daemon.

---

# 59. Minimal README specification

The README should remain operational and short.

Recommended content:

~~~~markdown
# index-hermes-bridge

A lightweight MCP bridge that sends Pebble Index requests to Hermes through Discord and returns Hermes responses to Index.

## How it works

Index -> MCP -> #index -> @Hermes -> Hermes thread -> MCP -> Index notification

Discord remains the canonical Hermes conversation history.

## Requirements

- Pebble Index 01
- Pebble mobile app with custom MCP support
- Hermes Agent with Discord enabled
- a Discord relay bot
- Docker
- Tailscale (recommended)

Hermes should have:

```env
DISCORD_REQUIRE_MENTION=true
DISCORD_ALLOW_BOTS=mentions
DISCORD_AUTO_THREAD=true
DISCORD_REACTIONS=true
```

The target channel must not be configured as a Hermes free-response or no-thread channel.

## Discord bot

Create a Discord application/bot and give it access to the target `#index` channel with:

- View Channel
- Send Messages
- Read Message History

Enable Message Content Intent.

Record:

- relay bot token
- `#index` channel ID
- Hermes bot user ID

## Setup

```bash
git clone <repo-url>
cd index-hermes-bridge

cp example.env .env
cp compose.example.yaml compose.yaml
```

Generate the MCP secret:

```bash
openssl rand -hex 32
```

Edit `.env`:

```env
MCP_BEARER_TOKEN="..."
DISCORD_BOT_TOKEN="..."
DISCORD_CHANNEL_ID="..."
HERMES_BOT_ID="..."
```

Start:

```bash
docker compose up -d --build
```

## Tailscale

Expose the local container to your tailnet:

```bash
tailscale serve --bg 8080
```

Use the resulting URL plus `/mcp`, for example:

```text
https://my-server.example-tailnet.ts.net/mcp
```

## Pebble Index

Create a custom MCP server:

- URL: `https://<tailscale-host>/mcp`
- Transport: **Streamable HTTP**
- Authorization: `Bearer <MCP_BEARER_TOKEN>`

Assign it to the desired Index sandbox/gesture.

The bridge exposes one tool:

```text
ask_hermes(message)
```

## Audio

Current Pebble remote MCP integrations do not forward the raw Index recording to custom MCP servers. Version 1 therefore sends the transcription/text only.

## License

MIT
~~~~

Do not copy this full implementation document into README.

---

# 60. Unit and integration testing strategy

Testing should be thorough at boundaries but small in implementation.

Use:

```text
testing
net/http/httptest
```

No third-party test framework.

---

# 61. Configuration tests

Test:

1. all valid variables load;
2. missing MCP token fails;
3. missing Discord token fails;
4. missing channel ID fails;
5. missing Hermes ID fails;
6. malformed nonnumeric Discord IDs fail if numeric validation is implemented.

---

# 62. Authentication tests

Test:

```text
no Authorization -> 401
wrong bearer -> 401
correct bearer -> MCP handler reached
```

Verify comparisons do not accidentally accept:

```text
bearer lowercase scheme variations
extra whitespace
prefix matches
empty token
```

Exact configured header behavior is sufficient.

---

# 63. Origin tests

Test:

```text
no Origin -> allowed
Origin present -> 403
```

The real-device test determines whether Pebble sends Origin.

---

# 64. MCP compatibility tests

These are critical.

Use the official Go MCP client or wire-level HTTP to exercise the server with a Pebble-compatible protocol version.

Verify:

1. Streamable HTTP initialization succeeds with `2025-11-25` or the exact version negotiated by current Pebble.
2. `tools/list` returns exactly one tool.
3. `nextCursor` is absent.
4. tool name is `ask_hermes`.
5. input schema accepts only `message`.
6. a successful call returns the expected `coreSchema` v1 structure.
7. failure returns `GenericFailure`.
8. server uses the stateful legacy lifecycle expected by Pebble.

Why no pagination:

Pebble's current remote MCP client explicitly throws a `TODO("Handle pagination")` if `tools/list` returns a `nextCursor`.[^1]

One tool naturally avoids this entire issue.

---

# 65. Pebble success-result wire test

Assert the serialized success result contains:

```json
"_meta": {
  "coreSchema": 1
}
```

and:

```json
"structuredContent": {
  "output": "...",
  "semanticResult": {
    "type": "Response",
    "text": "...",
    "question": "..."
  }
}
```

The exact JSON structure matters more than Go's internal types.

This test protects against future MCP SDK refactors accidentally removing metadata.

---

# 66. Discord create-message test

Fake Discord server receives:

```text
POST /channels/<channel>/messages
```

Assert:

- `Authorization: Bot ...`
- valid User-Agent
- content begins with exact `<@HERMES_BOT_ID>`
- original request follows
- `allowed_mentions` permits only Hermes
- nonce is present
- `enforce_nonce=true`

Fake response returns a source message ID.

---

# 67. Discord retry tests

Test:

### 429

First response:

```text
429
retry_after = short test interval
```

Second:

```text
200
```

Assert only one retry and same nonce.

### transient 5xx

One retry maximum.

### 403

No retry.

---

# 68. Thread-correlation test

Create source message:

```text
M
```

Fake Discord:

```text
GET /channels/M -> 404 twice
GET /channels/M -> 200
```

Assert bridge continues and never searches for a different thread ID.

---

# 69. Hermes completion tests

Test:

### success

Source message gets verified Hermes `✅`.

Thread contains:

```text
Hermes: part one
Hermes: part two
```

Expect:

```text
part one

part two
```

### failure

Hermes `❌` -> Pebble GenericFailure.

### unrelated reaction

A different user added `✅`.

Do not complete until Hermes reaction is present.

### unrelated thread user

Ignore non-Hermes thread messages.

### no text

Hermes completes but only empty/non-text responses exist -> GenericFailure.

---

# 70. Timeout tests

Do not make unit tests wait 50 seconds.

Internal functions should accept testable duration values through unexported parameters or a small internal options structure, while production constants remain fixed.

Test:

- thread creation timeout
- overall completion timeout
- request context cancellation

Do not expose these test knobs as environment configuration.

---

# 71. Real-device P0 integration test

Before declaring the project complete, test on the actual Pebble Index.

This test is mandatory because several critical behaviors belong to Pebble's app rather than the bridge.

## Test procedure

1. Start the container.
2. Run Tailscale Serve.
3. Confirm the phone is connected to the same tailnet.
4. Configure Pebble:
   - Streamable HTTP
   - Tailscale URL `/mcp`
   - bearer token
5. Configure the chosen Index gesture/sandbox.
6. Record:
   > “Ask Hermes what two plus two is. Answer with only the number.”
7. Observe:
   - bridge receives MCP tool call;
   - bridge posts to `#index`;
   - message explicitly mentions Hermes;
   - Hermes creates canonical thread;
   - Hermes posts response in thread;
   - Hermes marks completion;
   - bridge reads the response;
   - MCP call completes under 60 seconds;
   - Pebble notification displays Hermes's answer, not the original question.

This is the v1 release gate.

---

# 72. Long-duration integration test

After basic success, make Hermes deliberately take ~30 seconds.

Verify:

- Tailscale Serve does not terminate the connection;
- Go Streamable HTTP server keeps the request alive;
- Pebble waits;
- final result reaches the notification.

Then test close to the application boundary, e.g. ~45 seconds.

Do not attempt to exceed the 50-second server deadline.

---

# 73. Failure-mode integration tests

Manually verify at least once:

1. wrong MCP bearer token -> Index cannot invoke tool;
2. Discord bot token wrong -> clear tool failure;
3. Hermes offline -> 10-second no-thread failure or later timeout;
4. Hermes fails task -> failure notification;
5. phone not on tailnet -> MCP unreachable;
6. malformed/overlong request -> no duplicate threads.

---

# 74. Resource-efficiency acceptance

Run:

```bash
docker stats index-hermes-bridge
```

while:

- idle;
- handling one request;
- handling two overlapping requests.

Record that:

- idle CPU is effectively zero;
- memory remains small;
- no filesystem data grows;
- process returns to idle after requests.

Do not set an arbitrary memory optimization target unless measured use suggests a problem.

---

# 75. Security acceptance

Verify:

- `.env` not tracked;
- `compose.yaml` not tracked;
- container runs non-root;
- host port bound to `127.0.0.1`;
- Tailscale Serve, not Funnel;
- MCP endpoint rejects missing token;
- `Origin` policy works;
- Discord bot only has minimal channel permissions;
- accidental `@everyone` in transcription does not notify everyone;
- no secrets in logs;
- no Hermes HTTP port is exposed by this project.

---

# 76. Failure behavior table

| Failure | Behavior |
|---|---|
| Missing MCP bearer | HTTP `401` before MCP |
| Browser Origin present | HTTP `403` |
| Empty message | MCP `GenericFailure` |
| Input > Discord limit | MCP `GenericFailure` |
| Discord 401/403 | MCP `GenericFailure`; no retry |
| Discord 429 | honor delay; one retry |
| Discord transient error | one retry |
| Canonical thread absent after 10s | MCP `GenericFailure` |
| Hermes `❌` | MCP `GenericFailure` |
| Hermes no text after success | MCP `GenericFailure` |
| Hermes not finished by 50s | MCP `GenericFailure` |
| Pebble cancels request | stop all work immediately |
| Container shuts down | cancel/terminate active calls |
| Audio requested | unavailable in v1; text transcription only |

---

# 77. Implementation phases

## Phase 0 — compatibility spike

Before polishing the repo, prove the unknowns with the smallest possible code:

1. official Go MCP SDK;
2. Streamable HTTP;
3. bearer middleware;
4. one echo-style tool;
5. Tailscale Serve;
6. actual Pebble app.

Prove:

```text
Index -> private Tailscale MCP -> tool result -> Index
```

and verify no problematic Origin header.

Then return a hardcoded `coreSchema Response` and confirm the text appears in the notification.

This de-risks the Pebble-specific part before Discord work.

## Phase 1 — Discord REST proof

Implement:

```text
send relay message
wait for thread M
read thread M
```

against the real Discord server.

Do not implement MCP and Discord simultaneously if debugging can be separated.

## Phase 2 — Hermes completion

Add:

```text
reaction state polling
Hermes author filtering
multi-message aggregation
50-second context
```

Verify manually by calling the function from a temporary/local harness or focused test.

Delete temporary harness code afterward.

## Phase 3 — MCP integration

Wire:

```text
ask_hermes -> Discord workflow -> coreSchema Response
```

Add failure result builders.

Run real Index round trip.

## Phase 4 — tests

Complete:

```bash
go test ./...
go vet ./...
```

Add all boundary tests described above.

## Phase 5 — Docker

Create:

```text
Dockerfile
.dockerignore
compose.example.yaml
example.env
```

Run:

```bash
docker build -t index-hermes-bridge .
```

Then:

```bash
cp example.env .env
cp compose.example.yaml compose.yaml
docker compose up -d --build
```

Verify with Tailscale Serve.

## Phase 6 — README and cleanup

Write the minimal README.

Delete:

- temporary debug endpoints
- unused abstractions
- experimental scripts
- commented-out code
- dead configuration
- test secrets

Run formatting:

```bash
gofmt -w *.go
```

Then:

```bash
go test ./...
go vet ./...
docker build .
```

---

# 78. New GitHub repository creation

The implementation agent is explicitly instructed to create a **new repository under the user's authenticated GitHub account**.

First verify authentication:

```bash
gh auth status
```

Initialize locally:

```bash
git init -b main
git add .
git commit -m "Initial implementation"
```

Before committing, verify:

```bash
git status
```

does **not** show:

```text
.env
compose.yaml
```

Create the repository.

Default private:

```bash
gh repo create index-hermes-bridge \
  --private \
  --source=. \
  --remote=origin \
  --description "A lightweight MCP bridge that sends Pebble Index requests to Hermes through Discord and returns Hermes responses to Index."
```

Push:

```bash
git push -u origin main
```

If the user explicitly chooses public visibility, use `--public` instead.

Do not fork another project.

---

# 79. Commit strategy

No need for dozens of microcommits, but preserve understandable milestones:

```text
Initial MCP server and Pebble response integration
Add Discord Hermes bridge
Add Docker deployment
Add tests and documentation
```

If one agent implements everything before the first push, one clean initial commit is acceptable.

Do not optimize git history at the expense of implementation work.

---

# 80. No CI initially

Do not create GitHub Actions in v1.

Local quality gates are enough:

```bash
gofmt -w *.go
go test ./...
go vet ./...
docker build .
```

If the repo becomes public or receives contributors, add a minimal CI workflow later.

This is an explicit YAGNI decision.

---

# 81. License

Include MIT license.

If repository remains private, this is harmless.

If it becomes public, licensing is already clear.

---

# 82. Acceptance criteria

The implementation is considered complete only when all of these are true.

## Repository

- [ ] New `index-hermes-bridge` repository exists under the user's GitHub account.
- [ ] It is not a fork.
- [ ] Description matches project purpose.
- [ ] `.env` is not committed.
- [ ] `compose.yaml` is not committed.
- [ ] `compose.example.yaml` is committed.
- [ ] `example.env` is committed.

## MCP

- [ ] `/mcp` uses Streamable HTTP.
- [ ] Bearer authentication is required.
- [ ] Origin policy is enforced.
- [ ] Exactly one tool exists.
- [ ] `tools/list` has no pagination cursor.
- [ ] `ask_hermes` accepts exactly one required string field.
- [ ] Pebble-compatible protocol initialization is tested.
- [ ] Successful result uses `coreSchema=1`.
- [ ] Successful result uses `SemanticResult.Response`.
- [ ] Expected failures use `GenericFailure`.

## Discord

- [ ] One relay message per Index invocation.
- [ ] Only Hermes can be pinged through `allowed_mentions`.
- [ ] Discord message nonce is used.
- [ ] `enforce_nonce` is enabled.
- [ ] Thread ID is derived from source message ID.
- [ ] Bridge does not use Discord Gateway/WebSockets.
- [ ] Hermes completion reaction is detected.
- [ ] Hermes author identity is verified.
- [ ] Hermes multi-message response is concatenated chronologically.
- [ ] Non-Hermes messages are ignored.

## Timing

- [ ] Thread creation deadline is 10 seconds.
- [ ] Complete server-side tool deadline is 50 seconds.
- [ ] Request context cancellation stops polling.
- [ ] A real ~30-second Hermes request works end-to-end.
- [ ] Bridge always returns before Pebble's 60-second MCP timeout under its own normal failure logic.

## Docker

- [ ] Multi-stage build.
- [ ] Static Go binary.
- [ ] Scratch runtime.
- [ ] CA certificates included.
- [ ] Container runs non-root.
- [ ] Port 8080 exposed.
- [ ] Compose binds host port to `127.0.0.1`.
- [ ] No volume required.
- [ ] No data persists.

## Real Index

- [ ] Index can reach MCP over Tailscale Serve.
- [ ] Index sends transcription to `ask_hermes`.
- [ ] Discord message appears in `#index`.
- [ ] Hermes creates a new thread.
- [ ] Hermes performs normal agent work.
- [ ] Hermes's Discord answer is returned through MCP.
- [ ] Pebble notification displays Hermes's answer.

## Audio

- [ ] README clearly states current stock remote MCP is text/transcription-only.
- [ ] No fake/dead audio parameter exists.

---

# 83. Definition of simplicity

A successful implementation should feel almost disappointingly small.

The desired mental model is:

```text
MCP request in
    ->
Discord message out
    ->
wait on deterministic Discord thread
    ->
Discord text back
    ->
MCP response out
```

If the implementation starts requiring:

```text
database
message bus
generic plugin system
background worker
Discord gateway connection
large framework
audio cache
routing engine
```

the design has drifted.

Re-evaluate before adding it.

---

# 84. Future extensions — only after demonstrated need

Potential extensions, in reasonable order:

1. **Configurable timeout** if Pebble raises its MCP timeout or different environments need it.
2. **Rich Discord response extraction** if Hermes routinely responds through embeds.
3. **Audio ingress via Index webhook** if raw recording access becomes genuinely useful.
4. **Gesture-based routing** if multiple Hermes destinations are desired.
5. **Multiple agents/channels** if the single-Hermes assumption changes.
6. **Tiny health endpoint** if deployment monitoring actually needs one.
7. **CI** if outside contributors appear.

Do not implement these proactively.

---

# 85. Architecture decision record summary

## ADR-001 — Start a new repository

**Decision:** create `index-hermes-bridge` from scratch.

**Reason:** existing Pebble/Hermes projects contain unrelated features, use different languages, and bypass the required Discord canonical workflow.

## ADR-002 — Go

**Decision:** Go.

**Reason:** small static binary, excellent HTTP support, low idle resource use, official MCP SDK, simple Docker image.

## ADR-003 — Official MCP Go SDK

**Decision:** use `github.com/modelcontextprotocol/go-sdk/mcp`.

**Reason:** protocol implementation should not be hand-written merely to avoid one justified dependency.

## ADR-004 — Streamable HTTP

**Decision:** Streamable HTTP.

**Reason:** modern MCP transport, supported by Pebble, replaces old HTTP+SSE, one endpoint, easiest reverse-proxy path.

## ADR-005 — Text-only v1

**Decision:** send transcription only.

**Reason:** Pebble's remote MCP adapter does not forward recording `SessionContext` and therefore does not expose raw ring audio to custom MCP tools.

## ADR-006 — Discord is canonical

**Decision:** all Hermes requests travel through Discord.

**Reason:** preserves normal Hermes threading, history, session behavior, and user-visible audit trail.

## ADR-007 — REST-only Discord client

**Decision:** no Gateway/WebSocket.

**Reason:** all required operations are available through REST; Gateway adds persistent state and complexity.

## ADR-008 — Source message ID is correlation ID

**Decision:** use Discord's source-message/thread shared ID.

**Reason:** deterministic, free, no persistence required.

## ADR-009 — Reactions signal completion

**Decision:** wait for Hermes success/failure reaction before collecting thread text.

**Reason:** handles multi-message responses without guessing that the first Hermes message is final.

## ADR-010 — Fail closed on thread anomalies

**Decision:** require canonical thread ID `M`.

**Reason:** avoids incorrectly returning inline/fallback/duplicate-thread content during Hermes Discord failures.

## ADR-011 — Pebble `SemanticResult.Response`

**Decision:** emit Pebble's `coreSchema` v1 Response result.

**Reason:** current Pebble source explicitly surfaces this result in completion notifications and avoids known generic-result notification behavior.

## ADR-012 — 50-second deadline

**Decision:** hardcode 50 seconds.

**Reason:** Pebble's current MCP SDK defaults to a 60-second request timeout. A safety margin is required.

## ADR-013 — Tailscale Serve

**Decision:** use tailnet-private Tailscale Serve and bind Docker host port to loopback.

**Reason:** one stable HTTPS address, no public exposure, no changing local/Tailscale host configuration.

---

# 86. Sources

[^1]: **Core Devices / Pebble mobile app — `HttpMcpIntegration.kt`.** Current remote MCP implementation: Streamable HTTP/SSE selection, Authorization header, tool pagination behavior, session-context omission, `coreSchema` parsing, text-only fallback handling, and server instructions.  
   https://raw.githubusercontent.com/coredevices/mobileapp/master/mcp/src/commonMain/kotlin/coredevices/mcp/client/HttpMcpIntegration.kt

[^2]: **Model Context Protocol — Transports specification (2025-11-25).** Streamable HTTP replaces HTTP+SSE; security guidance for Origin validation, localhost binding, and authentication.  
   https://modelcontextprotocol.io/specification/2025-11-25/basic/transports

[^3]: **Core Devices / Pebble mobile app — `libs.versions.toml`.** Current MCP Kotlin SDK version pin (`0.15.0`).  
   https://raw.githubusercontent.com/coredevices/mobileapp/master/gradle/libs.versions.toml

[^4]: **Model Context Protocol Go SDK.** Official Go SDK and protocol-version compatibility matrix.  
   https://github.com/modelcontextprotocol/go-sdk

[^5]: **MCP Kotlin SDK 0.15.0 — `Protocol.kt`.** Defines `DEFAULT_REQUEST_TIMEOUT` as 60 seconds and applies it to outgoing requests.  
   https://raw.githubusercontent.com/modelcontextprotocol/kotlin-sdk/0.15.0/kotlin-sdk-core/src/commonMain/kotlin/io/modelcontextprotocol/kotlin/sdk/shared/Protocol.kt

[^6]: **MCP Go SDK server documentation.** Tool-result content support includes text, images, audio, embedded resources, and other MCP content types.  
   https://github.com/modelcontextprotocol/go-sdk/blob/main/docs/server.md

[^7]: **Discord — Channels Resource / Threads documentation.** A public thread created from a message has the same ID as its source message.  
   https://docs.discord.com/developers/resources/channel  
   https://docs.discord.com/developers/topics/threads

[^8]: **Nous Research Hermes Agent — Discord documentation.** Bot-message behavior, auto-threading, reactions, session behavior, and channel configuration.  
   https://github.com/NousResearch/hermes-agent/blob/main/website/docs/user-guide/messaging/discord.md

[^9]: **Discord — Message Resource.** Create/read message APIs, message-content behavior, message length, allowed mentions, nonce/idempotency fields, and history access.  
   https://docs.discord.com/developers/resources/message

[^10]: **Core Devices / Pebble mobile app — `ToolCallResult.kt`.** Defines `SemanticResult.Response`, `GenericFailure`, and their serialized names/fields.  
    https://raw.githubusercontent.com/coredevices/mobileapp/master/mcp/src/commonMain/kotlin/coredevices/mcp/data/ToolCallResult.kt

[^11]: **Core Devices / Pebble mobile app — `IndexNotificationManager.kt`.** Completion-notification rendering explicitly uses `SemanticResult.Response.text` and `GenericFailure.userErrorMessage`.  
    https://raw.githubusercontent.com/coredevices/mobileapp/master/experimental/src/commonMain/kotlin/coredevices/ring/service/IndexNotificationManager.kt

[^12]: **Core Devices mobile app issue #309.** Current issue describing MCP read-only results showing the original question instead of the returned answer in notifications.  
    https://github.com/coredevices/mobileapp/issues/309

[^13]: **Core Devices mobile app issue #346.** Current issue concerning successful third-party MCP calls appearing as “No action taken,” relevant to semantic result handling.  
    https://github.com/coredevices/mobileapp/issues/346

[^14]: **Tailscale Serve examples.** Tailnet-only HTTPS proxying of local ports and persistent `--bg` mode.  
    https://tailscale.com/docs/reference/examples/serve

[^15]: **Discord — API Reference.** HTTP clients must send a valid User-Agent and valid request content types.  
    https://docs.discord.com/developers/reference

[^16]: **Discord — Rate-limit guidance.** Respect `retry_after` and do not retry non-recoverable validation/permission failures.  
    https://docs.discord.com/developers/discord-social-sdk/how-to/handle-rate-limits

[^17]: **Hermes Agent issue reports concerning Discord auto-thread fallback/ambiguity.** Used to justify fail-closed canonical thread correlation rather than fallback scanning.  
    https://github.com/NousResearch/hermes-agent/issues/20243  
    https://github.com/NousResearch/hermes-agent/issues/73032

[^18]: **Go release history.** Go 1.27.1 released September 1, 2026.  
    https://go.dev/doc/devel/release

[^19]: **MCP Go SDK protocol documentation.** Distinguishes the legacy initialized lifecycle used through protocol 2025-11-25 from the newer 2026 stateless lifecycle.  
    https://github.com/modelcontextprotocol/go-sdk/blob/main/docs/protocol.md

[^20]: **MCP Go SDK `streamable.go`.** Streamable HTTP options, request-body limits, stateful/stateless behavior, session handling, and compatibility logic.  
    https://github.com/modelcontextprotocol/go-sdk/blob/main/mcp/streamable.go

[^21]: **MCP Go SDK issue #1155.** Documents current behavior around long-running Streamable HTTP POST calls remaining quiet until tool completion; included as a compatibility risk to test rather than preemptively engineer around.  
    https://github.com/modelcontextprotocol/go-sdk/issues/1155

[^22]: **Pebble Help Center — Index Advanced Features (MCP, Webhook).** Product-level configuration and remote MCP capabilities.  
    https://help.repebble.com/en/articles/15724406-index-advanced-features-mcp-webhook

---

# 87. Final implementation directive

Build exactly the smallest service that fulfills this sequence:

```text
Index transcription
    ->
authenticated Streamable HTTP MCP
    ->
ask_hermes(message)
    ->
Discord REST create-message in #index
    ->
@Hermes
    ->
canonical Hermes thread
    ->
wait for Hermes final reaction
    ->
read Hermes text
    ->
Pebble coreSchema Response
    ->
Index notification
```

Prefer deletion over abstraction.

Prefer fixed constants over configuration where deployment does not need variation.

Prefer one direct REST request over a library when the API surface is tiny.

Prefer the official MCP SDK over manually implementing MCP.

Prefer deterministic Discord IDs over application state.

Prefer an explicit failure over guessing which thread or message belongs to a request.

Version 1 is text-only because that is what the current Pebble remote MCP implementation can actually provide.

The finished project should be small, boring, deterministic, and easy to operate.
