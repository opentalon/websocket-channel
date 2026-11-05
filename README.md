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
      redis_url: "redis://redis:6379/0"  # optional: cross-pod fan-out (omit for single-pod)
```

### Multi-pod deployments

By default (no `redis_url`) the channel delivers replies only to sockets on its
own pod — fine for a single-pod deployment. When the orchestrator runs on
several pods, a reply may be generated on a different pod than the one holding
the user's browser socket. Set `redis_url` (pointing at the same Redis the
orchestrator uses) to fan replies out across pods over Redis pub/sub: every pod
publishes its outbound frames and every pod delivers the ones addressed to a
socket it holds. Delivery is owner-gated — a pod only delivers a frame whose
resolved owner matches the local socket's owner. An envelope without an owner
(e.g. published by a core version that predates owner stamping) is delivered
without the gate, so mixed-version rollouts degrade gracefully rather than
dropping messages.

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
transcript is reserved for the server-to-server endpoints below.

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

**Server-to-server open** (`POST {path}/open`)

A trusted backend opens a chat session for a user, seeded with hidden content
the assistant answers, so the assistant can start the conversation. Only a
system token (WhoAmI `kind: "system"`) may call it.

```json
{
  "token": "<system profile_token>",
  "opened_by": "<origin label>",
  "open_key": "<idempotency key of the event>",
  "content": "<hidden context for the first turn>",
  "visibility": "hidden",
  "reasoning_effort": "low"
}
```

To continue such a session, send `session_id` (from an earlier answer) and
`note_key` instead of `opened_by` and `open_key`: the content becomes a hidden
note in that session, stored and answered once per note key. `visibility` and
`reasoning_effort` are optional and passed to the core as on `/inject`; the
content is hidden either way.

Returns `200 {"session_id": "<user>:websocket:<conversation>", "created": true}`
only after the core committed the session. `created` is `false` when the same
user, origin and key found the existing session (its first turn never runs
twice) or the note key was already stored.

| Status | Meaning |
|---|---|
| `400` | bad body or missing field |
| `401` | unresolvable token |
| `403` | not a system token |
| `404` | the session to continue does not exist or belongs to another user |
| `405` | non-POST |
| `502` | the core failed; retry with the same key |
| `503` | the core did not take the request; retry with the same key |
| `504` | no answer within 10 seconds; retry with the same key |

The assistant's reply goes to the session's conversation like any reply: to
the sockets that hold it, never to another conversation's or another user's. A
client that attaches later reads it from the session's history.

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
