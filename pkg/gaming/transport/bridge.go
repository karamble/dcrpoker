package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/decred/slog"
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
	// BaseURL is the host's tunnel root, e.g. https://127.0.0.1:8080/gaming.
	BaseURL string

	// Token is the bearer token the host issued this game.
	//
	// It is an identity, not just a password. The host resolves it to decide
	// which game is calling, and everything it enforces - which game a frame
	// may be sent as, and later which account may be spent from and under
	// what caps - is enforced against that. So this game never states which
	// game it is; it presents the token and the host decides, which is why
	// it cannot send another game's traffic even by asking to.
	Token string

	// HTTP is the client used for requests.
	HTTP *http.Client

	// OnGap, if set, is called when the event stream reconnects.
	//
	// The host's stream is lossy on purpose - it buffers a little per game
	// and drops rather than blocking, so one game that stops draining
	// cannot stall the others - and a reconnection is where losses cluster.
	// Saying so is all the transport can honestly do; recovering what was
	// missed needs the protocol, which knows what it was expecting.
	OnGap func()

	// Log, if set, records connection trouble. Nothing here is fatal, so
	// without it a game reconnecting in a loop does so silently.
	Log slog.Logger
}

func (b *Bridge) debugf(format string, args ...interface{}) {
	if b.Log != nil {
		b.Log.Debugf(format, args...)
	}
}

// NewBridge returns a Bridge with a sane default client.
func NewBridge(baseURL, token string, client *http.Client) (*Bridge, error) {
	if strings.TrimSpace(baseURL) == "" {
		return nil, fmt.Errorf("bridge needs a base url")
	}
	if strings.TrimSpace(token) == "" {
		return nil, fmt.Errorf("bridge needs the token the host issued this game")
	}
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	return &Bridge{
		BaseURL: strings.TrimRight(baseURL, "/"),
		Token:   strings.TrimSpace(token),
		HTTP:    client,
	}, nil
}

// SendGC delivers one frame to a table's group chat, satisfying GCSender.
func (b *Bridge) SendGC(ctx context.Context, gcID, text string) error {
	body, err := json.Marshal(map[string]string{
		"gcid":  gcID,
		"frame": text,
	})
	if err != nil {
		return fmt.Errorf("encode send request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.BaseURL+"/send", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build send request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+b.Token)

	resp, err := b.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("send frame: %w", err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusNoContent:
		return nil
	case resp.StatusCode == http.StatusForbidden:
		// The host refused the frame itself: it is not one of ours.
		// Configuration, not transport, so say so rather than inviting a
		// retry.
		return fmt.Errorf("host refused the frame: %s", readError(resp))
	case resp.StatusCode == http.StatusNotFound:
		// The host does not recognise the token. A disabled gaming
		// section answers the same way as an absent one, so this covers
		// both: the game is not installed, its token was revoked, or
		// gaming is switched off.
		return fmt.Errorf("host does not recognise this game's token; check it is still installed")
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
