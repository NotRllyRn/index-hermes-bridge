# index-hermes-bridge

A lightweight MCP bridge that sends Pebble Index requests to Hermes through Discord and optionally delivers responses through ntfy.

## How it works

`Index -> MCP acknowledgement -> #index -> @Hermes -> Hermes thread -> ntfy notification`

If Hermes cannot create the canonical thread, the relay bot creates it from the source message and re-triggers Hermes inside.

Discord remains the canonical Hermes conversation history.

## Requirements

- Pebble Index 01 and mobile app with custom MCP support
- Hermes Agent with Discord enabled
- A Discord relay bot
- Docker
- Tailscale (recommended)

Configure Hermes with:

```env
DISCORD_REQUIRE_MENTION=true
DISCORD_ALLOW_BOTS=mentions
DISCORD_AUTO_THREAD=true
DISCORD_REACTIONS=true
```

The target channel must not be a Hermes free-response or no-thread channel.

## Discord bot

Create a Discord application/bot, enable Message Content Intent, and grant it only these permissions in the target `#index` channel:

- View Channel
- Send Messages
- Read Message History
- Create Public Threads
- Send Messages in Threads

The relay bot uses thread-creation permission only when Hermes's own thread creation fails or is rate-limited. It does not need Manage Threads or Administrator.

Record its token, the channel ID, and the Hermes bot user ID.

## Setup

```bash
git clone https://github.com/NotRllyRn/index-hermes-bridge.git
cd index-hermes-bridge
cp example.env .env
cp compose.example.yaml compose.yaml
openssl rand -hex 32
```

Put the generated secret and Discord values in `.env`, then start the bridge:

```bash
docker compose up -d --build
tailscale serve --bg 8080
```

To receive completed Hermes replies through ntfy, set both optional values:

```env
NTFY_SERVER_URL="https://ntfy.example.com"
NTFY_TOPIC="index-hermes"
```

Leave both empty to disable ntfy. The bridge waits in the background for Hermes's final reaction and posts the completed Discord response as plain text to that topic.

## Pebble Index

Create a custom MCP server:

- URL: `https://<tailscale-host>/mcp`
- Transport: **Streamable HTTP**
- Authorization: `Bearer <MCP_BEARER_TOKEN>`

Assign it to the desired sandbox or gesture. The server exposes one tool: `ask_hermes(message)`.

The tool returns **“Hermes will reply soon”** as soon as Hermes acknowledges the Discord message. It does not hold the MCP request open while Hermes works. If ntfy is configured, the final reply arrives there later.

## Audio

The stock Pebble remote MCP integration does not forward the raw Index recording to custom MCP servers. Version 1 sends only the transcription/text.

## License

MIT
