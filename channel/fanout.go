// Cross-pod fan-out subsystem.
//
// A reply is generated on whichever pod runs the orchestrator, which is not
// necessarily the pod holding the user's browser socket. When enabled
// (redis_url set), every outbound frame is also published to a Redis pub/sub
// channel that every pod subscribes to, so the pod that owns the socket
// delivers it. Disabled = single-pod local delivery, exactly as before.
//
// Wire shape: one fanoutEnvelope JSON object per frame —
// {origin, owner, conversation_id, frame} — where frame carries the
// already-filtered client bytes (buildFrame output) verbatim, so a remote pod
// writes byte-identical frames with no internal-metadata leak.
//
// Owner gate: a subscriber only delivers an envelope whose owner matches the
// local socket set's pinned owner; on mismatch it drops the frame, restoring
// the single-owner boundary across pods. An EMPTY owner is fail-open — it
// means the core did not resolve one (anonymous session, or a core version
// that predates owner stamping), so it is not gated.
//
// The authoritative definition of the owner-key contract (including the
// empty=fail-open semantics) is the orchestrator's
// pkg/channel.OwnerEntityMetadataKey, imported below.

package wschannel

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	pkg "github.com/opentalon/opentalon/pkg/channel"
	"github.com/redis/go-redis/v9"
)

// fanoutChannel is the Redis pub/sub channel every pod publishes outbound frames
// to and subscribes to. A frame published by the pod that generated a reply is
// picked up by the pod holding the user's socket, which delivers it.
const fanoutChannel = "opentalon:ws:fanout"

// fanoutEnvelope is the JSON published for one outbound frame. Frame carries the
// already-filtered client bytes (buildFrame output) verbatim, so a subscriber
// writes the exact same bytes a local Send would — no profile_token or internal
// (_-prefixed) metadata can leak to the browser through this path. Owner is the
// resolved owner entity; a subscriber drops the frame on owner mismatch so the
// single-owner boundary holds cross-pod.
type fanoutEnvelope struct {
	Origin         string          `json:"origin"`
	Owner          string          `json:"owner"`
	ConversationID string          `json:"conversation_id"`
	Frame          json.RawMessage `json:"frame"`
}

// startFanout wires up cross-pod delivery when enabled (RedisURL set, or a fake
// fanout injected by a test). It mints the process-unique origin, opens the
// subscription, and launches the subscribe goroutine tracked in c.wg. On any
// setup error it leaves fan-out fully disabled (origin/fanout/sub cleared) so
// the caller degrades to local-only rather than failing. No-op when disabled.
func (c *Channel) startFanout(ctx context.Context) error {
	if c.fanout == nil {
		if c.cfg.RedisURL == "" {
			return nil // fan-out disabled: local-only, exactly as before
		}
		f, err := newRedisFanout(c.cfg.RedisURL)
		if err != nil {
			return err
		}
		c.fanout = f
	}
	// A per-process-unique origin is required: a non-unique id (e.g. a bare
	// hostname reused after a restart) would make a pod skip a frame it did not
	// actually publish, silently dropping the delivery.
	c.origin = newID()
	sub, err := c.fanout.Subscribe(ctx)
	if err != nil {
		_ = c.fanout.Close()
		c.fanout = nil
		c.origin = ""
		return err
	}
	c.sub = sub
	c.wg.Add(1)
	go c.subscribeLoop()
	slog.Info("websocket channel: cross-pod fan-out enabled", "origin", c.origin)
	return nil
}

// subscribeLoop drains published frames and delivers those addressed to a
// conversation with a live socket on this pod. It ranges the subscription
// channel, which the go-redis pub/sub self-heals across transient drops and
// closes only on a permanent Close (Stop), which ends this loop.
func (c *Channel) subscribeLoop() {
	defer c.wg.Done()
	for payload := range c.sub.Messages() {
		var env fanoutEnvelope
		if err := json.Unmarshal(payload, &env); err != nil {
			slog.Warn("websocket fan-out: bad envelope", "error", err, "payload_bytes", len(payload))
			continue
		}
		// Our own publish — already delivered locally before publishing.
		if env.Origin == c.origin {
			continue
		}
		// Only deliver if a live socket set for this conversation exists on this
		// pod AND its owner matches the envelope's owner. On mismatch, drop: that
		// restores the single-owner boundary across pods (the same invariant the
		// local fan-out relies on). An empty owner means the core did not resolve
		// one (anonymous session), so it is not gated.
		owner, ok := c.conversationOwner(env.ConversationID)
		if !ok {
			continue
		}
		if env.Owner != "" && owner != env.Owner {
			slog.Warn("websocket fan-out: owner mismatch, dropping",
				"conversation_id", env.ConversationID, "origin", env.Origin,
				"want_owner", owner, "got_owner", env.Owner)
			continue
		}
		c.deliverLocal(context.Background(), env.ConversationID, env.Frame)
	}
}

// publish relays the frame to other pods over Redis pub/sub. No-op when fan-out
// is disabled (local-only). Best-effort and non-blocking: it is bounded by
// writeTimeout on a context detached from the caller's, and any error is logged
// and swallowed — local delivery already happened, so failure just degrades to
// local-only for this frame.
func (c *Channel) publish(msg pkg.OutboundMessage, frame []byte) {
	if c.fanout == nil {
		return
	}
	env := fanoutEnvelope{
		Origin:         c.origin,
		Owner:          msg.Metadata[pkg.OwnerEntityMetadataKey],
		ConversationID: msg.ConversationID,
		Frame:          json.RawMessage(frame),
	}
	payload, err := json.Marshal(env)
	if err != nil {
		slog.Warn("websocket fan-out: marshal envelope failed", "conversation_id", msg.ConversationID, "error", err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), writeTimeout)
	defer cancel()
	if err := c.fanout.Publish(ctx, payload); err != nil {
		slog.Warn("websocket fan-out: publish failed", "conversation_id", msg.ConversationID, "error", err)
	}
}

// ── cross-pod fan-out seam ───────────────────────────────────────────────────

// fanoutBus is the minimal pub/sub seam the channel needs for cross-pod
// delivery. The production impl (redisFanout) talks to a real Redis; tests wire
// an in-memory fake so the fan-out path is exercised without an external server.
type fanoutBus interface {
	// Publish sends payload to every subscriber of the fan-out channel.
	Publish(ctx context.Context, payload []byte) error
	// Subscribe opens a subscription. The returned fanoutSub yields published
	// payloads until it is closed.
	Subscribe(ctx context.Context) (fanoutSub, error)
	// Close releases the underlying client.
	Close() error
}

// fanoutSub is one live subscription. Messages() closes when the subscription is
// closed, which ends the ranging subscribe goroutine.
type fanoutSub interface {
	Messages() <-chan []byte
	Close() error
}

// redisFanout is the production fanoutBus, backed by go-redis pub/sub. This
// channel is its own subprocess, so it owns this client outright.
type redisFanout struct {
	client redis.UniversalClient
}

// newRedisFanout parses redisURL (a redis:// or rediss:// URL) and builds a
// client. The client connects lazily, so this does not block on Redis being up.
func newRedisFanout(redisURL string) (*redisFanout, error) {
	opts, err := redis.ParseURL(redisURL)
	if err != nil {
		return nil, fmt.Errorf("websocket fan-out: parsing redis_url: %w", err)
	}
	return &redisFanout{client: redis.NewClient(opts)}, nil
}

func (r *redisFanout) Publish(ctx context.Context, payload []byte) error {
	return r.client.Publish(ctx, fanoutChannel, payload).Err()
}

func (r *redisFanout) Subscribe(ctx context.Context) (fanoutSub, error) {
	ps := r.client.Subscribe(ctx, fanoutChannel)
	// Wait for the subscribe to be confirmed so a connect failure surfaces here
	// (and fan-out is disabled) rather than silently never delivering.
	if _, err := ps.Receive(ctx); err != nil {
		_ = ps.Close()
		return nil, fmt.Errorf("websocket fan-out: subscribe: %w", err)
	}
	return newRedisSub(ps), nil
}

func (r *redisFanout) Close() error { return r.client.Close() }

// redisSub adapts go-redis's <-chan *redis.Message to the seam's <-chan []byte.
// go-redis's Channel() self-heals transient reconnects; it closes only when the
// PubSub is Closed, which ends the translator and closes out.
type redisSub struct {
	ps  *redis.PubSub
	out chan []byte
}

func newRedisSub(ps *redis.PubSub) *redisSub {
	s := &redisSub{ps: ps, out: make(chan []byte, 64)}
	go func() {
		defer close(s.out)
		for m := range s.ps.Channel() {
			s.out <- []byte(m.Payload)
		}
	}()
	return s
}

func (s *redisSub) Messages() <-chan []byte { return s.out }
func (s *redisSub) Close() error            { return s.ps.Close() }
