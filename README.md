# websocket-channel

[![CI](https://github.com/opentalon/websocket-channel/actions/workflows/ci.yml/badge.svg)](https://github.com/opentalon/websocket-channel/actions/workflows/ci.yml)
[![Go Report Card](https://goreportcard.com/badge/github.com/opentalon/websocket-channel)](https://goreportcard.com/report/github.com/opentalon/websocket-channel)

Standalone WebSocket server channel for [OpenTalon](https://github.com/opentalon/opentalon). Browser clients connect with a profile token and chat via JSON frames. Supports text messages and file attachments (CSV, PDF, images).

## How it works

1. OpenTalon starts the binary as a subprocess.
2. Browser/client connects: `ws://host:9000/ws?token=<profile_token>`
3. The channel injects the token into every `InboundMessage` as `metadata["profile_token"]`.
4. OpenTalon verifies it via the [Profiles & WhoAmI](https://github.com/opentalon/opentalon/blob/master/docs/profiles.md) system and scopes the session to that identity.
5. Responses are returned as HTML — ready to render in the browser.

## Build

```bash
git clone https://github.com/opentalon/websocket-channel
cd websocket-channel
make build        # → ./websocket-channel binary
make test         # go test -race ./...
```

## OpenTalon config

```yaml
channels:
  - name: websocket
    plugin: ./websocket-channel
    config:
      addr: "0.0.0.0:9000"   # host:port to listen on
      path: "/ws"             # WebSocket endpoint path
      cors_origins:           # allowed origins (omit to allow all — dev only)
        - "https://mysite.com"
```

## Wire protocol

**Connect**

```
ws://host:9000/ws?token=<profile_token>
```

Or pass the token as an HTTP header on the upgrade request:

```
Authorization: Bearer <profile_token>
```

Connections without a token are rejected with HTTP 401.

**Client → server** (JSON text frame)

```json
{
  "content": "Summarise this document.",
  "files": [
    {
      "name": "report.pdf",
      "mime_type": "application/pdf",
      "data": "<base64>"
    }
  ]
}
```

**Server → client** (JSON text frame)

```json
{
  "conversation_id": "3f2a1b...",
  "content": "<p>The document says...</p>"
}
```

`content` is HTML. Each WebSocket connection gets a unique `conversation_id`.

## Demo

```bash
# Terminal 1 — run OpenTalon with the channel
./opentalon --config config.yaml

# Terminal 2 — serve the demo UI
python3 demo/serve.py
# → http://localhost:8080
```

The demo is a single HTML file with no dependencies — token input, chat log, and file upload.

## Standalone flags

```
-addr   string   Listen address (default "0.0.0.0:9000")
-path   string   WebSocket path (default "/ws")
-origins string  Comma-separated CORS origins (default: allow all)
```
