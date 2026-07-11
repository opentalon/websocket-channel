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

Clients cannot set message visibility — hiding a turn from the audited
transcript is reserved for the server-to-server inject endpoint below.

**Server-to-server inject** (`POST {path}/inject`)

A trusted backend posts a message into an existing conversation without holding
a socket — e.g. an async job reporting completion back into the chat that
started it. The token is resolved to its owning user (same whoami as the socket
upgrade), so a message can only ever reach that user's own conversation.

```json
{
  "token": "<profile_token>",
  "conversation_id": "3f2a1b...",
  "content": "[system] Your job finished.",
  "visibility": "hidden",
  "resume_intent": "true"
}
```

Returns `202` on accept (`400` bad body / missing field, `401` unresolvable
token, `403` the conversation is owned by a different user, `405` non-POST).
The token is resolved to its owner and, if the conversation already has live
sockets, they must belong to that same owner — an inject can never deliver into
another user's conversation. There is no reply on this request: the core's reply
fans out to the user's live browser sockets for the conversation. `visibility:
"hidden"` marks the injected turn as model-only — fed to the model but dropped
from the user-facing transcript (honored only for a WhoAmI-verified system
profile).

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
