// Command pokerplugin is poker as an installable game.
//
// It runs inside the gaming sandbox, supervised by a portal that fetched and
// verified this binary before executing it. It holds no Bison Relay or wallet
// credentials: it reaches the host's bridge and nothing else, because the
// sandbox it runs in has no route anywhere else. What it is given is a bearer
// token, which is its identity rather than a password - the host resolves it to
// decide which game is calling, so this process never states which game it is.
//
// The surface it serves is one route per thing the host can ask for, each of
// them narrow: the host decides what a person wants and this decides whether the
// protocol allows it. There is no general-purpose entry point, deliberately - a
// route that forwards an arbitrary named command is a surface nobody can audit,
// because its shape is whatever the other side happens to send.
package main

import (
	"context"
	"crypto/subtle"
	"encoding/hex"
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

	"github.com/decred/dcrd/txscript/v4/stdaddr"
	"github.com/decred/slog"
	"github.com/vctt94/pokerbisonrelay/pkg/gaming/schema"
	"github.com/vctt94/pokerbisonrelay/pkg/gaming/transport"
	"github.com/vctt94/pokerbisonrelay/pkg/membership"
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
		debug   = flag.Bool("debug", false, "log the transport: stream connections, and frames dropped and why")
		dataDir = flag.String("datadir", "/data/poker", "where this game keeps its identity")
		network = flag.String("network", "mainnet", "the chain this plays on")
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

	params, err := paramsForNetwork(*network)
	if err != nil {
		log.Fatalf("pokerplugin: %v", err)
	}

	p, err := newPlugin(ctx, *bridge, *token, id, newStore(*dataDir), params)
	if err != nil {
		log.Fatalf("pokerplugin: %v", err)
	}
	if *debug {
		p.enableTransportLog()
	}

	// Open the host's frame stream. This is the only way anything reaches
	// this process from another player: the sandbox has no route anywhere
	// but the host, so a game that cannot open this can never play.
	frames, err := p.bridge.Events(ctx)
	if err != nil {
		log.Fatalf("pokerplugin: %v", err)
	}
	go transport.Receive(ctx, frames, p.router)
	go p.watchChain(ctx)
	go p.watchTables(ctx)
	// Payments that were still in flight when this process last stopped. A
	// person may have approved one while it was down, and money that moved
	// with nothing pointing at it is worse than money that has not moved.
	p.resumeSpends()

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
	store  *store
	id     *identity
	token  string
	params stdaddr.AddressParams
	// notify is what says a table moved, to anybody watching. A table moves
	// for reasons nobody asked about - a block, somebody else's turn, a
	// claim - so there has to be something that speaks first.
	notify *notifier
	// spends is every payment asked for and not yet accounted for, kept so
	// a caller need not hold a request open while a person decides.
	spends *spends
}

func newPlugin(ctx context.Context, bridgeURL, token string, id *identity, st *store, params stdaddr.AddressParams) (*plugin, error) {
	b, err := transport.NewBridge(bridgeURL, token, nil)
	if err != nil {
		return nil, err
	}

	p := &plugin{ctx: ctx, bridge: b, tables: newTables(st), store: st, id: id, token: token,
		params: params, notify: newNotifier(), spends: newSpends(st)}
	// A seat has to cost something, so every join is checked against the
	// chain before it is admitted. The rule lives in pkg/membership; what
	// happens here is fetching the facts it needs.
	p.tables.checkBond = newBonds(b, params).check
	// What a stake is judged against: the script is derived here, whether it
	// was paid is the chain's answer.
	p.tables.chain, p.tables.params = b, params
	p.tables.signFunding = func(terms membership.Terms, seat uint32, outpoint string) (*membership.Funding, error) {
		session, err := id.sessionKey(terms.SID)
		if err != nil {
			return nil, err
		}
		return membership.SignFunding(terms, seat, outpoint, session)
	}
	p.tables.signBonded = func(terms membership.Terms, seat uint32, outpoint string) (*membership.Bonded, error) {
		session, err := id.sessionKey(terms.SID)
		if err != nil {
			return nil, err
		}
		return membership.SignBonded(terms, seat, outpoint, session)
	}
	// Asked for on each repeat rather than captured once: the host can rebind
	// the gaming account, and an address held from startup would go on naming
	// one the user has moved away from.
	p.tables.payoutAddr = id.payoutAddress
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

	// The host's stream drops frames rather than blocking, so one game that
	// stops draining cannot stall the others. Reconnecting is where that
	// loss clusters, and a player who missed frames is indistinguishable
	// from one who walked away - so it is said out loud rather than left to
	// be inferred from a table that stopped making sense.
	//
	// Saying so is not enough on its own. Formation messages are published
	// once: a peer whose stream was down while somebody committed is short a
	// signature its table needs to settle, and nothing would ever send it
	// again. So it also asks, naming what it holds, and the table answers
	// with the difference.
	b.OnGap = func() {
		log.Printf("pokerplugin: reconnected to the host; frames may have been missed")
		p.publish(p.ctx, p.tables.resync())
	}

	// Tables that are over and still hold coin. Nothing else reads a session
	// back, so without this a timelocked stake outlives every record of
	// where it is - and the timelock is measured in days while this process
	// is not.
	p.tables.resumeHeld(id)
	return p, nil
}

// enableTransportLog makes the transport say what it is doing.
//
// Without it a frame that is dropped is dropped in silence - an unauthorized
// sender, a payload that will not decode, a stream that never connected all
// look identical from outside, which is to say they look like nothing
// happening at all. That is fine for a game running well and useless the
// moment one is not.
func (p *plugin) enableTransportLog() {
	backend := slog.NewBackend(os.Stdout)
	logger := backend.Logger("GAME")
	logger.SetLevel(slog.LevelDebug)
	p.bridge.Log = logger
	p.router.SetLog(logger)
}

// deliver routes one decoded message to the table it belongs to, and sends
// whatever that produced.
//
// Publishing happens here rather than inside the registry because the registry
// holds a lock, and a slow send under it would stall every other table. An
// answer a handler built goes out here for the same reason: broadcasting is an
// RPC, and it does not belong under the lock either.
func (p *plugin) deliver(d transport.Delivery) {
	p.publish(p.ctx, p.tables.deliver(p.ctx, d))
	p.dispatchAnswers(p.ctx)
}

// chainPoll is how often the host is asked where the chain is. Blocks are about
// five minutes apart, so this is frequent enough that a deadline passes
// promptly and rare enough to be nothing.
const chainPoll = 30 * time.Second

// watchChain keeps every table told where the chain is.
//
// A deadline is a block height because it has to be a fact every peer can check
// and nobody can be shown to have read wrong; clocks disagree, and a table
// whose membership turned on whose clock ran fast would be decided by the
// wrong thing entirely.
func (p *plugin) watchChain(ctx context.Context) {
	ticker := time.NewTicker(chainPoll)
	defer ticker.Stop()

	for {
		tip, err := p.bridge.ChainTip(ctx)
		if err != nil {
			// Nothing to do but wait. A host with no node yet is
			// ordinary at startup, and a table with a deadline
			// nobody can read simply has not reached it.
			if ctx.Err() == nil {
				log.Printf("pokerplugin: cannot read the chain: %v", err)
			}
		} else {
			// Before the tick, because the tick is what proposes a bond
			// release and it cannot build one without the amount.
			p.learnBondValues(ctx)
			// Our own stake and bond are written down when they are paid
			// rather than when they confirm, so this is what lets a table
			// this box paid for start dealing.
			p.confirmOurPayments(ctx)
			// Before the tick as well: a bond that moved is a thing to
			// learn now, and a claim against ours a thing to answer now.
			p.watchBonds(ctx)
			// And the other side of a dispute: a claimed bond whose window
			// has closed is one the table can take.
			p.takeClaimed(ctx)
			p.publish(ctx, p.tables.tick(tip.Height))
			p.drawSeats(ctx, tip.Height)
		}

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// drawSeats gives each settled table the block its seating comes from.
//
// It is a block from after everybody committed, so the draw is something no
// player could have predicted when choosing a key - which matters because seat
// order decides the button, and session keys are free to generate.
func (p *plugin) drawSeats(ctx context.Context, height int64) {
	for sid, at := range p.tables.needSeating(height) {
		hash, err := p.bridge.BlockHash(ctx, at)
		if err != nil {
			log.Printf("pokerplugin: table %s: cannot read the block it seats from: %v", sid, err)
			continue
		}
		raw, err := hex.DecodeString(hash)
		if err != nil || len(raw) == 0 {
			log.Printf("pokerplugin: table %s: block %d has no usable hash", sid, at)
			continue
		}
		p.publish(ctx, p.tables.seat(sid, raw))
	}
}

func (p *plugin) routes() http.Handler {
	mux := http.NewServeMux()

	// The portal reads this to decide whether the game is ready, which is
	// what turns Ready true in the host's UI. It answers only once the
	// process is actually serving, so "ready" means reachable rather than
	// merely started.
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		// Whether the interface is really in here, so a release that
		// shipped the placeholder can be caught by the thing that signs
		// it rather than by a player looking at a page that explains
		// itself through a proxy, inside a frame, where it reads as
		// every layer in between being broken.
		ui := "placeholder"
		if uiBuilt() {
			ui = "built"
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"game":            schema.Game,
			"protocolVersion": schema.Version,
			"ui":              ui,
			"ok":              true,
		})
	})

	// The interface itself, unguarded and framed by the host. See ui.go.
	mux.HandleFunc("/ui/", p.handleUI)

	// Tables. Accepting an invitation is a user's decision, taken in the
	// host's interface, so the host is what drives this.
	mux.HandleFunc("/table/join", p.guard(p.handleJoin))
	mux.HandleFunc("/table/leave", p.guard(p.handleLeave))
	mux.HandleFunc("/tables", p.guard(p.handleTables))

	// Paying into a settled table, and the way back when a payment was made
	// but its outcome never reached the caller that asked for it.
	mux.HandleFunc("/table/fund", p.guard(p.handleFund))
	mux.HandleFunc("/table/deposit/set", p.guard(p.handleDepositSet))

	// The bond a table can take, as opposed to the standing one that buys a
	// seat. Posted once the seating is drawn, because it names the table.
	mux.HandleFunc("/table/bond", p.guard(p.handleTableBond))

	// Where this player wants coin sent that it did not pay for itself: a
	// share of somebody's forfeited bond, and settlement later. This process
	// holds no wallet, so it has to be told.
	mux.HandleFunc("/payout/set", p.guard(p.handlePayoutSet))
	// What identities are called, said by the host at panel mint. Host-only
	// for the same reason /payout/set is: it is the host's knowledge, and a
	// page must not get to rewrite it.
	mux.HandleFunc("/names/set", p.guard(p.handleNamesSet))
	mux.HandleFunc("/payout", p.guard(p.handlePayoutSet))

	// Playing. /table/hand is what a caller polls to know whose turn it is
	// and what it may do; /table/act is the one place a person's decision
	// enters the protocol at all.
	mux.HandleFunc("/table/hand", p.guard(p.handleHand))
	mux.HandleFunc("/table/act", p.guard(p.handleAct))

	// Where the money is, as opposed to where the cards are. It answers
	// before the table deals, because stakes, bonds, payout addresses and a
	// funding deadline all exist and all matter while /table/hand is still
	// saying there is no hand.
	mux.HandleFunc("/table/ledger", p.guard(p.handleLedger))

	// The signed log a table was played from, live or read back from disk.
	// It is the one account of a hand that needs nobody to be believed.
	mux.HandleFunc("/table/log", p.guard(p.handleTableLog))

	// Saying when a table moved, rather than waiting to be asked. A table
	// moves for reasons nobody asked about, so there has to be one route
	// here that speaks first.
	mux.HandleFunc("/events", p.guard(p.handleEvents))

	// What became of a payment somebody asked for and did not wait on. A
	// page cannot hold a request open for the half hour a person may take
	// to approve a bond, so it asks here instead.
	mux.HandleFunc("/spend", p.guard(p.handleSpend))

	// Taking our own coin back out, once its lock has matured. The escape
	// hatch that works when nothing else does.
	mux.HandleFunc("/table/refund", p.guard(p.handleTableRefund))
	mux.HandleFunc("/bond/sweep", p.guard(p.handleBondSweep))
	// Three locks, three routes, because the coin behind each is held by a
	// different key on a different clock: the stake by the session key for
	// this table's CSV, the standing bond by the identity's bond key for the
	// minimum, and this by the session key for a week.
	mux.HandleFunc("/table/bond/sweep", p.guard(p.handleTableBondSweep))
	// What is still locked at tables, and when each of it comes back.
	mux.HandleFunc("/table/bonds", p.guard(p.handleTableBonds))

	// The seed nothing can regenerate, and the one way to put it back.
	mux.HandleFunc("/identity/backup", p.guard(p.handleIdentityBackup))
	mux.HandleFunc("/identity/restore", p.guard(p.handleIdentityRestore))

	// The bond, which is what makes a seat cost something. Without one this
	// player cannot join anything.
	mux.HandleFunc("/bond", p.guard(p.handleBond))
	mux.HandleFunc("/bond/fund", p.guard(p.handleBondFund))
	mux.HandleFunc("/bond/set", p.guard(p.handleBondSet))
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
	// A table that is dealing settles at its next boundary rather than being
	// dropped: leaving has to be something other than walking out, or the
	// only exit is the one a bond claim answers.
	out, left := p.tables.leave(strings.TrimSpace(req.SID))
	p.publish(r.Context(), out)
	writeJSON(w, map[string]any{"left": left})
}

func (p *plugin) handleTables(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"tables": p.tables.snapshots()})
}

// handleIdentityBackup hands out the seed this player is derived from, so the
// host can offer it to the person who would otherwise lose it.
//
// It is the one secret here that nothing can regenerate. The bond key and every
// session key are derived from it, so a data volume removed without a copy
// leaves the bond unspendable forever and any stake in escrow unrefundable.
// It is also the one route here that must never be reachable from a browser,
// and this process cannot enforce that: every request arrives with the same
// valid token, and there is no honest way to tell a proxied page from the host.
// The enforcement is the host's route allowlist.
//
// So this asks for a header as well - one no proxy has any reason to forward.
// It stops a page that got as far as this route from reading the seed with an
// ordinary fetch, which is not the same as making it safe, and is worth having
// precisely because the real control lives in another repository.
func (p *plugin) handleIdentityBackup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET required", http.StatusMethodNotAllowed)
		return
	}
	if r.Header.Get(confirmHeader) != confirmSeed {
		writeErr(w, http.StatusForbidden, fmt.Errorf(
			"reading the seed needs the %s: %s header, and is not something to do from a game's own interface",
			confirmHeader, confirmSeed))
		return
	}
	seed, outpoint := p.id.backup()
	writeJSON(w, map[string]any{"seedHex": seed, "bondOutpoint": outpoint})
}

// handleIdentityRestore puts a saved seed back, onto a player that has not
// played.
//
// Refusing the rest is the point. This process derives its keys at startup, so
// by the time anyone can call this an identity already exists - and replacing
// one that holds a bond or has sat at a table would strand the first and
// invalidate the second. A restore belongs on an empty volume, and saying so is
// better than half-applying it.
func (p *plugin) handleIdentityRestore(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		SeedHex      string `json:"seedHex"`
		BondOutpoint string `json:"bondOutpoint"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "decode body: "+err.Error(), http.StatusBadRequest)
		return
	}

	used, err := p.store.used()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if used {
		writeErr(w, http.StatusConflict,
			fmt.Errorf("this player has already sat at a table; restore onto an empty data volume"))
		return
	}
	if err := p.id.restore(req.SeedHex, req.BondOutpoint); err != nil {
		writeErr(w, http.StatusConflict, err)
		return
	}
	log.Printf("pokerplugin: identity restored from a backup")
	writeJSON(w, map[string]any{"restored": true, "bondOutpoint": p.id.bondDeposit()})
}

// confirmHeader is asked for by the routes that hand out something no page
// should ever hold. It is not a secret and not authentication - it is a
// deliberate step, of the kind a fetch from a page does not take by accident.
const (
	confirmHeader = "X-Poker-Confirm"
	confirmSeed   = "seed"
)

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
