package wschannel

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	pkg "github.com/opentalon/opentalon/pkg/channel"
)

// ID is the channel identifier for the WebSocket channel.
const ID = "websocket"

// controlMetadataKey / controlResumeHello mirror the core's
// pkg/channel.ControlMetadataKey / ControlResumeHello string contract. They are
// kept as literals (not the pkg constants) so this plugin keeps building
// against its currently-pinned core module version — the wire contract is the
// string value, which the core reads via its own constant. Switch to the pkg
// constants once the core dependency is bumped to a release carrying them.
const (
	controlMetadataKey = "control"
	controlResumeHello = "resume_hello"
)

// writeTimeout bounds a single fan-out write so one stuck socket (e.g. a
// half-open TCP connection after a laptop sleep) cannot delay delivery to a
// user's other connections beyond this bound.
const writeTimeout = 5 * time.Second

// Config holds the WebSocket server configuration.
type Config struct {
	Addr        string   // listening address, e.g. "0.0.0.0:9000"
	Path        string   // WebSocket path, e.g. "/ws"
	CORSOrigins []string // allowed origins; empty = allow all (dev mode)
	// WhoamiURL is the Timly /whoami endpoint the channel calls at upgrade to
	// resolve a profile token to its owning user. Required: without it the
	// channel cannot group a user's connections and refuses to start.
	WhoamiURL string
	// WhoamiSecret is the shared secret sent as X-Security-Token on /whoami
	// calls. When empty it falls back to the WHOAMI_SECRET environment variable
	// (inherited from the core process), so the secret need not be duplicated in
	// the plugin config alongside the core's own profiles.who_am_i block.
	WhoamiSecret string
}

type wsConn struct {
	ws *websocket.Conn
	mu sync.Mutex
}

// connSet holds every live socket for one conversation. All of them are owned
// by the same user: ownerEntityID is pinned at first connect and a token that
// resolves to a different user is refused at upgrade. That single-owner
// invariant is what lets Send fan a response out to the whole set without ever
// crossing a user boundary — even though the core addresses responses by the
// raw (un-scoped) conversation id.
type connSet struct {
	ownerEntityID string
	conns         map[*wsConn]struct{}
}

// Channel is a WebSocket server channel. Browser clients connect to it with a
// profile token and exchange JSON text frames with the OpenTalon core.
type Channel struct {
	cfg      Config
	resolver *whoamiResolver

	connsMu sync.Mutex
	conns   map[string]*connSet // conversationID → that user's live sockets (1:N)

	inbox   chan<- pkg.InboundMessage
	srv     *http.Server
	stopMu  sync.Mutex
	stopped bool
	wg      sync.WaitGroup
}

// inboundFrame is the JSON structure for client → server messages. A client
// cannot set message visibility: hiding a turn from the audited transcript is a
// privileged capability reserved for the server-to-server /inject path, so no
// `visibility` field is accepted here.
type inboundFrame struct {
	Content  string         `json:"content"`
	Files    []fileFrame    `json:"files,omitempty"`
	Metadata map[string]any `json:"metadata,omitempty"` // client hints (e.g. prompt_type); used locally — only the confirmation decision is forwarded to core (see metadata["confirmation"])
}

type fileFrame struct {
	Name     string `json:"name"`
	MimeType string `json:"mime_type"`
	Data     string `json:"data"` // base64-encoded
}

// outboundFrame is the JSON structure for server → client messages.
type outboundFrame struct {
	ConversationID string            `json:"conversation_id"`
	Content        string            `json:"content"`
	Metadata       map[string]string `json:"metadata,omitempty"`  // pass-through from core (e.g. type=confirmation, options=approve,reject)
	Streaming      bool              `json:"streaming,omitempty"` // true while LLM is still generating; false (or absent) = final message
	Done           bool              `json:"done,omitempty"`      // true on the last streaming frame
	Typing         bool              `json:"typing,omitempty"`    // true for keepalive typing-indicator frames (no content)
}

// New returns a Channel with the given default config.
// Config values are overridden by Configure() when run under OpenTalon.
func New(cfg Config) *Channel {
	if cfg.Addr == "" {
		cfg.Addr = "0.0.0.0:9000"
	}
	if cfg.Path == "" {
		cfg.Path = "/ws"
	}
	return &Channel{cfg: cfg, conns: make(map[string]*connSet)}
}

// Configure implements pkg.ConfigurableChannel. Called by OpenTalon with the
// config map from the channel YAML before Start is called.
func (c *Channel) Configure(config map[string]interface{}) error {
	if v, ok := config["addr"].(string); ok && v != "" {
		c.cfg.Addr = v
	}
	if v, ok := config["path"].(string); ok && v != "" {
		c.cfg.Path = v
	}
	if origins, ok := config["cors_origins"].([]interface{}); ok {
		c.cfg.CORSOrigins = nil
		for _, o := range origins {
			if s, ok := o.(string); ok && s != "" {
				c.cfg.CORSOrigins = append(c.cfg.CORSOrigins, s)
			}
		}
	}
	if v, ok := config["whoami_url"].(string); ok && v != "" {
		c.cfg.WhoamiURL = v
	}
	if v, ok := config["whoami_secret"].(string); ok && v != "" {
		c.cfg.WhoamiSecret = v
	}
	return nil
}

// ID implements pkg.Channel.
func (c *Channel) ID() string { return ID }

// Kind implements pkg.Channel. The websocket channel runs as a single
// instance, so its channel type and its per-instance ID are the same value.
func (c *Channel) Kind() string { return ID }

// Capabilities implements pkg.Channel.
func (c *Channel) Capabilities() pkg.Capabilities {
	return pkg.Capabilities{
		ID:               ID,
		Name:             "WebSocket",
		Files:            true,
		Threads:          false,
		Reactions:        false,
		Edits:            true,
		MaxMessageLength: 64 * 1024,
		// Markdown instead of HTML: the HTML format hint in the system prompt
		// prevents weak LLMs (e.g. gpt-oss-120b) from producing [tool_call]
		// blocks. The frontend converts markdown to HTML with a JS library.
		ResponseFormat: pkg.FormatMarkdown,
	}
}

// Start implements pkg.Channel. It starts the HTTP/WebSocket server and returns
// immediately; the server runs in a background goroutine.
func (c *Channel) Start(ctx context.Context, inbox chan<- pkg.InboundMessage) error {
	resolver, err := newWhoamiResolver(c.cfg.WhoamiURL, c.cfg.WhoamiSecret)
	if err != nil {
		return err
	}
	c.resolver = resolver
	c.inbox = inbox

	mux := http.NewServeMux()
	mux.HandleFunc(c.cfg.Path, c.handleUpgrade)
	// Server-side inject: a trusted backend (not a browser) posts a message
	// into an existing conversation — e.g. a hidden system status note from a
	// finished background job. Same delivery path as a live socket, minus the
	// socket: the message is handed to the core, whose reply fans out to the
	// user's live browser sockets for that conversation.
	mux.HandleFunc(c.cfg.Path+"/inject", c.handleInject)

	c.srv = &http.Server{
		Addr:    c.cfg.Addr,
		Handler: mux,
	}

	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		if err := c.srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("websocket channel: server error", "error", err)
		}
	}()

	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = c.srv.Shutdown(shutCtx)
	}()

	slog.Info("websocket channel: listening", "addr", c.cfg.Addr, "path", c.cfg.Path)
	return nil
}

// Send implements pkg.Channel. It delivers a response to the WebSocket client
// identified by msg.ConversationID. Safe for concurrent use.
func (c *Channel) Send(ctx context.Context, msg pkg.OutboundMessage) error {
	typing := msg.Metadata["_typing"] == "true"
	targets := c.targets(msg.ConversationID)
	slog.Debug("websocket Send", "conv", msg.ConversationID, "content_len", len(msg.Content), "typing", typing, "sockets", len(targets))
	if len(targets) == 0 {
		return nil // no live connection for this conversation
	}
	// Filter out internal metadata keys before forwarding to the client.
	var meta map[string]string
	for k, v := range msg.Metadata {
		if k == "_typing" || k == "profile_token" {
			continue
		}
		if meta == nil {
			meta = make(map[string]string, len(msg.Metadata))
		}
		meta[k] = v
	}
	frame := outboundFrame{
		ConversationID: msg.ConversationID,
		Content:        msg.Content,
		Metadata:       meta,
		Typing:         typing,
	}
	data, err := json.Marshal(frame)
	if err != nil {
		return fmt.Errorf("marshal response: %w", err)
	}
	// Fan out to every live socket the user has open on this conversation.
	// Writes are sequential because each socket requires serialized writes, but
	// each is bounded by writeTimeout so one stuck socket delays the rest by at
	// most that bound rather than blocking the fan-out indefinitely; writes to
	// distinct sockets never contend (separate wsConn.mu). A failed or timed-out
	// write is logged but never aborts delivery to the others — that socket's
	// readLoop observes the same break and unregisters it via its defer, so
	// removal stays single-sourced (no double bookkeeping here).
	for _, cn := range targets {
		wctx, cancel := context.WithTimeout(ctx, writeTimeout)
		cn.mu.Lock()
		werr := cn.ws.Write(wctx, websocket.MessageText, data)
		cn.mu.Unlock()
		cancel()
		if werr != nil {
			slog.Debug("websocket Send: write failed", "conv", msg.ConversationID, "error", werr)
		}
	}
	return nil
}

// SendAndCapture implements pkg.UpdatableChannel. Returns an error so
// the StreamWriter never sets flushed=true. This makes registry.go
// fall through to ch.Send() with the clean final response — the user
// only sees one frame with the correct answer, no intermediate flicker.
func (c *Channel) SendAndCapture(_ context.Context, msg pkg.OutboundMessage) (string, error) {
	slog.Debug("websocket SendAndCapture suppressed", "conv", msg.ConversationID, "content_len", len(msg.Content))
	return "", fmt.Errorf("websocket: streaming suppressed")
}

// SendUpdate implements pkg.UpdatableChannel. No-op because SendAndCapture
// returns an error, so the StreamWriter never has a messageID to update.
func (c *Channel) SendUpdate(_ context.Context, msgID string, msg pkg.OutboundMessage) error {
	slog.Debug("websocket SendUpdate suppressed", "msgID", msgID, "content_len", len(msg.Content))
	return nil
}

// Stop implements pkg.Channel.
func (c *Channel) Stop() error {
	if c.srv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = c.srv.Shutdown(ctx)
	}
	c.stopMu.Lock()
	c.stopped = true
	c.stopMu.Unlock()
	c.wg.Wait()
	return nil
}

// handleInject accepts a server-to-server message injection. A trusted backend
// POSTs {token, conversation_id, content, visibility, resume_intent}; the token
// is resolved to its owning user (same whoami as the socket upgrade), and the
// message is pushed to the core as an InboundMessage. There is no socket and no
// reply on this request — the core's reply is delivered to the user's live
// browser sockets for the conversation. Because the core re-scopes the
// conversation id to the token owner, a message can only ever reach that user's
// own conversation.
func (c *Channel) handleInject(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var body struct {
		Token          string `json:"token"`
		ConversationID string `json:"conversation_id"`
		Content        string `json:"content"`
		Visibility     string `json:"visibility"`
		ResumeIntent   string `json:"resume_intent"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64*1024)).Decode(&body); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	if body.Token == "" || body.ConversationID == "" || body.Content == "" {
		http.Error(w, "token, conversation_id and content are required", http.StatusBadRequest)
		return
	}

	// Fail closed: an unresolvable token never reaches the core.
	if _, err := c.resolver.resolve(r.Context(), body.Token); err != nil {
		slog.Warn("websocket channel: inject identity resolution failed", "error", err)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	meta := map[string]string{"profile_token": body.Token}
	if body.ResumeIntent != "" {
		meta[pkg.ResumeIntentMetadataKey] = body.ResumeIntent
	}
	if body.Visibility != "" {
		meta["visibility"] = body.Visibility
	}
	msg := pkg.InboundMessage{
		ChannelID:      ID,
		ConversationID: body.ConversationID,
		SenderID:       body.ConversationID,
		Content:        body.Content,
		Metadata:       meta,
		Timestamp:      time.Now(),
	}

	select {
	case c.inbox <- msg:
		w.WriteHeader(http.StatusAccepted)
	case <-r.Context().Done():
		http.Error(w, "request cancelled", http.StatusRequestTimeout)
	case <-time.After(writeTimeout):
		http.Error(w, "channel busy", http.StatusServiceUnavailable)
	}
}

// handleUpgrade upgrades an HTTP request to a WebSocket connection.
// The client can pass ?conversation_id=<id> to resume a previous session
// (reconnect). Without it, a new conversation ID is generated.
func (c *Channel) handleUpgrade(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("token")
	if token == "" {
		if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
			token = strings.TrimPrefix(auth, "Bearer ")
		}
	}
	if token == "" {
		http.Error(w, "token required", http.StatusUnauthorized)
		return
	}

	// Resolve the token to its owning user BEFORE accepting the socket: the
	// connection has to be grouped by user (so a user's tabs share a fan-out
	// set), and a token that cannot be resolved must never get a live socket.
	// Fail closed — an unresolvable token is a plain 401, no upgrade.
	entityID, err := c.resolver.resolve(r.Context(), token)
	if err != nil {
		slog.Warn("websocket channel: identity resolution failed", "error", err)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	opts := &websocket.AcceptOptions{}
	if len(c.cfg.CORSOrigins) > 0 {
		opts.OriginPatterns = c.cfg.CORSOrigins
	} else {
		opts.InsecureSkipVerify = true // dev: allow all origins
	}

	ws, err := websocket.Accept(w, r, opts)
	if err != nil {
		slog.Warn("websocket channel: accept failed", "error", err)
		return
	}

	// Allow reconnection: if the client provides a conversation_id, reuse it
	// so the session and its history are preserved across disconnects.
	// A client-supplied id signals resume-intent to the core handler (see
	// readLoop); a server-minted one signals fresh-create.
	clientConvID := r.URL.Query().Get("conversation_id")
	convID := clientConvID
	if convID == "" {
		convID = newID()
	}
	resumeIntent := clientConvID != ""
	cn := &wsConn{ws: ws}

	// Register the socket in its conversation's set. A conversation is owned by
	// exactly one user; multiple connections of THAT user (several tabs/windows)
	// are all welcome and each assistant frame fans out to all of them. A token
	// resolving to a DIFFERENT user is refused — the core's entity-scoped session
	// key is the real boundary, this stops cross-user fan-out at the transport
	// layer. The StatusPolicyViolation here is a deliberate terminal refusal of a
	// genuine cross-user collision (a normal user never hits it); the client must
	// not reconnect-loop on it.
	if err := c.addConn(convID, entityID, cn); err != nil {
		slog.Warn("websocket channel: connection refused — conversation owned by another user", "conversation_id", convID)
		_ = ws.Close(websocket.StatusPolicyViolation, "conversation owned by another user")
		return
	}

	c.stopMu.Lock()
	if c.stopped {
		c.stopMu.Unlock()
		c.removeConn(convID, cn)
		_ = ws.CloseNow()
		return
	}
	c.wg.Add(1)
	c.stopMu.Unlock()
	defer func() {
		c.wg.Done()
		c.removeConn(convID, cn)
		_ = ws.CloseNow()
	}()

	// Send a welcome frame so the client knows its conversation_id for
	// reconnection. If the write fails the client never learns its id and
	// any subsequent reconnect attempt with that id would be untrackable —
	// bail out via the defer rather than enter readLoop on a half-open
	// socket.
	welcome := outboundFrame{
		ConversationID: convID,
		Metadata:       map[string]string{"type": "connected"},
	}
	data, err := json.Marshal(welcome)
	if err != nil {
		slog.Warn("websocket channel: welcome marshal failed", "conversation_id", convID, "error", err)
		return
	}
	cn.mu.Lock()
	werr := ws.Write(r.Context(), websocket.MessageText, data)
	cn.mu.Unlock()
	if werr != nil {
		slog.Warn("websocket channel: welcome write failed", "conversation_id", convID, "error", werr)
		return
	}

	slog.Info("websocket channel: client connected", "conversation_id", convID, "reconnect", resumeIntent)

	// Resume handshake: on a reconnect (client-supplied conversation_id) send
	// one control message to core BEFORE the user types. If a tool confirmation
	// is still awaiting the user's decision, core re-emits its prompt frame so
	// this freshly-reconnected tab redraws the Approve/Reject buttons instead of
	// showing a dead transcript. The socket is already in the fan-out set (added
	// above), so the re-emit reaches it. A fresh connection (server-minted id)
	// has no prior pending state, so the hello is resume-only.
	if resumeIntent {
		hello := pkg.InboundMessage{
			ChannelID:      ID,
			ConversationID: convID,
			SenderID:       convID,
			Metadata: map[string]string{
				"profile_token":             token,
				pkg.ResumeIntentMetadataKey: "true",
				controlMetadataKey:          controlResumeHello,
			},
			Timestamp: time.Now(),
		}
		select {
		case <-r.Context().Done():
			return
		case c.inbox <- hello:
		}
	}

	c.readLoop(r.Context(), cn, convID, token, resumeIntent)
}

func (c *Channel) readLoop(ctx context.Context, cn *wsConn, convID, token string, resumeIntent bool) {
	for {
		_, data, err := cn.ws.Read(ctx)
		if err != nil {
			return
		}

		var frame inboundFrame
		if err := json.Unmarshal(data, &frame); err != nil {
			slog.Warn("websocket channel: bad frame", "error", err)
			continue
		}
		if frame.Content == "" && len(frame.Files) == 0 {
			continue
		}

		// Echo the user's text to their OTHER tabs so it shows up there live,
		// not just the assistant's reply. Confirmation replies are echoed too:
		// the widget now sends a readable localized label ("Approve"/"Reject")
		// as the content, not a bare "y"/"n", so it reads as a normal user
		// bubble — and seeing the reply is what tells a sibling tab its own
		// still-open confirmation was answered elsewhere, so it retires those
		// buttons instead of leaving a stale, clickable prompt. The sending
		// socket already rendered the message locally and is skipped inside
		// broadcastUserInput.
		if frame.Content != "" {
			c.broadcastUserInput(convID, cn, frame.Content)
		}

		// Visibility is NOT read from the client frame: hiding a turn from the
		// audited transcript is a privileged capability reserved for the trusted
		// server-to-server /inject path (handleInject sets it there). Honoring a
		// browser-supplied visibility would let a user feed model-directed
		// content while keeping it out of the transcript and their sibling tabs.
		meta := map[string]string{"profile_token": token}
		if resumeIntent {
			// Signal to the core handler: this conversation_id came from the
			// client, not from server-side mint. Triggers strict Load and
			// session_expired error frame on miss, instead of silent auto-
			// create against a UI that still shows the prior history.
			meta[pkg.ResumeIntentMetadataKey] = "true"
		}
		// Forward the deterministic confirmation decision from a confirmation
		// button click (prompt_type=confirmation_response, action=approve|reject)
		// as metadata["confirmation"]. Core takes its deterministic structured-
		// signal path ONLY for the canonical values approve/reject; any other
		// value (or none) safely falls through to LLM classification of the reply
		// text. The frontend buttons only ever send approve/reject, so a button
		// press never depends on the y/n content being interpreted. Only this one
		// key is forwarded; all other client hints stay local.
		if isControlReply(frame.Metadata) {
			if action, _ := frame.Metadata["action"].(string); action != "" {
				meta["confirmation"] = action
			}
		}
		msg := pkg.InboundMessage{
			ChannelID:      ID,
			ConversationID: convID,
			SenderID:       convID,
			Content:        frame.Content,
			Metadata:       meta,
			Timestamp:      time.Now(),
		}

		for _, f := range frame.Files {
			decoded, err := base64.StdEncoding.DecodeString(f.Data)
			if err != nil {
				slog.Warn("websocket channel: base64 decode failed", "file", f.Name, "error", err)
				continue
			}
			msg.Files = append(msg.Files, pkg.FileAttachment{
				Name:     f.Name,
				MimeType: f.MimeType,
				Data:     decoded,
				Size:     int64(len(decoded)),
			})
		}

		select {
		case <-ctx.Done():
			return
		case c.inbox <- msg:
		}
	}
}

func newID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%x", b)
}

// ── conversation registry (1:N) ─────────────────────────────────────────────

// errOwnerMismatch is returned by addConn when a token resolves to a different
// user than the one that already owns the conversation; the upgrade is refused.
var errOwnerMismatch = errors.New("conversation owned by another user")

// addConn registers cn under convID, creating the conversation's set and pinning
// its owner on first use. A token resolving to a different user than the existing
// owner is refused with errOwnerMismatch.
func (c *Channel) addConn(convID, entityID string, cn *wsConn) error {
	c.connsMu.Lock()
	defer c.connsMu.Unlock()
	set, ok := c.conns[convID]
	if !ok {
		set = &connSet{ownerEntityID: entityID, conns: make(map[*wsConn]struct{})}
		c.conns[convID] = set
	} else if set.ownerEntityID != entityID {
		return errOwnerMismatch
	}
	set.conns[cn] = struct{}{}
	return nil
}

// removeConn unregisters cn from convID's set, deleting the set when its last
// socket leaves so a conversation key never lingers after its owner disconnects.
func (c *Channel) removeConn(convID string, cn *wsConn) {
	c.connsMu.Lock()
	defer c.connsMu.Unlock()
	set, ok := c.conns[convID]
	if !ok {
		return
	}
	delete(set.conns, cn)
	if len(set.conns) == 0 {
		delete(c.conns, convID)
	}
}

// targets returns a snapshot of the live sockets for convID. The caller writes
// outside the registry lock so one slow client can't stall other deliveries.
func (c *Channel) targets(convID string) []*wsConn {
	c.connsMu.Lock()
	defer c.connsMu.Unlock()
	set, ok := c.conns[convID]
	if !ok {
		return nil
	}
	out := make([]*wsConn, 0, len(set.conns))
	for cn := range set.conns {
		out = append(out, cn)
	}
	return out
}

// isControlReply reports whether an inbound frame is a tool-confirmation
// decision (prompt_type=confirmation_response). Used to forward the structured
// approve/reject decision to core as metadata["confirmation"]. The reply text
// itself (a localized "Approve"/"Reject" label) is still echoed to the user's
// other tabs like any user message — seeing it is how a sibling tab learns its
// own open confirmation was answered here.
func isControlReply(meta map[string]any) bool {
	pt, _ := meta["prompt_type"].(string)
	return pt == "confirmation_response"
}

// broadcastUserInput echoes a user's inbound text to their OTHER live
// connections on the same conversation, so a message typed in one tab appears
// in their other open tabs immediately — not only after a reload re-pulls the
// server transcript. The sending socket is skipped: it already rendered the
// message locally. Carries metadata type "user_message" so the client renders
// it as a user bubble rather than an assistant reply.
func (c *Channel) broadcastUserInput(convID string, sender *wsConn, content string) {
	targets := c.targets(convID)
	if len(targets) <= 1 {
		return // sender is the only connection; nothing to echo
	}
	frame := outboundFrame{
		ConversationID: convID,
		Content:        content,
		Metadata:       map[string]string{"type": "user_message"},
	}
	data, err := json.Marshal(frame)
	if err != nil {
		return
	}
	for _, cn := range targets {
		if cn == sender {
			continue
		}
		// Decouple sibling-echo writes from the SENDER's connection lifetime:
		// the sender disconnecting must not cut short — or, via the library's
		// born-cancelled-context teardown, prematurely close — delivery to the
		// user's OTHER tabs. Independent timeout, like the assistant fan-out in Send.
		wctx, cancel := context.WithTimeout(context.Background(), writeTimeout)
		cn.mu.Lock()
		werr := cn.ws.Write(wctx, websocket.MessageText, data)
		cn.mu.Unlock()
		cancel()
		if werr != nil {
			slog.Debug("websocket user-echo: write failed", "conv", convID, "error", werr)
		}
	}
}

// ── identity resolution ─────────────────────────────────────────────────────

// whoamiResolver maps a profile token to its owning user id (Timly user.id) by
// calling the same /whoami authority the core uses. It lives in the channel
// because grouping a user's sockets has to happen at connect time — before the
// core ever sees a message. Timly stays the single source of truth; this is a
// client of it, not a second authority.
type whoamiResolver struct {
	url    string
	secret string
	client *http.Client
}

// newWhoamiResolver builds a resolver. The secret falls back to the inherited
// WHOAMI_SECRET env var when not given in config, so it is not duplicated next
// to the core's profiles.who_am_i block. A missing url is fatal: without it the
// channel cannot resolve identity and would refuse every connection — fail at
// boot rather than silently per-connection under load.
func newWhoamiResolver(url, secret string) (*whoamiResolver, error) {
	if url == "" {
		return nil, errors.New("websocket channel: whoami_url is required for identity resolution")
	}
	if secret == "" {
		secret = os.Getenv("WHOAMI_SECRET")
	}
	if secret == "" {
		slog.Warn("websocket channel: whoami secret is empty — /whoami will reject if it requires X-Security-Token")
	}
	return &whoamiResolver{
		url:    url,
		secret: secret,
		client: &http.Client{Timeout: 5 * time.Second},
	}, nil
}

// resolve returns the owning user id for token. Fail-closed: any transport,
// non-2xx, decode, or empty-id outcome yields an error and the caller refuses
// the socket. Mirrors the core verifier's contract (X-User-ID + X-Channel-Type
// + X-Security-Token; entity_id read from the JSON body).
func (r *whoamiResolver) resolve(ctx context.Context, token string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.url, nil)
	if err != nil {
		return "", fmt.Errorf("whoami: build request: %w", err)
	}
	req.Header.Set("X-User-ID", token)
	req.Header.Set("X-Channel-Type", ID)
	if r.secret != "" {
		req.Header.Set("X-Security-Token", r.secret)
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("whoami: request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("whoami: status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return "", fmt.Errorf("whoami: read body: %w", err)
	}
	var payload struct {
		EntityID string `json:"entity_id"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", fmt.Errorf("whoami: decode body: %w", err)
	}
	if payload.EntityID == "" {
		return "", errors.New("whoami: response missing entity_id")
	}
	return payload.EntityID, nil
}
