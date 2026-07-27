package transport

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// eventsHost is a stand-in for dcrpulse's /gaming/events route: it upgrades,
// records the Authorization header it was given, and sends whatever the test
// tells it to.
type eventsHost struct {
	mu    sync.Mutex
	auth  []string
	conns int32

	// serve runs for one connection. Returning closes it, which is how a
	// test makes the stream drop.
	serve func(t *testing.T, conn *websocket.Conn)
}

func (h *eventsHost) handler(t *testing.T) http.HandlerFunc {
	up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	return func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		h.auth = append(h.auth, r.Header.Get("Authorization"))
		h.mu.Unlock()
		atomic.AddInt32(&h.conns, 1)

		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		h.serve(t, conn)
	}
}

func (h *eventsHost) headers() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.auth...)
}

// newTestBridge points a Bridge at a test host, mirroring how the portal hands
// a game its tunnel root without a trailing slash.
func newTestBridge(t *testing.T, srv *httptest.Server) *Bridge {
	t.Helper()
	b, err := NewBridge(srv.URL+"/gaming", "tok-abc", nil)
	if err != nil {
		t.Fatalf("new bridge: %v", err)
	}
	return b
}

func TestEventsCarriesFramesAndPresentsTheToken(t *testing.T) {
	host := &eventsHost{serve: func(t *testing.T, conn *websocket.Conn) {
		_ = conn.WriteJSON(InboundFrame{Game: "poker", GCID: "aa", From: "bob", Frame: "--gaming[..]--QUJD"})
		// Hold the connection so the reader is not racing a close.
		time.Sleep(200 * time.Millisecond)
	}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/gaming/events" {
			t.Errorf("host was asked for %q, want /gaming/events", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		host.handler(t)(w, r)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	frames, err := newTestBridge(t, srv).Events(ctx)
	if err != nil {
		t.Fatalf("events: %v", err)
	}

	select {
	case f := <-frames:
		if f.Game != "poker" || f.GCID != "aa" || f.From != "bob" {
			t.Fatalf("frame arrived mangled: %+v", f)
		}
	case <-ctx.Done():
		t.Fatal("no frame arrived")
	}

	if got := host.headers(); len(got) == 0 || got[0] != "Bearer tok-abc" {
		t.Fatalf("host saw Authorization %q, want the bearer token", got)
	}
}

// A dropped connection is not the end of the stream. The host restarting is
// ordinary, and a game that stopped listening would look exactly like a player
// who walked away from a funded table.
func TestEventsReconnectsAndSaysSoWhenItDoes(t *testing.T) {
	var round int32
	host := &eventsHost{serve: func(t *testing.T, conn *websocket.Conn) {
		n := atomic.AddInt32(&round, 1)
		if n == 1 {
			// Drop immediately, without sending anything.
			return
		}
		_ = conn.WriteJSON(InboundFrame{Game: "poker", GCID: "aa", From: "bob", Frame: "second"})
		time.Sleep(200 * time.Millisecond)
	}}
	srv := httptest.NewServer(host.handler(t))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	b := newTestBridge(t, srv)
	gaps := make(chan struct{}, 8)
	b.OnGap = func() {
		select {
		case gaps <- struct{}{}:
		default:
		}
	}

	frames, err := b.Events(ctx)
	if err != nil {
		t.Fatalf("events: %v", err)
	}

	select {
	case f := <-frames:
		if f.Frame != "second" {
			t.Fatalf("got frame %q, want the one sent after reconnecting", f.Frame)
		}
	case <-ctx.Done():
		t.Fatal("stream did not recover from a dropped connection")
	}

	// The gap must be reported, because that is where losses cluster and
	// only the protocol above can recover them.
	select {
	case <-gaps:
	default:
		t.Fatal("reconnected without reporting a gap")
	}
}

// An unrecognised token is answered with 404, the same as a gaming section that
// is switched off. Both mean "not now", so it must retry rather than give up -
// the sandbox offers no other route to anything.
func TestEventsKeepsTryingWhenTheHostRefusesTheToken(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		http.NotFound(w, r)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	frames, err := newTestBridge(t, srv).Events(ctx)
	if err != nil {
		t.Fatalf("events: %v", err)
	}

	// Channel closes only when the context is done, never on refusal.
	select {
	case _, ok := <-frames:
		if ok {
			t.Fatal("a refusing host somehow produced a frame")
		}
	case <-time.After(4 * time.Second):
		t.Fatal("stream never closed after the context expired")
	}

	if n := atomic.LoadInt32(&attempts); n < 2 {
		t.Fatalf("gave up after %d attempt(s); it must keep retrying", n)
	}
}

func TestEventsStopsWhenTheCallerDoes(t *testing.T) {
	host := &eventsHost{serve: func(t *testing.T, conn *websocket.Conn) {
		time.Sleep(2 * time.Second)
	}}
	srv := httptest.NewServer(host.handler(t))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	frames, err := newTestBridge(t, srv).Events(ctx)
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	cancel()

	select {
	case _, ok := <-frames:
		if ok {
			t.Fatal("frame delivered after the caller gave up")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("channel stayed open after the context was cancelled")
	}
}

func TestTheEventsEndpointIsTheTunnelRootAsAWebsocket(t *testing.T) {
	for _, tc := range []struct {
		base string
		want string
	}{
		{"http://dashboard:8080/gaming", "ws://dashboard:8080/gaming/events"},
		{"https://dashboard:8080/gaming", "wss://dashboard:8080/gaming/events"},
		{"ws://dashboard:8080/gaming", "ws://dashboard:8080/gaming/events"},
		{"wss://dashboard:8080/gaming", "wss://dashboard:8080/gaming/events"},
	} {
		b, err := NewBridge(tc.base, "tok", nil)
		if err != nil {
			t.Fatalf("new bridge %q: %v", tc.base, err)
		}
		got, err := b.eventsURL()
		if err != nil {
			t.Fatalf("events url %q: %v", tc.base, err)
		}
		if got != tc.want {
			t.Fatalf("got %q, want %q", got, tc.want)
		}
	}

	b, err := NewBridge("ftp://dashboard/gaming", "tok", nil)
	if err != nil {
		t.Fatalf("new bridge: %v", err)
	}
	if _, err := b.eventsURL(); err == nil {
		t.Fatal("a non-http tunnel root should be refused")
	}
}
