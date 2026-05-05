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
	if caps.ResponseFormat != pkg.FormatHTML {
		t.Errorf("Capabilities().ResponseFormat = %q, want %q", caps.ResponseFormat, pkg.FormatHTML)
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

// testServer starts the channel's HTTP handler on an httptest.Server and
// returns the channel, inbox (readable end), server, and a cleanup function.
func testServer(t *testing.T) (*Channel, <-chan pkg.InboundMessage, *httptest.Server, func()) {
	t.Helper()
	ch := New(Config{Path: "/ws"})
	inbox := make(chan pkg.InboundMessage, 16)
	ch.inbox = inbox

	mux := http.NewServeMux()
	mux.HandleFunc("/ws", ch.handleUpgrade)
	srv := httptest.NewServer(mux)

	return ch, inbox, srv, func() {
		srv.Close()
		_ = ch.Stop()
	}
}

func wsURL(srv *httptest.Server, token string) string {
	u := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws"
	if token != "" {
		u += "?token=" + token
	}
	return u
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
