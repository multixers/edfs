package core

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

// Config is what the server tells a driver at startup. edfs is started with one
// base URL and nothing else; where the change feed lives, which channel to join
// and how to authorize are all answered here, so any of it can move without
// touching or redeploying the driver.
type Config struct {
	// Origin identifies this mount's grant. Changes carrying it were caused by
	// this very driver, so they're skipped rather than re-fetched.
	Origin  int           `json:"origin"`
	Changes ChangesConfig `json:"changes"`
}

type ChangesConfig struct {
	Enabled      bool   `json:"enabled"`
	URL          string `json:"url"`
	Key          string `json:"key"`
	Channel      string `json:"channel"`
	Event        string `json:"event"`
	AuthEndpoint string `json:"auth_endpoint"`
}

// Change is a mutation to the user's Storage made somewhere else — the web UI,
// an agent, or another machine's mount.
type Change struct {
	Path   string `json:"path"`
	Op     string `json:"op"` // created|updated|deleted|renamed
	From   string `json:"from"`
	Origin int    `json:"origin"`
}

func (c *Client) Config() (*Config, error) {
	resp, err := c.do("GET", "/config", nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("config: HTTP %d: %s", resp.StatusCode, body)
	}

	var cfg Config
	if err := json.NewDecoder(resp.Body).Decode(&cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// Invalidate drops cached metadata for a path so the next lookup goes to the
// server: the entry itself, its listing if it was a directory, and its parent's
// listing, whose contents just changed.
func (c *Client) Invalidate(filePath string) {
	if filePath == "" {
		return
	}
	c.statCache.delete(filePath)
	c.listCache.delete(filePath)
	c.listCache.delete(parent(filePath))
}

// WatchChanges follows the server's change feed for as long as ctx lives,
// invalidating cached metadata for every path someone else touched and calling
// onChange so a frontend can also drop the kernel's copy. Without it a change
// made elsewhere stays invisible until the cache TTL expires; with it the mount
// learns immediately, and it costs one idle socket rather than polling.
//
// Reconnects with backoff, and does nothing but retry slowly if the server says
// broadcasting is off — a driver shouldn't fail to mount over a missing feed.
func (c *Client) WatchChanges(ctx context.Context, onChange func(Change)) {
	backoff := time.Second
	const maxBackoff = 2 * time.Minute

	for ctx.Err() == nil {
		cfg, err := c.Config()
		switch {
		case err != nil:
			log.Printf("changes: config: %v", err)
		case !cfg.Changes.Enabled:
			// Broadcasting isn't configured server-side; check again later.
			sleep(ctx, maxBackoff)
			continue
		default:
			if err := c.streamChanges(ctx, cfg, onChange); err != nil && ctx.Err() == nil {
				log.Printf("changes: %v", err)
			}
			backoff = time.Second // a successful session resets the backoff
		}

		if !sleep(ctx, backoff) {
			return
		}
		if backoff *= 2; backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

// streamChanges runs one websocket session: handshake, authorize, subscribe,
// then dispatch until the connection drops.
func (c *Client) streamChanges(ctx context.Context, cfg *Config, onChange func(Change)) error {
	endpoint := cfg.Changes.URL + "?protocol=7&client=edfs&version=1"

	conn, _, err := websocket.DefaultDialer.DialContext(ctx, endpoint, nil)
	if err != nil {
		return fmt.Errorf("dial %s: %w", cfg.Changes.URL, err)
	}
	defer conn.Close()

	go func() { // let the read loop below unblock when the caller is done
		<-ctx.Done()
		conn.Close()
	}()

	socketID, err := awaitSocketID(conn)
	if err != nil {
		return err
	}

	auth, err := c.authorizeChannel(cfg, socketID)
	if err != nil {
		return fmt.Errorf("authorize %s: %w", cfg.Changes.Channel, err)
	}

	if err := conn.WriteJSON(map[string]any{
		"event": "pusher:subscribe",
		"data":  map[string]string{"channel": cfg.Changes.Channel, "auth": auth},
	}); err != nil {
		return err
	}

	for {
		var msg pusherMessage
		if err := conn.ReadJSON(&msg); err != nil {
			return err
		}

		switch msg.Event {
		case "pusher:ping":
			_ = conn.WriteJSON(map[string]any{"event": "pusher:pong"})
		case "pusher:error":
			return fmt.Errorf("server: %s", msg.Data)

		// Only now is the subscription real. Announcing it when the request was
		// merely *sent* would report a working feed even when the server rejected
		// it, which is the sort of thing that turns into an hour of confusion.
		case "pusher_internal:subscription_succeeded":
			log.Printf("changes: watching %s", cfg.Changes.Channel)
		case "pusher_internal:subscription_error", "pusher:subscription_error":
			return fmt.Errorf("subscribe %s rejected: %s", cfg.Changes.Channel, msg.Data)

		case cfg.Changes.Event:
			var ch Change
			if err := decodePusherData(msg.Data, &ch); err != nil {
				log.Printf("changes: bad payload: %v", err)
				continue
			}
			if ch.Origin != 0 && ch.Origin == cfg.Origin {
				continue // our own write coming back to us
			}
			log.Printf("changes: %s %s", ch.Op, ch.Path)
			c.Invalidate(ch.Path)
			c.Invalidate(ch.From)
			if onChange != nil {
				onChange(ch)
			}

		default:
			// The feed is low volume, so surfacing anything unrecognised is cheap
			// and saves guessing when the server and driver disagree.
			log.Printf("changes: unhandled %q on %q: %s", msg.Event, msg.Channel, msg.Data)
		}
	}
}

type pusherMessage struct {
	Event   string          `json:"event"`
	Channel string          `json:"channel"`
	Data    json.RawMessage `json:"data"`
}

func awaitSocketID(conn *websocket.Conn) (string, error) {
	_ = conn.SetReadDeadline(time.Now().Add(15 * time.Second))
	defer conn.SetReadDeadline(time.Time{})

	for {
		var msg pusherMessage
		if err := conn.ReadJSON(&msg); err != nil {
			return "", err
		}
		if msg.Event != "pusher:connection_established" {
			continue
		}
		var payload struct {
			SocketID string `json:"socket_id"`
		}
		if err := decodePusherData(msg.Data, &payload); err != nil {
			return "", err
		}
		return payload.SocketID, nil
	}
}

// authorizeChannel trades the driver's own bearer token for a channel signature.
// It posts to the fuse-guarded endpoint, not the web app's /broadcasting/auth,
// which only knows how to authenticate a session.
func (c *Client) authorizeChannel(cfg *Config, socketID string) (string, error) {
	form := url.Values{
		"socket_id":    {socketID},
		"channel_name": {cfg.Changes.Channel},
	}

	req, err := http.NewRequest("POST", cfg.Changes.AuthEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, body)
	}

	var out struct {
		Auth string `json:"auth"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	if out.Auth == "" {
		return "", fmt.Errorf("no auth signature returned")
	}
	return out.Auth, nil
}

// decodePusherData unwraps a Pusher payload, which carries its JSON as a quoted
// string rather than a nested object.
func decodePusherData(raw json.RawMessage, v any) error {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return json.Unmarshal([]byte(s), v)
	}
	return json.Unmarshal(raw, v)
}

// sleep waits, reporting false if the context ended first.
func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
