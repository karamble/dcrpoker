package transport

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/gorilla/websocket"
)

const (
	// eventsReadTimeout is how long a stream may be silent before it is
	// treated as dead. The host pings every thirty seconds, so this is
	// generously past two missed pings: a live connection that has simply
	// carried no frames must never be torn down, because a quiet table is
	// the normal case and reconnecting loses whatever was in flight.
	eventsReadTimeout = 90 * time.Second

	// eventsMinBackoff and eventsMaxBackoff bound reconnection.
	//
	// A game reaches the host and nothing else - the sandbox it runs in has
	// no other route - so giving up is not an option that leaves anything
	// working. It retries forever and backs off instead, which also covers
	// the case the host is simply not up yet.
	eventsMinBackoff = time.Second
	eventsMaxBackoff = 30 * time.Second
)

// Events opens the host's frame stream and returns the frames it carries.
//
// The returned channel is closed when ctx is done, and not before: a dropped
// connection is reconnected rather than reported as the end of the stream,
// because the host restarting is ordinary and a game that stopped listening
// would look exactly like a player who walked away from a funded table.
//
// What it cannot do is hide the gap. The host's stream is deliberately lossy -
// it buffers a little per game and drops rather than blocking, so one wedged
// game cannot stall the others - and a reconnection is where losses cluster.
// OnGap is how that becomes visible; recovering the missed frames is the
// protocol's job, not the transport's.
func (b *Bridge) Events(ctx context.Context) (<-chan InboundFrame, error) {
	endpoint, err := b.eventsURL()
	if err != nil {
		return nil, err
	}

	out := make(chan InboundFrame)
	go func() {
		defer close(out)
		b.streamForever(ctx, endpoint, out)
	}()
	return out, nil
}

// eventsURL rewrites the tunnel root as a websocket endpoint.
func (b *Bridge) eventsURL() (string, error) {
	u, err := url.Parse(b.BaseURL + "/events")
	if err != nil {
		return "", fmt.Errorf("bridge url: %w", err)
	}
	switch u.Scheme {
	case "http", "ws":
		u.Scheme = "ws"
	case "https", "wss":
		u.Scheme = "wss"
	default:
		return "", fmt.Errorf("bridge url scheme %q is not http or https", u.Scheme)
	}
	return u.String(), nil
}

// streamForever dials, reads, and dials again until ctx is done.
func (b *Bridge) streamForever(ctx context.Context, endpoint string, out chan<- InboundFrame) {
	backoff := eventsMinBackoff
	connected := false

	for ctx.Err() == nil {
		if connected {
			// Only report a gap on reconnection, not on the first
			// connection: there is nothing to have missed before the
			// game was listening at all.
			b.debugf("gaming stream dropped; reconnecting")
			if b.OnGap != nil {
				b.OnGap()
			}
		}

		err := b.streamOnce(ctx, endpoint, out, func() {
			connected = true
			backoff = eventsMinBackoff
		})
		switch {
		case ctx.Err() != nil:
			return
		case err != nil:
			b.debugf("gaming stream: %v", err)
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff *= 2; backoff > eventsMaxBackoff {
			backoff = eventsMaxBackoff
		}
	}
}

// streamOnce holds one connection until it fails or ctx is done. onUp is called
// once the handshake succeeded, which is what tells the caller the endpoint is
// reachable and the token still resolves.
func (b *Bridge) streamOnce(ctx context.Context, endpoint string, out chan<- InboundFrame, onUp func()) error {
	dialer := websocket.Dialer{
		HandshakeTimeout: 15 * time.Second,
	}
	conn, resp, err := dialer.DialContext(ctx, endpoint, http.Header{
		"Authorization": []string{"Bearer " + b.Token},
	})
	if err != nil {
		if errors.Is(err, websocket.ErrBadHandshake) && resp != nil {
			// The host answers an unrecognised token with 404, the
			// same way it answers a gaming section that is switched
			// off. Both mean "not now" rather than "never", so this
			// is worth saying clearly and then retrying.
			if resp.StatusCode == http.StatusNotFound {
				return fmt.Errorf("host does not recognise this game's token; check it is still installed")
			}
			return fmt.Errorf("open stream: host returned %s", resp.Status)
		}
		return fmt.Errorf("open stream: %w", err)
	}
	defer conn.Close()
	onUp()

	// Close the connection when the caller gives up, so a blocked read does
	// not outlive the context.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-done:
		}
	}()

	_ = conn.SetReadDeadline(time.Now().Add(eventsReadTimeout))
	conn.SetPingHandler(func(appData string) error {
		// A ping is proof the host is alive even when no frames are
		// flowing, so it renews the deadline as well as being answered.
		_ = conn.SetReadDeadline(time.Now().Add(eventsReadTimeout))
		return conn.WriteControl(websocket.PongMessage, []byte(appData), time.Now().Add(10*time.Second))
	})

	for {
		var f InboundFrame
		if err := conn.ReadJSON(&f); err != nil {
			return fmt.Errorf("read frame: %w", err)
		}
		_ = conn.SetReadDeadline(time.Now().Add(eventsReadTimeout))

		select {
		case out <- f:
		case <-ctx.Done():
			return nil
		}
	}
}
