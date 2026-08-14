// Command dcrpoker is poker played between Bison Relay peers.
//
// It holds no Bison Relay or wallet credentials. It reaches one dcrpulse bridge
// and nothing else, over mutual TLS where the client certificate is the
// identity: the bridge resolves it to decide which game is calling, so this
// process is never believed about which game it is. Money moves only by asking
// that bridge, which asks a person.
//
// It serves its own interface on loopback, guarded by a token minted fresh each
// run and printed to the terminal that started it. That token is a session key
// rather than a password, which is why the listener may not leave loopback.
package main

import (
	"context"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"github.com/decred/dcrd/txscript/v4/stdaddr"
	"github.com/vctt94/dcrpoker/internal/config"
	dcrlog "github.com/vctt94/dcrpoker/internal/log"
	"github.com/vctt94/dcrpoker/pkg/gaming/schema"
	"github.com/vctt94/dcrpoker/pkg/gaming/transport"
	"github.com/vctt94/dcrpoker/pkg/membership"
)

func main() {
	// Answered before anything is parsed, loaded or created. The release
	// script asks this of a freshly built binary that has no configuration
	// and no data directory, and reads both the answer and the exit code, so
	// nothing here may depend on either existing or touch the disk.
	if asksAboutInterface(os.Args[1:]) {
		if !uiBuilt() {
			fmt.Println("interface: placeholder")
			os.Exit(1)
		}
		fmt.Println("interface: built")
		return
	}

	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", config.AppName, err)
		os.Exit(1)
	}
}

// asksAboutInterface reports whether the release script's question is on the
// line. Scanned rather than parsed: a parser that had already read a config
// file could fail for an unrelated reason and be reported as a binary shipping
// the placeholder, which is an accusation whose obvious fix is to delete the
// check.
func asksAboutInterface(args []string) bool {
	for _, a := range args {
		if a == "--check-interface" || a == "-check-interface" {
			return true
		}
	}
	return false
}

func run() (err error) {
	cfg, err := config.Load(os.Args[1:], version())
	if err != nil {
		if config.IsDone(err) {
			return nil
		}
		return err
	}

	if err := dcrlog.InitRotator(cfg.LogFile(), cfg.RollSizeKB); err != nil {
		return err
	}
	// Closed last, after the deferred report below has had its say. Anything
	// that fails before this point can only reach stderr, which includes every
	// way the configuration itself can be wrong.
	defer dcrlog.CloseRotator()
	defer func() {
		if err != nil {
			// Said twice on purpose: the file is where an operator looks after
			// a crash, and stderr is where whatever started this looks.
			pokrLog.Errorf("%v", err)
		}
	}()
	if err := dcrlog.SetDebugLevel(cfg.DebugLevel); err != nil {
		return err
	}
	dcrlog.RedirectStdLog(dcrlog.POKR)

	// What listens here is this game's own interface, for the person sitting
	// at this machine. Serving it anywhere but loopback would turn a token
	// printed to a terminal into a network credential, which is not what it is
	// or how it is handed over.
	if !loopbackOnly(cfg.Listen) {
		return fmt.Errorf("listen must be a loopback address; %s would put this game's "+
			"interface on the network, guarded only by a token printed to this terminal", cfg.Listen)
	}

	pokrLog.Infof("dcrpoker starting on %s, protocol %d", cfg.Network(), schema.Version)
	pokrLog.Infof("App data: %s", cfg.AppDataDir)
	pokrLog.Infof("Data:     %s", cfg.DataDir)
	pokrLog.Infof("Log:      %s", cfg.LogFile())

	bridgeCfg, err := loadBridge(cfg, os.Stdin, os.Stdout, isTerminal(os.Stdin))
	if err != nil {
		return err
	}
	// The chain the bridge was set up against decides, because that answer was
	// checked by connecting and this one was typed. Disagreeing about it is
	// refused rather than resolved: the two build different scripts, and the
	// difference is paid for in real money.
	network := cfg.Network()
	if stored := storedNetwork(cfg.DataDir); stored != "" && stored != network {
		return fmt.Errorf("this game was set up against %s but has been asked to run on %s; "+
			"change the configuration back or connect it to a %s bridge", stored, network, network)
	}

	params, err := paramsForNetwork(network)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	id, err := loadIdentity(cfg.DataDir)
	if err != nil {
		// Without a seed there are no session keys, and without those
		// there is no way to hold a seat. Starting anyway would mean
		// joining tables this process could never sign for.
		return err
	}

	store := newStore(cfg.DataDir)
	if left := store.strandedTranscripts(); len(left) > 0 {
		return fmt.Errorf("%d transcript(s) are still in %s and nothing reads them there: %s. Move them to %s",
			len(left), filepath.Join(cfg.DataDir, "logs"), strings.Join(left, ", "),
			filepath.Join(cfg.DataDir, transcriptDir))
	}

	p, err := newPlugin(ctx, bridgeCfg, id, store, params)
	if err != nil {
		return err
	}
	p.wireTransportLog()

	// Introduce this game before anything else. It learns what the bridge
	// resolved its credential to, and - the part that has to stop the
	// program - which chain the bridge is on. A game playing across a
	// network mismatch builds scripts nobody can spend and pays real money
	// into them.
	hello, err := p.bridge.Hello(ctx, network)
	if err != nil {
		return err
	}
	brdgLog.Infof("the bridge knows this game as %q on %s", hello.GetGame(), hello.GetNetwork())

	// The one way anything reaches this process from another player.
	frames, err := p.bridge.Events(ctx)
	if err != nil {
		return err
	}
	go transport.Receive(ctx, frames, p.router)
	// What the operator asked for, on the same stream. Nothing else can make
	// this process do anything: the console has no route to it.
	go p.serveBridgeRequests(ctx)
	go p.watchChain(ctx)
	go p.watchTables(ctx)
	// Payments that were still in flight when this process last stopped. A
	// person may have approved one while it was down, and money that moved
	// with nothing pointing at it is worse than money that has not moved.
	p.resumeSpends()

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           p.routes(),
		ReadHeaderTimeout: 10 * time.Second,
		// Its own tag, so a scanner rattling the door can be silenced without
		// silencing anything that matters.
		ErrorLog: dcrlog.StdErrorLogger(dcrlog.HTTP),
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()

	pokrLog.Infof("open %s to play; connected to the bridge at %s as %q",
		uiURL(cfg.Listen, p.uiToken), bridgeCfg.Addr, p.bridge.Game())
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	pokrLog.Info("stopped")
	return nil
}

// version reports what this build is, in the shape dcrd answers in.
//
// There is no release number to state, so the revision stands in for one: it
// says what can be proved about this file rather than what a constant somebody
// forgot to raise says. A build from a modified tree says so, because that is
// exactly when the revision alone would mislead.
func version() string {
	rev, dirty := "unknown", false
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, s := range info.Settings {
			switch s.Key {
			case "vcs.revision":
				if len(s.Value) > 12 {
					s.Value = s.Value[:12]
				}
				rev = s.Value
			case "vcs.modified":
				dirty = s.Value == "true"
			}
		}
	}
	if dirty {
		rev += "-dirty"
	}
	return fmt.Sprintf("%s (protocol %d, Go version %s %s/%s)",
		rev, schema.Version, runtime.Version(), runtime.GOOS, runtime.GOARCH)
}

type plugin struct {
	ctx     context.Context
	bridge  *transport.Bridge
	router  *transport.Router
	tables  *tables
	store   *store
	id      *identity
	uiToken string
	params  stdaddr.AddressParams
	// notify is what says a table moved, to anybody watching. A table moves
	// for reasons nobody asked about - a block, somebody else's turn, a
	// claim - so there has to be something that speaks first.
	notify *notifier
	// spends is every payment asked for and not yet accounted for, kept so
	// a caller need not hold a request open while a person decides.
	spends *spends
}

func newPlugin(ctx context.Context, bridgeCfg transport.BridgeConfig, id *identity, st *store, params stdaddr.AddressParams) (*plugin, error) {
	b, err := transport.Dial(ctx, bridgeCfg)
	if err != nil {
		return nil, err
	}

	uiToken, err := newUIToken()
	if err != nil {
		return nil, err
	}

	p := &plugin{ctx: ctx, bridge: b, tables: newTables(st), store: st, id: id, uiToken: uiToken,
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
	b.SetOnGap(func(gcids []string) {
		if len(gcids) > 0 {
			brdgLog.Warnf("the bridge missed frames for %d table(s); resynchronising", len(gcids))
		} else {
			brdgLog.Warnf("the bridge missed frames and could not say which tables; resynchronising")
		}
		p.publish(p.ctx, p.tables.resync())
	})

	// Tables that are over and still hold coin. Nothing else reads a session
	// back, so without this a timelocked stake outlives every record of
	// where it is - and the timelock is measured in days while this process
	// is not.
	p.tables.resumeHeld(id)
	return p, nil
}

// wireTransportLog lets the transport say what it is doing.
//
// Always wired, and quiet until asked for with debuglevel: a frame that is
// dropped is otherwise dropped in silence, and an unauthorized sender, a
// payload that will not decode and a stream that never connected all look
// identical from outside, which is to say they look like nothing happening.
func (p *plugin) wireTransportLog() {
	p.bridge.SetLog(brdgLog)
	p.router.SetLog(brdgLog)
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
				chanLog.Errorf("cannot read the chain: %v", err)
			}
		} else {
			// Before the tick, because the tick is what proposes a bond
			// release and it cannot build one without the amount.
			p.learnBondValues(ctx)
			// Our own stake and bond are written down when they are paid
			// rather than when they confirm, so this is what lets a table
			// this box paid for start dealing.
			p.confirmOurPayments(ctx, tip.Height)
			// Before the tick as well: a bond that moved is a thing to
			// learn now, and a claim against ours a thing to answer now.
			p.watchBonds(ctx)
			// The bookend of confirmOurPayments: a stake the chain has
			// paid back out is forgotten, which is what finally lets a
			// receipt whose coin is all accounted for be dropped.
			p.forgetSpentStakes(ctx, tip.Height)
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
			tablLog.Errorf("table %s: cannot read the block it seats from: %v", sid, err)
			continue
		}
		raw, err := hex.DecodeString(hash)
		if err != nil || len(raw) == 0 {
			tablLog.Errorf("table %s: block %d has no usable hash", sid, at)
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
	mux.HandleFunc("/table/leave", p.guard(p.handleLeave))
	mux.HandleFunc("/table/challenge", p.guard(p.handleTableChallenge))
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
	// What identities are called, said by the host at panel mint. Host-only
	// for the same reason /payout/set is: it is the host's knowledge, and a
	// page must not get to rewrite it.

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
	// Three locks, three routes, because the coin behind each is held by a
	// different key on a different clock: the stake by the session key for
	// this table's CSV, the standing bond by the identity's bond key for the
	// minimum, and this by the session key for a week.
	// What is still locked at tables, and when each of it comes back.

	// The seed nothing can regenerate, and the one way to put it back.
	mux.HandleFunc("/identity/backup", p.guard(p.handleIdentityBackup))
	mux.HandleFunc("/identity/restore", p.guard(p.handleIdentityRestore))
	mux.HandleFunc("/identity/acknowledge", p.guard(p.handleSeedAcknowledge))

	// The bond, which is what makes a seat cost something. Without one this
	// player cannot join anything.
	mux.HandleFunc("/bond", p.guard(p.handleBond))
	mux.HandleFunc("/bond/fund", p.guard(p.handleBondFund))
	mux.HandleFunc("/bond/set", p.guard(p.handleBondSet))
	return mux
}

// guard requires this run's interface token.
//
// It is not the bridge credential and shares nothing with it. That one is an
// identity a person carried to this machine and it moves money; this is a
// session key for a page served to a browser on the same machine, minted at
// startup and printed once.
//
// It bounds who may drive this process, not who may reach it - so the listener
// is loopback, checked at startup. The old arrangement passed a token on the
// command line where anything on the box could read it out of /proc; nothing
// is passed on the command line now.
func (p *plugin) guard(next http.HandlerFunc) http.HandlerFunc {
	want := []byte("Bearer " + p.uiToken)
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
	pokrLog.Infof("identity restored from a backup")
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
