// Command pokerplugin is poker as an installable game.
//
// It runs inside the gaming sandbox, supervised by a portal that fetched and
// verified this binary before executing it. It holds no Bison Relay or wallet
// credentials: it reaches the host's bridge and nothing else, because the
// sandbox it runs in has no route anywhere else. What it is given is a bearer
// token, which is its identity rather than a password - the host resolves it to
// decide which game is calling, so this process never states which game it is.
//
// The command surface is the one pokerui/golib already had for the Flutter
// client, served over HTTP instead of FFI. One entry point, a typed command, a
// JSON payload: the shape was already right, so the host drives the same
// vocabulary rather than a second one written to drift from it.
package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/vctt94/pokerbisonrelay/pkg/gaming/schema"
	"github.com/vctt94/pokerbisonrelay/pkg/gaming/transport"
	"github.com/vctt94/pokerbisonrelay/pokerui/golib"
)

func main() {
	var (
		bridge = flag.String("bridge", "", "the host's gaming tunnel, e.g. http://dashboard:8080/gaming")
		token  = flag.String("token", "", "this game's bearer token, issued by the host")
		// The host reaches this over the sandbox's internal network, so
		// it cannot be loopback. That network has no route anywhere but
		// the host, and every route here needs the same bearer token the
		// host issued, so the exposure is to the host and to whatever
		// else the sandbox is running.
		listen  = flag.String("listen", ":8790", "address the host drives this game on")
		dataDir = flag.String("datadir", "/data/poker", "where this game keeps its identity")
	)
	flag.Parse()

	if *bridge == "" || *token == "" {
		// Without both there is no way to reach the table and no identity
		// to reach it as. Refusing now beats starting something that can
		// never play.
		log.Fatalf("pokerplugin: --bridge and --token are both required")
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	id, err := loadIdentity(*dataDir)
	if err != nil {
		// Without a seed there are no session keys, and without those
		// there is no way to hold a seat. Starting anyway would mean
		// joining tables this process could never sign for.
		log.Fatalf("pokerplugin: %v", err)
	}

	p, err := newPlugin(ctx, *bridge, *token, id)
	if err != nil {
		log.Fatalf("pokerplugin: %v", err)
	}

	// Open the host's frame stream. This is the only way anything reaches
	// this process from another player: the sandbox has no route anywhere
	// but the host, so a game that cannot open this can never play.
	frames, err := p.bridge.Events(ctx)
	if err != nil {
		log.Fatalf("pokerplugin: %v", err)
	}
	go transport.Receive(ctx, frames, p.router)

	srv := &http.Server{
		Addr:              *listen,
		Handler:           p.routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()

	log.Printf("pokerplugin: serving on %s, tunnelling through %s", *listen, *bridge)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("pokerplugin: %v", err)
	}
	log.Printf("pokerplugin: stopped")
	os.Exit(0)
}

type plugin struct {
	ctx    context.Context
	bridge *transport.Bridge
	router *transport.Router
	tables *tables
	id     *identity
	token  string
}

func newPlugin(ctx context.Context, bridgeURL, token string, id *identity) (*plugin, error) {
	b, err := transport.NewBridge(bridgeURL, token, nil)
	if err != nil {
		return nil, err
	}

	// The host's stream drops frames rather than blocking, so one game that
	// stops draining cannot stall the others. Reconnecting is where that
	// loss clusters, and a player who missed frames is indistinguishable
	// from one who walked away - so it is said out loud rather than left to
	// be inferred from a table that stopped making sense.
	b.OnGap = func() {
		log.Printf("pokerplugin: reconnected to the host; frames may have been missed")
	}

	p := &plugin{ctx: ctx, bridge: b, tables: newTables(), id: id, token: token}
	p.router, err = transport.NewRouter(transport.Config{
		Game:    schema.Game,
		GameVer: schema.Version,
		Sender:  b,
		// Only sessions this process was told to join. Gaming frames are
		// invisible to the user by design, so admitting anyone would be
		// a silent way to fill the reassembly buffers with fragments for
		// a table nobody is playing.
		Authorize: p.tables.authorized,
		Handle:    p.deliver,
	})
	if err != nil {
		return nil, err
	}
	return p, nil
}

// deliver routes one decoded message to the table it belongs to, and sends
// whatever that produced.
//
// Publishing happens here rather than inside the registry because the registry
// holds a lock, and a slow send under it would stall every other table.
func (p *plugin) deliver(d transport.Delivery) {
	p.publish(p.ctx, p.tables.deliver(d))
}

func (p *plugin) routes() http.Handler {
	mux := http.NewServeMux()

	// The portal reads this to decide whether the game is ready, which is
	// what turns Ready true in the host's UI. It answers only once the
	// process is actually serving, so "ready" means reachable rather than
	// merely started.
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"game":            schema.Game,
			"protocolVersion": schema.Version,
			"ok":              true,
		})
	})

	// One entry point for the whole command vocabulary, mirroring the FFI
	// the Flutter client used. Adding a command to golib adds it here.
	mux.HandleFunc("/cmd", p.guard(p.handleCmd))

	// Tables. Accepting an invitation is a user's decision, taken in the
	// host's interface, so the host is what drives this.
	mux.HandleFunc("/table/join", p.guard(p.handleJoin))
	mux.HandleFunc("/table/leave", p.guard(p.handleLeave))
	mux.HandleFunc("/tables", p.guard(p.handleTables))
	return mux
}

// guard requires the token the host issued this game.
//
// The same token in both directions is deliberate: it is the one secret this
// process and the host share, and it already stands for "this game" in every
// frame sent through the tunnel. Health is left open because it says nothing
// and the portal has no token to present.
//
// It bounds who may drive this process, not who may reach it. The token is
// passed on a command line, so anything else in the sandbox can read it out of
// /proc - which is a property of how the portal launches games, and worth
// fixing there rather than pretended away here.
func (p *plugin) guard(next http.HandlerFunc) http.HandlerFunc {
	want := []byte("Bearer " + p.token)
	return func(w http.ResponseWriter, r *http.Request) {
		got := []byte(strings.TrimSpace(r.Header.Get("Authorization")))
		if subtle.ConstantTimeCompare(got, want) != 1 {
			// Constant time, so a caller cannot learn the token a
			// character at a time.
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		next(w, r)
	}
}

func (p *plugin) handleJoin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Invite string `json:"invite"`
		GCID   string `json:"gcid"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "decode body: "+err.Error(), http.StatusBadRequest)
		return
	}

	inv, err := schema.ParseInvite(req.Invite)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	gcID := strings.ToLower(strings.TrimSpace(req.GCID))
	if !gcIDRe.MatchString(gcID) {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("gcid must be 64 hex characters"))
		return
	}

	out, err := p.tables.join(inv, gcID, p.id)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	p.publish(p.ctx, out)

	log.Printf("pokerplugin: joined table %s in group chat %s", inv.SID, gcID)
	writeJSON(w, map[string]any{"sid": inv.SID, "tables": p.tables.snapshots()})
}

func (p *plugin) handleLeave(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		SID string `json:"sid"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "decode body: "+err.Error(), http.StatusBadRequest)
		return
	}
	// Leaving stops admitting frames for the session, so this is also how
	// a table stops costing anything.
	writeJSON(w, map[string]any{"left": p.tables.leave(strings.TrimSpace(req.SID))})
}

func (p *plugin) handleTables(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"tables": p.tables.snapshots()})
}

// gcIDRe is the shape a Bison Relay group chat id takes, checked here so an
// unusable one is refused rather than carried until the host rejects the first
// frame sent to it.
var gcIDRe = regexp.MustCompile(`^[0-9a-f]{64}$`)

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}

func (p *plugin) handleCmd(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Handle  int32           `json:"handle"`
		Type    uint32          `json:"type"`
		Payload json.RawMessage `json:"payload"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "decode body: "+err.Error(), http.StatusBadRequest)
		return
	}

	out, err := golib.Call(req.Handle, golib.CmdType(req.Type), req.Payload)
	w.Header().Set("Content-Type", "application/json")
	if err != nil {
		// The command failed, not the transport. A 200 with an error
		// field would make every caller check two places.
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	if len(out) == 0 {
		out = []byte("null")
	}
	_, _ = w.Write(out)
}
