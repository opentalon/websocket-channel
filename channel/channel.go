package wschannel

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	pkg "github.com/opentalon/opentalon/pkg/channel"
)

// ID is the channel identifier for the WebSocket channel.
const ID = "websocket"

// Config holds the WebSocket server configuration.
type Config struct {
	Addr        string   // listening address, e.g. "0.0.0.0:9000"
	Path        string   // WebSocket path, e.g. "/ws"
	CORSOrigins []string // allowed origins; empty = allow all (dev mode)
}

type wsConn struct {
	ws *websocket.Conn
	mu sync.Mutex
}

// Channel is a WebSocket server channel. Browser clients connect to it with a
// profile token and exchange JSON text frames with the OpenTalon core.
type Channel struct {
	cfg     Config
	conns   sync.Map // conversationID → *wsConn
	inbox   chan<- pkg.InboundMessage
	srv     *http.Server
	stopMu  sync.Mutex
	stopped bool
	wg      sync.WaitGroup
}

// inboundFrame is the JSON structure for client → server messages.
type inboundFrame struct {
	Content string      `json:"content"`
	Files   []fileFrame `json:"files,omitempty"`
}

type fileFrame struct {
	Name     string `json:"name"`
	MimeType string `json:"mime_type"`
	Data     string `json:"data"` // base64-encoded
}

// outboundFrame is the JSON structure for server → client messages.
type outboundFrame struct {
	ConversationID string `json:"conversation_id"`
	Content        string `json:"content"`
	Streaming      bool   `json:"streaming,omitempty"` // true while LLM is still generating; false (or absent) = final message
	Done           bool   `json:"done,omitempty"`      // true on the last streaming frame
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
	return &Channel{cfg: cfg}
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
	return nil
}

// ID implements pkg.Channel.
func (c *Channel) ID() string { return ID }

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
		ResponseFormat:   pkg.FormatHTML,
	}
}

// Start implements pkg.Channel. It starts the HTTP/WebSocket server and returns
// immediately; the server runs in a background goroutine.
func (c *Channel) Start(ctx context.Context, inbox chan<- pkg.InboundMessage) error {
	c.inbox = inbox

	mux := http.NewServeMux()
	mux.HandleFunc(c.cfg.Path, c.handleUpgrade)

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
	v, ok := c.conns.Load(msg.ConversationID)
	if !ok {
		return nil // client already disconnected
	}
	cn := v.(*wsConn)
	frame := outboundFrame{
		ConversationID: msg.ConversationID,
		Content:        msg.Content,
	}
	data, err := json.Marshal(frame)
	if err != nil {
		return fmt.Errorf("marshal response: %w", err)
	}
	cn.mu.Lock()
	defer cn.mu.Unlock()
	return cn.ws.Write(ctx, websocket.MessageText, data)
}

// SendAndCapture implements pkg.UpdatableChannel. Returns an error so
// the StreamWriter never sets flushed=true. This makes registry.go
// fall through to ch.Send() with the clean final response — the user
// only sees one frame with the correct answer, no intermediate flicker.
func (c *Channel) SendAndCapture(_ context.Context, _ pkg.OutboundMessage) (string, error) {
	return "", fmt.Errorf("websocket: streaming suppressed")
}

// SendUpdate implements pkg.UpdatableChannel. No-op because SendAndCapture
// returns an error, so the StreamWriter never has a messageID to update.
func (c *Channel) SendUpdate(_ context.Context, _ string, _ pkg.OutboundMessage) error {
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

// handleUpgrade upgrades an HTTP request to a WebSocket connection.
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

	convID := newID()
	cn := &wsConn{ws: ws}
	c.conns.Store(convID, cn)

	c.stopMu.Lock()
	if c.stopped {
		c.stopMu.Unlock()
		c.conns.Delete(convID)
		_ = ws.CloseNow()
		return
	}
	c.wg.Add(1)
	c.stopMu.Unlock()
	defer func() {
		c.wg.Done()
		c.conns.Delete(convID)
		_ = ws.CloseNow()
	}()

	c.readLoop(r.Context(), ws, convID, token)
}

func (c *Channel) readLoop(ctx context.Context, ws *websocket.Conn, convID, token string) {
	for {
		_, data, err := ws.Read(ctx)
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

		msg := pkg.InboundMessage{
			ChannelID:      ID,
			ConversationID: convID,
			SenderID:       convID,
			Content:        frame.Content,
			Metadata:       map[string]string{"profile_token": token},
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
