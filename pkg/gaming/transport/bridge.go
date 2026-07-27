package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Bridge sends frames through a host that holds the Bison Relay credentials.
//
// The game never holds them itself. It runs beside a wallet, a dcrlnd node and
// a Bison Relay identity, and it plays for real money against strangers - so
// the host does the talking, and a game that was compromised gains a tunnel
// rather than an identity.
//
// The host carries frames whole and reads only the routing key. That division
// is what keeps this honest: players sign their own actions and check each
// other's, so a host that understood the payloads would be a party to the game
// nobody agreed to trust.
type Bridge struct {
	// BaseURL is the host's API root, e.g. https://127.0.0.1:8080/api.
	BaseURL string

	// Game is the routing key this game's frames carry.
	Game string

	// HTTP is the client used for requests. It carries whatever credential
	// the host requires.
	HTTP *http.Client
}

// NewBridge returns a Bridge with a sane default client.
func NewBridge(baseURL, game string, client *http.Client) (*Bridge, error) {
	if strings.TrimSpace(baseURL) == "" {
		return nil, fmt.Errorf("bridge needs a base url")
	}
	if strings.TrimSpace(game) == "" {
		return nil, fmt.Errorf("bridge needs a game")
	}
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	return &Bridge{
		BaseURL: strings.TrimRight(baseURL, "/"),
		Game:    game,
		HTTP:    client,
	}, nil
}

// SendGC delivers one frame to a table's group chat, satisfying GCSender.
func (b *Bridge) SendGC(ctx context.Context, gcID, text string) error {
	body, err := json.Marshal(map[string]string{
		"game":  b.Game,
		"gcid":  gcID,
		"frame": text,
	})
	if err != nil {
		return fmt.Errorf("encode send request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.BaseURL+"/br/gaming/send", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build send request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := b.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("send frame: %w", err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusNoContent:
		return nil
	case resp.StatusCode == http.StatusForbidden:
		// The host refused: this game is not installed, or the frame is
		// not one of ours. Both are configuration, not transport, so say
		// so plainly rather than inviting a retry.
		return fmt.Errorf("host refused the frame: %s", readError(resp))
	default:
		return fmt.Errorf("send frame: host returned %s: %s", resp.Status, readError(resp))
	}
}

func readError(resp *http.Response) string {
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(http.MaxBytesReader(nil, resp.Body, 4<<10))
	msg := strings.TrimSpace(buf.String())
	if msg == "" {
		return "(no detail)"
	}
	return msg
}

// InboundFrame is one frame the host delivered.
type InboundFrame struct {
	Game  string `json:"game"`
	GCID  string `json:"gcid"`
	From  string `json:"from"`
	Frame string `json:"frame"`
}

// Receive feeds frames from the host into a router until ctx is cancelled.
//
// It takes a stream of already-decoded frames rather than opening the
// connection itself, so the wire protocol between game and host - today a
// websocket, tomorrow whatever the plugin model settles on - stays out of the
// routing logic and out of its tests.
func Receive(ctx context.Context, frames <-chan InboundFrame, r *Router) {
	for {
		select {
		case <-ctx.Done():
			return
		case f, ok := <-frames:
			if !ok {
				return
			}
			// The host has already decided this frame is ours, but the
			// router checks again: it is the thing that knows which
			// game it serves, and a host that got it wrong should not
			// be able to inject another game's traffic.
			r.HandleGCMessage(f.GCID, f.From, f.Frame, time.Now())
		}
	}
}
