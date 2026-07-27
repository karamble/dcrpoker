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
	"encoding/json"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
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
		listen = flag.String("listen", "127.0.0.1:8790", "address the host drives this game on")
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

	p, err := newPlugin(*bridge, *token)
	if err != nil {
		log.Fatalf("pokerplugin: %v", err)
	}

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
	bridge *transport.Bridge
	router *transport.Router
}

func newPlugin(bridgeURL, token string) (*plugin, error) {
	b, err := transport.NewBridge(bridgeURL, token, nil)
	if err != nil {
		return nil, err
	}

	p := &plugin{bridge: b}
	p.router, err = transport.NewRouter(transport.Config{
		Game:    schema.Game,
		GameVer: schema.Version,
		Sender:  b,
		// Nothing is authorized yet: a table's roster is what says who may
		// be believed about its history, and this process holds no table
		// until it joins one. Allowing anyone in the meantime would let a
		// stranger fill the reassembly buffers of a game that is not even
		// playing.
		Authorize: func(string, string) bool { return false },
		Handle:    p.deliver,
	})
	if err != nil {
		return nil, err
	}
	return p, nil
}

// deliver receives a decoded message from the table.
func (p *plugin) deliver(d transport.Delivery) {
	log.Printf("pokerplugin: %s from %s at table %s", d.Msg.Kind, d.Sender, d.SID)
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
	mux.HandleFunc("/cmd", p.handleCmd)
	return mux
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
