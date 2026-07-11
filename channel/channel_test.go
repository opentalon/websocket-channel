package wschannel

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	pkg "github.com/opentalon/opentalon/pkg/channel"
)

// ── Unit tests ────────────────────────────────────────────────────────────────

func TestID(t *testing.T) {
	if ID != "websocket" {
		t.Errorf("ID = %q, want \"websocket\"", ID)
	}
}

func TestNew_defaults(t *testing.T) {
	ch := New(Config{})
	if ch == nil {
		t.Fatal("New() returned nil")
	}
	if ch.cfg.Addr != "0.0.0.0:9000" {
		t.Errorf("default Addr = %q, want \"0.0.0.0:9000\"", ch.cfg.Addr)
	}
	if ch.cfg.Path != "/ws" {
		t.Errorf("default Path = %q, want \"/ws\"", ch.cfg.Path)
	}
}

func TestNew_customConfig(t *testing.T) {
	ch := New(Config{Addr: "127.0.0.1:8080", Path: "/chat"})
	if ch.cfg.Addr != "127.0.0.1:8080" {
		t.Errorf("Addr = %q, want \"127.0.0.1:8080\"", ch.cfg.Addr)
	}
	if ch.cfg.Path != "/chat" {
		t.Errorf("Path = %q, want \"/chat\"", ch.cfg.Path)
	}
}

func TestChannelID(t *testing.T) {
	ch := New(Config{})
	if ch.ID() != "websocket" {
		t.Errorf("ID() = %q, want \"websocket\"", ch.ID())
	}
}

func TestCapabilities(t *testing.T) {
	ch := New(Config{})
	caps := ch.Capabilities()

	if caps.ID != "websocket" {
		t.Errorf("Capabilities().ID = %q, want \"websocket\"", caps.ID)
	}
	if caps.Name != "WebSocket" {
		t.Errorf("Capabilities().Name = %q, want \"WebSocket\"", caps.Name)
	}
	if !caps.Files {
		t.Error("Capabilities().Files should be true")
	}
	if caps.Threads {
		t.Error("Capabilities().Threads should be false")
	}
	if caps.Reactions {
		t.Error("Capabilities().Reactions should be false")
	}
	if !caps.Edits {
		t.Error("Capabilities().Edits should be true")
	}
	if caps.MaxMessageLength != 64*1024 {
		t.Errorf("Capabilities().MaxMessageLength = %d, want %d", caps.MaxMessageLength, 64*1024)
	}
	if caps.ResponseFormat != pkg.FormatMarkdown {
		t.Errorf("Capabilities().ResponseFormat = %q, want %q", caps.ResponseFormat, pkg.FormatMarkdown)
	}
}

func TestConfigure(t *testing.T) {
	ch := New(Config{})
	err := ch.Configure(map[string]interface{}{
		"addr":         "0.0.0.0:7777",
		"path":         "/chat",
		"cors_origins": []interface{}{"https://a.com", "https://b.com"},
	})
	if err != nil {
		t.Fatalf("Configure() = %v", err)
	}
	if ch.cfg.Addr != "0.0.0.0:7777" {
		t.Errorf("cfg.Addr = %q, want \"0.0.0.0:7777\"", ch.cfg.Addr)
	}
	if ch.cfg.Path != "/chat" {
		t.Errorf("cfg.Path = %q, want \"/chat\"", ch.cfg.Path)
	}
	if len(ch.cfg.CORSOrigins) != 2 || ch.cfg.CORSOrigins[0] != "https://a.com" {
		t.Errorf("cfg.CORSOrigins = %v", ch.cfg.CORSOrigins)
	}
}

func TestConfigure_emptyValuesIgnored(t *testing.T) {
	ch := New(Config{Addr: "0.0.0.0:9000", Path: "/ws"})
	_ = ch.Configure(map[string]interface{}{
		"addr": "",
		"path": "",
	})
	if ch.cfg.Addr != "0.0.0.0:9000" {
		t.Errorf("empty addr should not override, got %q", ch.cfg.Addr)
	}
	if ch.cfg.Path != "/ws" {
		t.Errorf("empty path should not override, got %q", ch.cfg.Path)
	}
}

func TestStop_beforeStart(t *testing.T) {
	ch := New(Config{})
	if err := ch.Stop(); err != nil {
		t.Errorf("Stop() before Start = %v", err)
	}
	// idempotent
	if err := ch.Stop(); err != nil {
		t.Errorf("Stop() second call = %v", err)
	}
}

func TestSend_unknownConversation(t *testing.T) {
	ch := New(Config{})
	err := ch.Send(context.Background(), pkg.OutboundMessage{
		ConversationID: "nonexistent",
		Content:        "hello",
	})
	if err != nil {
		t.Errorf("Send() to unknown conversation = %v, want nil", err)
	}
}

func TestNewID_format(t *testing.T) {
	id := newID()
	if len(id) != 32 {
		t.Errorf("newID() length = %d, want 32", len(id))
	}
	for _, c := range id {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			t.Errorf("newID() contains non-hex char %q in %q", c, id)
		}
	}
}

func TestNewID_unique(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 100; i++ {
		id := newID()
		if seen[id] {
			t.Fatalf("newID() produced duplicate: %q", id)
		}
		seen[id] = true
	}
}

// ── Integration tests (real HTTP/WebSocket server) ────────────────────────────

// fakeWhoami stands in for Timly's /whoami. It maps the X-User-ID (token)
// header to an entity_id via mapping; a token absent from the map resolves to
// the token string itself, so single-token tests get a stable user without
// ceremony. An entity_id that resolves to "" yields HTTP 404 (token rejected).
func fakeWhoami(t *testing.T, mapping map[string]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := r.Header.Get("X-User-ID")
		entityID, ok := mapping[token]
		if !ok {
			entityID = token
		}
		if entityID == "" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{
			"entity_id": entityID,
			"group":     "g-" + entityID,
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// testServer starts the channel's HTTP handler on an httptest.Server with a
// default fake /whoami (every token is its own user) and returns the channel,
// inbox (readable end), server, and a cleanup function.
func testServer(t *testing.T) (*Channel, <-chan pkg.InboundMessage, *httptest.Server, func()) {
	t.Helper()
	return testServerWithUsers(t, nil)
}

// testServerWithUsers is testServer with an explicit token→entity_id map, so a
// test can model several tabs of ONE user (different tokens → same entity) or
// a genuine cross-user collision (different tokens → different entities).
func testServerWithUsers(t *testing.T, mapping map[string]string) (*Channel, <-chan pkg.InboundMessage, *httptest.Server, func()) {
	t.Helper()
	who := fakeWhoami(t, mapping)
	ch := New(Config{Path: "/ws"})
	resolver, err := newWhoamiResolver(who.URL, "test-secret")
	if err != nil {
		t.Fatalf("newWhoamiResolver: %v", err)
	}
	ch.resolver = resolver
	inbox := make(chan pkg.InboundMessage, 16)
	ch.inbox = inbox

	mux := http.NewServeMux()
	mux.HandleFunc("/ws", ch.handleUpgrade)
	mux.HandleFunc("/ws/inject", ch.handleInject)
	srv := httptest.NewServer(mux)

	return ch, inbox, srv, func() {
		srv.Close()
		_ = ch.Stop()
	}
}

func TestHandleInject_pushesHiddenMessageToInbox(t *testing.T) {
	_, inbox, srv, cleanup := testServer(t)
	defer cleanup()

	body := `{"token":"u1","conversation_id":"conv1","content":"[system] job done","visibility":"hidden","resume_intent":"true"}`
	resp, err := http.Post(srv.URL+"/ws/inject", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST inject: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", resp.StatusCode)
	}

	select {
	case msg := <-inbox:
		if msg.ConversationID != "conv1" {
			t.Errorf("ConversationID = %q, want conv1", msg.ConversationID)
		}
		if msg.Content != "[system] job done" {
			t.Errorf("Content = %q", msg.Content)
		}
		if msg.Metadata["visibility"] != "hidden" {
			t.Errorf("visibility = %q, want hidden", msg.Metadata["visibility"])
		}
		if msg.Metadata[pkg.ResumeIntentMetadataKey] != "true" {
			t.Errorf("resume_intent = %q, want true", msg.Metadata[pkg.ResumeIntentMetadataKey])
		}
		if msg.Metadata["profile_token"] != "u1" {
			t.Errorf("profile_token = %q, want u1", msg.Metadata["profile_token"])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no message pushed to inbox")
	}
}

// TestHandleInject_ownershipGate is the security boundary: a token resolving to
// user u2 must NOT inject into a conversation whose live sockets are owned by
// u1 (else the core's reply, addressed by the raw conversation id, would fan out
// to u1's sockets). The conversation's own owner is still accepted.
func TestHandleInject_ownershipGate(t *testing.T) {
	ch, inbox, srv, cleanup := testServer(t)
	defer cleanup()

	// u1 owns "conv1": register a live socket under it (default whoami maps a
	// token to its own entity, so "u1" is entity u1).
	if err := ch.addConn("conv1", "u1", &wsConn{}); err != nil {
		t.Fatalf("addConn: %v", err)
	}

	// A foreign user (u2) is refused with 403 and never reaches the core.
	foreign := `{"token":"u2","conversation_id":"conv1","content":"[system] x"}`
	resp, err := http.Post(srv.URL+"/ws/inject", "application/json", strings.NewReader(foreign))
	if err != nil {
		t.Fatalf("POST foreign: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("foreign inject: status = %d, want 403", resp.StatusCode)
	}
	select {
	case msg := <-inbox:
		t.Fatalf("foreign inject must not reach the core, got %+v", msg)
	case <-time.After(100 * time.Millisecond):
	}

	// The conversation's own owner (u1) is still accepted even with a live socket.
	own := `{"token":"u1","conversation_id":"conv1","content":"[system] job done"}`
	resp2, err := http.Post(srv.URL+"/ws/inject", "application/json", strings.NewReader(own))
	if err != nil {
		t.Fatalf("POST own: %v", err)
	}
	_ = resp2.Body.Close()
	if resp2.StatusCode != http.StatusAccepted {
		t.Fatalf("owner inject: status = %d, want 202", resp2.StatusCode)
	}
	select {
	case <-inbox: // good, enqueued
	case <-time.After(2 * time.Second):
		t.Fatal("owner inject did not reach the core")
	}
}

func TestHandleInject_rejectsBadTokenAndMissingFields(t *testing.T) {
	// Token "" resolves to entity_id "" → whoami 404 → unauthorized.
	_, _, srv, cleanup := testServerWithUsers(t, map[string]string{"bad": ""})
	defer cleanup()

	resp, err := http.Post(srv.URL+"/ws/inject", "application/json",
		strings.NewReader(`{"token":"bad","conversation_id":"c","content":"x"}`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("bad token: status = %d, want 401", resp.StatusCode)
	}

	resp2, err := http.Post(srv.URL+"/ws/inject", "application/json",
		strings.NewReader(`{"token":"u1","conversation_id":"c"}`)) // no content
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	_ = resp2.Body.Close()
	if resp2.StatusCode != http.StatusBadRequest {
		t.Errorf("missing content: status = %d, want 400", resp2.StatusCode)
	}

	// Any non-POST method is rejected before touching the body or the resolver.
	respGet, err := http.Get(srv.URL + "/ws/inject")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	_ = respGet.Body.Close()
	if respGet.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET: status = %d, want 405", respGet.StatusCode)
	}

	// A malformed JSON body is rejected with 400.
	respBad, err := http.Post(srv.URL+"/ws/inject", "application/json", strings.NewReader(`{not json`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	_ = respBad.Body.Close()
	if respBad.StatusCode != http.StatusBadRequest {
		t.Errorf("malformed body: status = %d, want 400", respBad.StatusCode)
	}
}

// dialConvID dials the channel with a token and (optional) conversation_id and
// returns the connection plus the welcome conversation_id. Caller closes conn.
func dialConvID(t *testing.T, ctx context.Context, srv *httptest.Server, token, convID string) (*websocket.Conn, string) {
	t.Helper()
	u := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws?token=" + token
	if convID != "" {
		u += "&conversation_id=" + convID
	}
	conn, _, err := websocket.Dial(ctx, u, nil)
	if err != nil {
		t.Fatalf("Dial(token=%q, conv=%q) = %v", token, convID, err)
	}
	return conn, readWelcome(t, ctx, conn)
}

func wsURL(srv *httptest.Server, token string) string {
	u := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws"
	if token != "" {
		u += "?token=" + token
	}
	return u
}

// readWelcome reads and discards the welcome frame the server sends on connect.
// Returns the conversation_id from the welcome frame.
func readWelcome(t *testing.T, ctx context.Context, conn *websocket.Conn) string {
	t.Helper()
	_, raw, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("readWelcome: %v", err)
	}
	var frame outboundFrame
	if err := json.Unmarshal(raw, &frame); err != nil {
		t.Fatalf("readWelcome unmarshal: %v", err)
	}
	if frame.Metadata["type"] != "connected" {
		t.Fatalf("readWelcome: expected type=connected, got %v", frame.Metadata)
	}
	return frame.ConversationID
}

// recvInbox reads the next NON-control message from the core inbox, skipping the
// resume-handshake control frame that a resumed dial (conversation_id supplied)
// now emits on connect. Use it wherever a test asserts on the user's message.
func recvInbox(t *testing.T, ctx context.Context, inbox <-chan pkg.InboundMessage) pkg.InboundMessage {
	t.Helper()
	for {
		select {
		case msg := <-inbox:
			if msg.Metadata[controlMetadataKey] != "" {
				continue
			}
			return msg
		case <-ctx.Done():
			t.Fatal("no non-control message reached inbox")
			return pkg.InboundMessage{}
		}
	}
}

func TestConnect_withQueryToken(t *testing.T) {
	ch, inbox, srv, cleanup := testServer(t)
	_ = ch
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	conn, _, err := websocket.Dial(ctx, wsURL(srv, "my-token"), nil)
	if err != nil {
		t.Fatalf("Dial() = %v", err)
	}
	defer func() { _ = conn.CloseNow() }()
	readWelcome(t, ctx, conn)

	// Send a message and verify it arrives in inbox with correct token.
	frame := inboundFrame{Content: "hello"}
	data, _ := json.Marshal(frame)
	if err := conn.Write(ctx, websocket.MessageText, data); err != nil {
		t.Fatalf("Write() = %v", err)
	}

	select {
	case msg := <-inbox:
		if msg.Metadata["profile_token"] != "my-token" {
			t.Errorf("profile_token = %q, want \"my-token\"", msg.Metadata["profile_token"])
		}
		if msg.Content != "hello" {
			t.Errorf("Content = %q, want \"hello\"", msg.Content)
		}
		if msg.ChannelID != "websocket" {
			t.Errorf("ChannelID = %q, want \"websocket\"", msg.ChannelID)
		}
		if msg.ConversationID == "" {
			t.Error("ConversationID should not be empty")
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for inbound message")
	}
}

func TestConnect_withBearerHeader(t *testing.T) {
	_, inbox, srv, cleanup := testServer(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	opts := &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer bearer-token"}},
	}
	conn, _, err := websocket.Dial(ctx, wsURL(srv, ""), opts)
	if err != nil {
		t.Fatalf("Dial() with Bearer header = %v", err)
	}
	defer func() { _ = conn.CloseNow() }()
	readWelcome(t, ctx, conn)

	frame := inboundFrame{Content: "hi"}
	data, _ := json.Marshal(frame)
	_ = conn.Write(ctx, websocket.MessageText, data)

	select {
	case msg := <-inbox:
		if msg.Metadata["profile_token"] != "bearer-token" {
			t.Errorf("profile_token = %q, want \"bearer-token\"", msg.Metadata["profile_token"])
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for inbound message")
	}
}

func TestConnect_noToken_rejected(t *testing.T) {
	_, _, srv, cleanup := testServer(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	_, resp, err := websocket.Dial(ctx, wsURL(srv, ""), nil)
	if err == nil {
		t.Fatal("expected Dial() to fail without token")
	}
	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("expected HTTP 401, got %v", resp)
	}
}

func TestSend_deliversToClient(t *testing.T) {
	ch, inbox, srv, cleanup := testServer(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	conn, _, err := websocket.Dial(ctx, wsURL(srv, "tok"), nil)
	if err != nil {
		t.Fatalf("Dial() = %v", err)
	}
	defer func() { _ = conn.CloseNow() }()
	readWelcome(t, ctx, conn)

	// Get the conversation ID assigned to this connection.
	frame := inboundFrame{Content: "ping"}
	data, _ := json.Marshal(frame)
	_ = conn.Write(ctx, websocket.MessageText, data)

	var convID string
	select {
	case msg := <-inbox:
		convID = msg.ConversationID
	case <-ctx.Done():
		t.Fatal("timed out waiting for ping")
	}

	// Now send a response back via Send().
	if err := ch.Send(ctx, pkg.OutboundMessage{
		ConversationID: convID,
		Content:        "<p>pong</p>",
	}); err != nil {
		t.Fatalf("Send() = %v", err)
	}

	// Read it from the client side.
	_, raw, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("Read() = %v", err)
	}
	var out outboundFrame
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if out.Content != "<p>pong</p>" {
		t.Errorf("response Content = %q, want \"<p>pong</p>\"", out.Content)
	}
	if out.ConversationID != convID {
		t.Errorf("response ConversationID = %q, want %q", out.ConversationID, convID)
	}
}

func TestSend_typingIndicator(t *testing.T) {
	ch, inbox, srv, cleanup := testServer(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	conn, _, err := websocket.Dial(ctx, wsURL(srv, "tok"), nil)
	if err != nil {
		t.Fatalf("Dial() = %v", err)
	}
	defer func() { _ = conn.CloseNow() }()
	readWelcome(t, ctx, conn)

	// Get conversation ID.
	data, _ := json.Marshal(inboundFrame{Content: "hi"})
	_ = conn.Write(ctx, websocket.MessageText, data)
	var convID string
	select {
	case msg := <-inbox:
		convID = msg.ConversationID
	case <-ctx.Done():
		t.Fatal("timed out")
	}

	// Send a typing indicator.
	if err := ch.Send(ctx, pkg.OutboundMessage{
		ConversationID: convID,
		Metadata:       map[string]string{"_typing": "true"},
	}); err != nil {
		t.Fatalf("Send typing = %v", err)
	}

	_, raw, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("Read() = %v", err)
	}
	var out outboundFrame
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !out.Typing {
		t.Error("expected typing=true in frame")
	}
	if out.Content != "" {
		t.Errorf("typing frame should have empty content, got %q", out.Content)
	}
}

// TestInbound_clientVisibilityIsIgnored is the security regression guard: a
// browser frame that sets visibility=hidden must NOT be honored — hiding a turn
// from the audited transcript is reserved for the trusted /inject path. The
// forwarded message must carry no visibility metadata.
func TestInbound_clientVisibilityIsIgnored(t *testing.T) {
	_, inbox, srv, cleanup := testServer(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	conn, _, err := websocket.Dial(ctx, wsURL(srv, "tok"), nil)
	if err != nil {
		t.Fatalf("Dial() = %v", err)
	}
	defer func() { _ = conn.CloseNow() }()
	readWelcome(t, ctx, conn)

	// Raw frame carrying a visibility field a client must not be able to set.
	_ = conn.Write(ctx, websocket.MessageText, []byte(`{"content":"sneaky","visibility":"hidden"}`))

	select {
	case msg := <-inbox:
		if v, ok := msg.Metadata["visibility"]; ok {
			t.Errorf("client visibility was honored: metadata[visibility]=%q, want absent", v)
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for inbound message")
	}
}

func TestInbound_withFileAttachment(t *testing.T) {
	_, inbox, srv, cleanup := testServer(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	conn, _, err := websocket.Dial(ctx, wsURL(srv, "tok"), nil)
	if err != nil {
		t.Fatalf("Dial() = %v", err)
	}
	defer func() { _ = conn.CloseNow() }()
	readWelcome(t, ctx, conn)

	fileBytes := []byte("col1,col2\n1,2\n3,4")
	frame := inboundFrame{
		Content: "analyse this",
		Files: []fileFrame{
			{
				Name:     "data.csv",
				MimeType: "text/csv",
				Data:     base64.StdEncoding.EncodeToString(fileBytes),
			},
		},
	}
	data, _ := json.Marshal(frame)
	_ = conn.Write(ctx, websocket.MessageText, data)

	select {
	case msg := <-inbox:
		if len(msg.Files) != 1 {
			t.Fatalf("Files len = %d, want 1", len(msg.Files))
		}
		f := msg.Files[0]
		if f.Name != "data.csv" {
			t.Errorf("File.Name = %q, want \"data.csv\"", f.Name)
		}
		if f.MimeType != "text/csv" {
			t.Errorf("File.MimeType = %q, want \"text/csv\"", f.MimeType)
		}
		if string(f.Data) != string(fileBytes) {
			t.Errorf("File.Data = %q, want %q", f.Data, fileBytes)
		}
		if f.Size != int64(len(fileBytes)) {
			t.Errorf("File.Size = %d, want %d", f.Size, len(fileBytes))
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for inbound message with file")
	}
}

func TestInbound_emptyFrame_skipped(t *testing.T) {
	_, inbox, srv, cleanup := testServer(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	conn, _, err := websocket.Dial(ctx, wsURL(srv, "tok"), nil)
	if err != nil {
		t.Fatalf("Dial() = %v", err)
	}
	defer func() { _ = conn.CloseNow() }()
	readWelcome(t, ctx, conn)

	empty := inboundFrame{}
	data, _ := json.Marshal(empty)
	_ = conn.Write(ctx, websocket.MessageText, data)

	select {
	case msg := <-inbox:
		t.Errorf("expected empty frame to be skipped, got msg: %+v", msg)
	case <-ctx.Done():
		// expected: nothing in inbox
	}
}

func TestInbound_resumeIntent_setWhenClientSuppliedConvID(t *testing.T) {
	// Client-supplied conversation_id at handshake must propagate as
	// metadata["resume_intent"]="true" on every message from that
	// connection. The core handler routes Load (strict) vs Create
	// (idempotent) based on this flag — getting it wrong is the root
	// of the silent-session-drift bug on standby/wake.
	_, inbox, srv, cleanup := testServer(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	u := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws?token=tok&conversation_id=existing-conv"
	conn, _, err := websocket.Dial(ctx, u, nil)
	if err != nil {
		t.Fatalf("Dial() = %v", err)
	}
	defer func() { _ = conn.CloseNow() }()
	convID := readWelcome(t, ctx, conn)
	if convID != "existing-conv" {
		t.Fatalf("welcome convID = %q, want \"existing-conv\" (server must echo client id)", convID)
	}

	data, _ := json.Marshal(inboundFrame{Content: "still here?"})
	_ = conn.Write(ctx, websocket.MessageText, data)

	select {
	case msg := <-inbox:
		if got := msg.Metadata[pkg.ResumeIntentMetadataKey]; got != "true" {
			t.Errorf("Metadata[resume_intent] = %q, want \"true\"", got)
		}
		if msg.ConversationID != "existing-conv" {
			t.Errorf("ConversationID = %q, want \"existing-conv\"", msg.ConversationID)
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for inbound message")
	}
}

func TestInbound_resumeIntent_absentOnFreshHandshake(t *testing.T) {
	// No client-supplied conversation_id => server mints => Create path
	// in the core handler. resume_intent must be absent (not "false"),
	// matching the absence semantics the handler expects.
	_, inbox, srv, cleanup := testServer(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	conn, _, err := websocket.Dial(ctx, wsURL(srv, "tok"), nil)
	if err != nil {
		t.Fatalf("Dial() = %v", err)
	}
	defer func() { _ = conn.CloseNow() }()
	readWelcome(t, ctx, conn)

	data, _ := json.Marshal(inboundFrame{Content: "fresh start"})
	_ = conn.Write(ctx, websocket.MessageText, data)

	select {
	case msg := <-inbox:
		if _, present := msg.Metadata[pkg.ResumeIntentMetadataKey]; present {
			t.Errorf("Metadata[resume_intent] should be absent on fresh handshake, got %q",
				msg.Metadata[pkg.ResumeIntentMetadataKey])
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for inbound message")
	}
}

func TestConversationID_uniquePerConnection(t *testing.T) {
	_, inbox, srv, cleanup := testServer(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	dialAndGetConvID := func() string {
		conn, _, err := websocket.Dial(ctx, wsURL(srv, "tok"), nil)
		if err != nil {
			t.Fatalf("Dial() = %v", err)
		}
		defer func() { _ = conn.CloseNow() }()
		readWelcome(t, ctx, conn)
		data, _ := json.Marshal(inboundFrame{Content: "hi"})
		_ = conn.Write(ctx, websocket.MessageText, data)
		select {
		case msg := <-inbox:
			return msg.ConversationID
		case <-ctx.Done():
			t.Fatal("timeout")
			return ""
		}
	}

	id1 := dialAndGetConvID()
	id2 := dialAndGetConvID()

	if id1 == id2 {
		t.Errorf("two connections got the same conversation_id: %q", id1)
	}
}

// ── 1:N multi-connection ──────────────────────────────────────────────────────

// readContent reads one frame from conn and returns it parsed.
func readContent(t *testing.T, ctx context.Context, conn *websocket.Conn) outboundFrame {
	t.Helper()
	_, raw, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	var f outboundFrame
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return f
}

func TestUpgrade_sameUserMultipleConnections_fanOut(t *testing.T) {
	// Two tabs of the SAME user on the SAME conversation (different tokens, as a
	// real browser mints one per connect). Both must connect — no token-equality
	// rejection — and a single Send must fan out to BOTH. This is the core bug
	// the refactor fixes.
	ch, _, srv, cleanup := testServerWithUsers(t, map[string]string{
		"tabA": "user1",
		"tabB": "user1",
	})
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	connA, idA := dialConvID(t, ctx, srv, "tabA", "shared")
	defer func() { _ = connA.CloseNow() }()
	connB, idB := dialConvID(t, ctx, srv, "tabB", "shared")
	defer func() { _ = connB.CloseNow() }()

	if idA != "shared" || idB != "shared" {
		t.Fatalf("both connections should share conv id; got %q and %q", idA, idB)
	}

	if err := ch.Send(ctx, pkg.OutboundMessage{ConversationID: "shared", Content: "hi all"}); err != nil {
		t.Fatalf("Send() = %v", err)
	}

	if got := readContent(t, ctx, connA).Content; got != "hi all" {
		t.Errorf("tab A content = %q, want \"hi all\"", got)
	}
	if got := readContent(t, ctx, connB).Content; got != "hi all" {
		t.Errorf("tab B content = %q, want \"hi all\"", got)
	}
}

func TestUpgrade_refusesCrossUserOnSameConversation(t *testing.T) {
	// A token resolving to a DIFFERENT user than the conversation's owner is
	// refused with StatusPolicyViolation (1008). The legitimate owner's
	// connection is untouched and keeps receiving. This is security case #8.
	ch, _, srv, cleanup := testServerWithUsers(t, map[string]string{
		"tokA": "user1",
		"tokB": "user2",
	})
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	connA, _ := dialConvID(t, ctx, srv, "tokA", "shared")
	defer func() { _ = connA.CloseNow() }()

	// Second user dials the same conversation id. The HTTP upgrade succeeds,
	// then the server closes the socket with a policy violation before any
	// welcome frame — so we read the close directly, not a welcome.
	uB := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws?token=tokB&conversation_id=shared"
	connB, _, err := websocket.Dial(ctx, uB, nil)
	if err != nil {
		t.Fatalf("cross-user Dial handshake failed unexpectedly: %v", err)
	}
	defer func() { _ = connB.CloseNow() }()
	if _, _, rerr := connB.Read(ctx); websocket.CloseStatus(rerr) != websocket.StatusPolicyViolation {
		t.Fatalf("cross-user second connection: CloseStatus = %d, want %d (PolicyViolation)",
			websocket.CloseStatus(rerr), websocket.StatusPolicyViolation)
	}

	// The owner's connection still works and never saw the intruder's traffic.
	if err := ch.Send(ctx, pkg.OutboundMessage{ConversationID: "shared", Content: "still mine"}); err != nil {
		t.Fatalf("Send() to owner = %v", err)
	}
	if got := readContent(t, ctx, connA).Content; got != "still mine" {
		t.Errorf("owner content = %q, want \"still mine\"", got)
	}
}

func TestReadLoop_userInputEchoedToSiblingsNotSender(t *testing.T) {
	// A message typed in one tab must appear in the user's OTHER tabs live (the
	// server echoes the inbound to siblings), but the sender must NOT get its
	// own message back (it rendered it locally).
	ch, inbox, srv, cleanup := testServerWithUsers(t, map[string]string{"tabA": "user1", "tabB": "user1"})
	defer cleanup()
	_ = ch

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	connA, _ := dialConvID(t, ctx, srv, "tabA", "shared")
	defer func() { _ = connA.CloseNow() }()
	connB, _ := dialConvID(t, ctx, srv, "tabB", "shared")
	defer func() { _ = connB.CloseNow() }()

	data, _ := json.Marshal(inboundFrame{Content: "hello siblings"})
	if err := connA.Write(ctx, websocket.MessageText, data); err != nil {
		t.Fatalf("write: %v", err)
	}

	// The inbound still reaches the core inbox (skip the resume-handshake
	// control frames the two resumed dials emit on connect).
	if msg := recvInbox(t, ctx, inbox); msg.Content != "hello siblings" {
		t.Errorf("inbox content = %q, want \"hello siblings\"", msg.Content)
	}

	// Sibling tab B receives the echo as a user_message.
	echo := readContent(t, ctx, connB)
	if echo.Metadata["type"] != "user_message" {
		t.Errorf("sibling frame type = %q, want \"user_message\"", echo.Metadata["type"])
	}
	if echo.Content != "hello siblings" {
		t.Errorf("sibling echo content = %q, want \"hello siblings\"", echo.Content)
	}

	// Sender tab A must NOT receive an echo of its own message.
	shortCtx, shortCancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
	defer shortCancel()
	if _, _, err := connA.Read(shortCtx); err == nil {
		t.Error("sender should not receive an echo of its own message")
	}
}

func TestReadLoop_confirmationReplyEchoedToSiblings(t *testing.T) {
	// A tool-confirmation click now sends a readable localized label ("Approve")
	// as content, so it IS echoed to sibling tabs like any user message — that
	// echo is what tells the other tab its own still-open confirmation was
	// answered here, so it retires those buttons instead of leaving them
	// clickable. The reply still reaches core (with the confirmation decision)
	// and is NOT echoed back to the sending tab.
	_, inbox, srv, cleanup := testServerWithUsers(t, map[string]string{"tabA": "user1", "tabB": "user1"})
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	connA, _ := dialConvID(t, ctx, srv, "tabA", "shared")
	defer func() { _ = connA.CloseNow() }()
	connB, _ := dialConvID(t, ctx, srv, "tabB", "shared")
	defer func() { _ = connB.CloseNow() }()

	data, _ := json.Marshal(inboundFrame{
		Content:  "Approve",
		Metadata: map[string]any{"prompt_type": "confirmation_response", "action": "approve"},
	})
	if err := connA.Write(ctx, websocket.MessageText, data); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Reaches core with the deterministic confirmation decision so the write
	// actually resolves (skip the resume-handshake control frames on connect).
	msg := recvInbox(t, ctx, inbox)
	if msg.Content != "Approve" {
		t.Errorf("inbox content = %q, want \"Approve\"", msg.Content)
	}
	if msg.Metadata["confirmation"] != "approve" {
		t.Errorf("inbox confirmation = %q, want \"approve\"", msg.Metadata["confirmation"])
	}

	// Sibling tab B DOES receive the echo as a user_message bubble.
	echo := readContent(t, ctx, connB)
	if echo.Metadata["type"] != "user_message" {
		t.Errorf("sibling frame type = %q, want \"user_message\"", echo.Metadata["type"])
	}
	if echo.Content != "Approve" {
		t.Errorf("sibling echo content = %q, want \"Approve\"", echo.Content)
	}

	// Sender tab A must NOT receive an echo of its own reply.
	shortCtx, shortCancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
	defer shortCancel()
	if _, _, err := connA.Read(shortCtx); err == nil {
		t.Error("sender should not receive an echo of its own confirmation reply")
	}
}

func TestUpgrade_resumeConnect_EmitsResumeHello(t *testing.T) {
	// A reconnect (client-supplied conversation_id) emits exactly one
	// resume-handshake control message to core so a pending confirmation can be
	// re-emitted — carrying resume_intent + the profile token, with empty
	// content. A fresh connect (server-minted id) emits none.
	_, inbox, srv, cleanup := testServerWithUsers(t, map[string]string{"tabA": "user1"})
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	conn, _ := dialConvID(t, ctx, srv, "tabA", "shared")
	defer func() { _ = conn.CloseNow() }()

	select {
	case msg := <-inbox:
		if msg.Metadata[controlMetadataKey] != controlResumeHello {
			t.Errorf("control = %q, want %q", msg.Metadata[controlMetadataKey], controlResumeHello)
		}
		if msg.Metadata[pkg.ResumeIntentMetadataKey] != "true" {
			t.Errorf("resume_intent = %q, want true", msg.Metadata[pkg.ResumeIntentMetadataKey])
		}
		if msg.Metadata["profile_token"] != "tabA" {
			t.Errorf("profile_token = %q, want tabA", msg.Metadata["profile_token"])
		}
		if msg.Content != "" {
			t.Errorf("resume hello content = %q, want empty", msg.Content)
		}
		if msg.ConversationID != "shared" {
			t.Errorf("conversation_id = %q, want shared", msg.ConversationID)
		}
	case <-ctx.Done():
		t.Fatal("resume hello never reached inbox")
	}

	// Fresh connect (no conversation_id → server-minted id) must NOT emit a
	// resume_hello: only the user's own message reaches core, with no control
	// key. The resume hello above was already drained, so the next inbox item
	// is the fresh message itself.
	fresh, _ := dialConvID(t, ctx, srv, "tabA", "")
	defer func() { _ = fresh.CloseNow() }()
	data, _ := json.Marshal(inboundFrame{Content: "hi fresh"})
	if err := fresh.Write(ctx, websocket.MessageText, data); err != nil {
		t.Fatalf("fresh write: %v", err)
	}
	select {
	case msg := <-inbox:
		if msg.Metadata[controlMetadataKey] != "" {
			t.Errorf("fresh connect emitted a control frame: %+v", msg.Metadata)
		}
		if msg.Content != "hi fresh" {
			t.Errorf("fresh inbox content = %q, want \"hi fresh\"", msg.Content)
		}
	case <-ctx.Done():
		t.Fatal("fresh user message never reached inbox")
	}
}

func TestUpgrade_whoamiRejects_unauthorized(t *testing.T) {
	// A token /whoami rejects (empty entity_id → 404) must never get a live
	// socket: the upgrade fails closed with HTTP 401, no WebSocket.
	_, _, srv, cleanup := testServerWithUsers(t, map[string]string{"bad-token": ""})
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	_, resp, err := websocket.Dial(ctx, wsURL(srv, "bad-token"), nil)
	if err == nil {
		t.Fatal("expected Dial() to fail for an unresolvable token")
	}
	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("expected HTTP 401 for unresolvable token, got %v", resp)
	}
}

func TestNewWhoamiResolver_requiresURL(t *testing.T) {
	if _, err := newWhoamiResolver("", "secret"); err == nil {
		t.Error("newWhoamiResolver(\"\", ...) should error — whoami_url is required")
	}
	if _, err := newWhoamiResolver("http://x/whoami", "secret"); err != nil {
		t.Errorf("newWhoamiResolver with url should succeed, got %v", err)
	}
}

func TestWhoamiResolver_resolve(t *testing.T) {
	ctx := context.Background()

	// Success: returns entity_id AND sends the exact /whoami contract headers.
	t.Run("success_sends_contract_headers", func(t *testing.T) {
		var gotUser, gotChan, gotSecret string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotUser = r.Header.Get("X-User-ID")
			gotChan = r.Header.Get("X-Channel-Type")
			gotSecret = r.Header.Get("X-Security-Token")
			_ = json.NewEncoder(w).Encode(map[string]string{"entity_id": "user-42", "group": "g1"})
		}))
		defer srv.Close()
		r, err := newWhoamiResolver(srv.URL, "sek")
		if err != nil {
			t.Fatalf("newWhoamiResolver: %v", err)
		}
		id, err := r.resolve(ctx, "tok-abc")
		if err != nil || id != "user-42" {
			t.Fatalf("resolve = %q, %v; want \"user-42\", nil", id, err)
		}
		if gotUser != "tok-abc" || gotChan != ID || gotSecret != "sek" {
			t.Errorf("headers wrong: X-User-ID=%q X-Channel-Type=%q X-Security-Token=%q", gotUser, gotChan, gotSecret)
		}
	})

	// Fail-closed: every non-happy outcome must yield an error (caller refuses the socket).
	for _, tc := range []struct {
		name    string
		handler http.HandlerFunc
	}{
		{"status_500", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusInternalServerError) }},
		{"malformed_json", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("{not json")) }},
		{"missing_entity_id", func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]string{"group": "g1"})
		}},
	} {
		t.Run("failclosed_"+tc.name, func(t *testing.T) {
			srv := httptest.NewServer(tc.handler)
			defer srv.Close()
			r, _ := newWhoamiResolver(srv.URL, "sek")
			if id, err := r.resolve(ctx, "tok"); err == nil {
				t.Errorf("%s: expected error, got id=%q nil", tc.name, id)
			}
		})
	}

	// Secret falls back to the inherited WHOAMI_SECRET env when not given in config.
	t.Run("secret_env_fallback", func(t *testing.T) {
		t.Setenv("WHOAMI_SECRET", "from-env")
		var gotSecret string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotSecret = r.Header.Get("X-Security-Token")
			_ = json.NewEncoder(w).Encode(map[string]string{"entity_id": "u"})
		}))
		defer srv.Close()
		r, _ := newWhoamiResolver(srv.URL, "") // empty config secret → env fallback
		if _, err := r.resolve(ctx, "tok"); err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if gotSecret != "from-env" {
			t.Errorf("X-Security-Token = %q, want \"from-env\" (env fallback)", gotSecret)
		}
	})
}

func TestConnRegistry_lastSocketDeletesBucket(t *testing.T) {
	// White-box: a conversation key must not linger after its last socket leaves,
	// or the map grows unbounded across reconnects.
	ch := New(Config{})
	a, b := &wsConn{}, &wsConn{}
	if err := ch.addConn("c", "user1", a); err != nil {
		t.Fatalf("addConn a = %v", err)
	}
	if err := ch.addConn("c", "user1", b); err != nil {
		t.Fatalf("addConn b (same user) should be allowed, got %v", err)
	}
	if err := ch.addConn("c", "user2", &wsConn{}); err != errOwnerMismatch {
		t.Fatalf("addConn for a different user = %v, want errOwnerMismatch", err)
	}
	ch.removeConn("c", a)
	if len(ch.targets("c")) != 1 {
		t.Errorf("after removing 1 of 2, targets = %d, want 1", len(ch.targets("c")))
	}
	ch.removeConn("c", b)
	ch.connsMu.Lock()
	_, present := ch.conns["c"]
	ch.connsMu.Unlock()
	if present {
		t.Error("bucket should be deleted once its last socket leaves")
	}
}
