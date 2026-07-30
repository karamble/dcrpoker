package main

import (
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.dedis.ch/kyber/v4"

	"github.com/vctt94/pokerbisonrelay/pkg/deck"
	"github.com/vctt94/pokerbisonrelay/pkg/driver"
	"github.com/vctt94/pokerbisonrelay/pkg/gaming/schema"
	"github.com/vctt94/pokerbisonrelay/pkg/membership"
)

// challengeOf is one table's view of a challenge, or nil.
func challengeOf(t *testing.T, p *plugin, sid string, hand uint64) *challengeView {
	t.Helper()
	v := ledgerOf(t, p, sid)
	for i := range v.Challenges {
		if v.Challenges[i].Hand == hand {
			return &v.Challenges[i]
		}
	}
	return nil
}

// audited reports whether every peer has recomputed the hand clean.
func audited(t *testing.T, sid string, hand uint64, peers ...*plugin) bool {
	t.Helper()
	for _, p := range peers {
		ch := challengeOf(t, p, sid, hand)
		if ch == nil || ch.Verdict != "clean" {
			return false
		}
	}
	return true
}

// waitAudited waits until every peer has recomputed the hand clean, on the
// frames alone.
//
// Deliberately without ticking the chain. A challenge and the reveals it
// obliges are answered on delivery, so needing a block here would mean the
// exchange only works on the clock - and the shared height this harness counts
// in is one every other test reads, so a helper that spent blocks freely would
// push somebody else's table past its funding deadline.
func waitAudited(t *testing.T, h *hub, sid string, hand uint64, peers ...*plugin) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		h.inflight.Wait()
		if audited(t, sid, hand, peers...) {
			return
		}
		if time.Now().After(deadline) {
			for i, p := range peers {
				v := ledgerOf(t, p, sid)
				t.Logf("peer %d challenges %+v events %+v", i, v.Challenges, v.Events)
			}
			t.Fatal("the challenged hand was never recomputed clean everywhere")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A challenged hand is revealed by every seat and recomputed clean, with the
// audited cards reported - the whole path, no proof consulted anywhere.
func TestAChallengedHandIsRevealedAndAuditsClean(t *testing.T) {
	h := newHub(t)
	a, b, terms := dealingTable(t, h)
	waitBetting(t, a, b)
	playHand(t, h, terms.SID, checkOrCall, a, b)
	waitSettled(t, terms.SID, 1, a, b)

	if code, body := post(t, a, "/table/challenge", map[string]any{"sid": terms.SID, "hand": 1}); code != http.StatusOK {
		t.Fatalf("/table/challenge returned %d: %s", code, body)
	}
	waitAudited(t, h, terms.SID, 1, a, b)

	ch := challengeOf(t, a, terms.SID, 1)
	if len(ch.Cards) == 0 {
		t.Fatal("a clean audit reported no cards, and the audited hand is the point of asking")
	}
	for _, p := range []*plugin{a, b} {
		if !hasEvent(ledgerOf(t, p, terms.SID).Events, eventAudited) {
			t.Fatal("a peer recomputed the hand and never said so")
		}
		if hasEvent(ledgerOf(t, p, terms.SID).Events, eventCheat) {
			t.Fatal("an honest hand was called a cheat")
		}
	}
}

// A lost challenge and a lost reveal are both said again, and the audit still
// completes: repeated on a clock, stopped by the challenge closing.
func TestAChallengeSurvivesALostFrame(t *testing.T) {
	h := newHub(t)
	a, b, terms := dealingTable(t, h)
	waitBetting(t, a, b)
	playHand(t, h, terms.SID, checkOrCall, a, b)
	waitSettled(t, terms.SID, 1, a, b)

	h.drop(schema.KindChallenge, 1)
	h.drop(schema.KindSecrets, 1)
	if code, body := post(t, a, "/table/challenge", map[string]any{"sid": terms.SID, "hand": 1}); code != http.StatusOK {
		t.Fatalf("/table/challenge returned %d: %s", code, body)
	}

	// The first telling of each is gone, so only the repeats can finish this.
	// Bounded, because the blocks spent here are counted by every other test.
	for i := 0; i < 6 && !audited(t, terms.SID, 1, a, b); i++ {
		advance(t, h, 1, a, b)
	}
	if !audited(t, terms.SID, 1, a, b) {
		for i, p := range []*plugin{a, b} {
			t.Logf("peer %d challenges %+v", i, ledgerOf(t, p, terms.SID).Challenges)
		}
		t.Fatal("a lost challenge and a lost reveal were never said again")
	}
	if got := h.dropped(schema.KindChallenge) + h.dropped(schema.KindSecrets); got == 0 {
		t.Fatal("nothing was lost, so nothing was proven about the repeats")
	}
}

// A finished receipt says nothing - except its challenges. The silence gates
// on every other repeat must not reach these two: an open challenge is
// restored from disk with the receipt, a restarted peer still owes its reveal,
// and a seat that stops answering because it got up is exactly the refusal a
// challenge makes claimable.
func TestAFinishedReceiptStillAnswersItsChallenges(t *testing.T) {
	h := newHub(t)
	a, b, terms := dealingTable(t, h)
	waitBetting(t, a, b)
	playHand(t, h, terms.SID, checkOrCall, a, b)
	waitSettled(t, terms.SID, 1, a, b)

	if code, body := post(t, a, "/table/challenge", map[string]any{"sid": terms.SID, "hand": 1}); code != http.StatusOK {
		t.Fatalf("/table/challenge returned %d: %s", code, body)
	}

	dir := filepath.Dir(a.tables.store.dir)
	back := h.restart(t, dir, "tok-a")
	tbl := back.tables.m[terms.SID]
	if tbl == nil {
		t.Fatal("the table did not come back")
	}
	if !tbl.finished || len(tbl.openChal) == 0 {
		t.Fatalf("finished=%v with %d open challenges; this is not testing a challenged receipt",
			tbl.finished, len(tbl.openChal))
	}

	out := back.tables.tick(int64(terms.Until) + 50)
	var chals, secrets int
	for _, o := range out {
		switch o.kind {
		case schema.KindChallenge:
			chals++
		case schema.KindSecrets:
			secrets++
		default:
			t.Fatalf("a challenged receipt said a %s; only challenge traffic may outlive finished", o.kind)
		}
	}
	if chals == 0 {
		t.Fatal("the restarted challenger stopped repeating its own challenge")
	}
	if secrets == 0 {
		t.Fatal("the restarted peer stopped revealing for an open challenge")
	}
}

// A reveal refused past the window becomes the claim, with the table over -
// which is when most challenges happen and when nothing else is owed.
func TestARefusedRevealCostsTheBond(t *testing.T) {
	h := newHub(t)
	a, b, terms := dealingTable(t, h)
	waitBetting(t, a, b)

	for _, p := range []*plugin{a, b} {
		addr := payoutAddress(t, p)
		if code, body := post(t, p, "/payout/set", map[string]string{"address": addr}); code != http.StatusOK {
			t.Fatalf("/payout/set returned %d: %s", code, body)
		}
	}
	waitPayouts(t, terms.SID, a, b)
	waitAccusable(t, h, terms.SID, a, b)

	playHand(t, h, terms.SID, checkOrCall, a, b)
	waitSettled(t, terms.SID, 1, a, b)

	// The table ends first, which is when a hand is usually doubted and when
	// a reveal is the only thing anybody still owes. Everything else about
	// this scenario depends on that: a finished table proposes no claims at
	// all unless a challenge is open.
	getUp(t, h, terms.SID, a)
	waitOver(t, h, terms.SID, a, b)

	// Then the refuser goes quiet, and then the hand is challenged.
	h.silence(t, "tok-b")
	_ = b
	if code, body := post(t, a, "/table/challenge", map[string]any{"sid": terms.SID, "hand": 1}); code != http.StatusOK {
		t.Fatalf("/table/challenge returned %d: %s", code, body)
	}
	a.tables.mu.Lock()
	if !a.tables.m[terms.SID].play.Over() {
		a.tables.mu.Unlock()
		t.Fatal("the table is not over, so this is not testing a challenge after the game")
	}
	a.tables.mu.Unlock()

	deadline := time.Now().Add(30 * time.Second)
	height := int64(terms.Until) + 2
	for {
		height += int64(claimAfter) + 1
		playOn(t, a, terms.SID)
		a.publish(t.Context(), a.tables.tick(height))

		v := ledgerOf(t, a, terms.SID)
		for _, c := range v.Claims {
			if c.Duty.Kind == driver.DutyReveal {
				if !hasEvent(v.Events, eventProposed) {
					t.Fatal("a reveal claim exists and was never reported")
				}
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("refusing a reveal never became a claim; claims %+v events %+v",
				v.Claims, v.Events)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// A reveal somebody tampered with in flight discharges nothing: the signature
// is checked against the roster, and the duty stands.
func TestATamperedRevealDischargesNothing(t *testing.T) {
	h := newHub(t)
	a, b, terms := dealingTable(t, h)
	waitBetting(t, a, b)
	playHand(t, h, terms.SID, checkOrCall, a, b)
	waitSettled(t, terms.SID, 1, a, b)

	// The real reveals never arrive; only the forgery does.
	h.drop(schema.KindSecrets, 50)
	if code, body := post(t, a, "/table/challenge", map[string]any{"sid": terms.SID, "hand": 1}); code != http.StatusOK {
		t.Fatalf("/table/challenge returned %d: %s", code, body)
	}
	advance(t, h, 2, a, b)

	atbl := a.tables.m[terms.SID]
	theirs := theirSeat(t, atbl)
	own := atbl.bundles[1].view.Own
	forged := *own
	forged.Seat = theirs
	forged.Sig = "00"
	deliverKind(t, a, terms, schema.KindSecrets, forged)

	advance(t, h, 1, a)
	ch := challengeOf(t, a, terms.SID, 1)
	if ch == nil || !ch.Open {
		t.Fatal("a forged reveal closed the challenge")
	}
	v := ledgerOf(t, a, terms.SID)
	if hasEvent(v.Events, eventAudited) || hasEvent(v.Events, eventCheat) {
		t.Fatal("a forged reveal reached a verdict")
	}
}

// An open challenge blocks the payout and the releases, and closing it lets
// them go: nothing gets paid until the table has answered for itself.
//
// Checked directly rather than by holding reveals back on the wire, because a
// reveal held back long enough is a reveal being refused, and the machinery
// correctly answers that with an accusation - a different test, and one that
// would race this one for the same table.
func TestAnOpenChallengeBlocksSettlementAndRelease(t *testing.T) {
	h := newHub(t)
	a, b, terms := dealingTable(t, h)
	waitBetting(t, a, b)

	sayWhereToPay(t, h, a, b)
	playHand(t, h, terms.SID, checkOrCall, a, b)
	waitSettled(t, terms.SID, 1, a, b)
	getUp(t, h, terms.SID, a)
	waitOver(t, h, terms.SID, a, b)

	a.tables.mu.Lock()
	defer a.tables.mu.Unlock()
	atbl := a.tables.m[terms.SID]
	mine, _ := atbl.form.OurSeat()
	if err := atbl.recordChallenge(1, mine); err != nil {
		t.Fatalf("opening a challenge on the settled hand: %v", err)
	}
	if !atbl.challengeOpen() {
		t.Fatal("the challenge did not open, so the gate is not being tested")
	}
	if out := atbl.proposeSettlement(); len(out) != 0 {
		t.Fatal("a settlement was proposed while a hand is challenged")
	}
	if out := atbl.proposeReleases(); len(out) != 0 {
		t.Fatal("a bond release was proposed while a hand is challenged")
	}

	// And with the challenge closed, both go out. Same table, same moment:
	// the only thing that changed is the challenge.
	atbl.closeChallenge(1, verdictClean)
	if out := atbl.proposeSettlement(); len(out) == 0 {
		t.Fatal("no settlement was proposed once the challenge closed")
	}
	if out := atbl.proposeReleases(); len(out) == 0 {
		t.Fatal("no bond release was proposed once the challenge closed")
	}
}

// A restarted peer answers a challenge from its stored bundle, with no driver,
// and the challenger's audit completes on it.
func TestSecretsAnswerFromDiskAfterARestart(t *testing.T) {
	h := newHub(t)
	a, b, terms := dealingTable(t, h)
	waitBetting(t, a, b)
	playHand(t, h, terms.SID, checkOrCall, a, b)
	waitSettled(t, terms.SID, 1, a, b)

	dir := filepath.Dir(a.tables.store.dir)
	back := h.restart(t, dir, "tok-a")
	btbl := back.tables.m[terms.SID]
	if btbl == nil {
		t.Fatal("the table did not come back")
	}
	if btbl.play != nil {
		t.Fatal("a restarted table has a driver, so this is not testing the disk path")
	}

	// The challenge, issued by the live peer, delivered by hand: a restarted
	// plugin is not on this harness's wire.
	b.tables.mu.Lock()
	chalOut, err := b.tables.m[terms.SID].challengeHand(1)
	b.tables.mu.Unlock()
	if err != nil {
		t.Fatalf("the live peer could not challenge: %v", err)
	}
	var chal *schema.Challenge
	var bSecrets *schema.Secrets
	for _, o := range chalOut {
		switch body := o.body.(type) {
		case schema.Challenge:
			chal = &body
		case schema.Secrets:
			bSecrets = &body
		}
	}
	if chal == nil || bSecrets == nil {
		t.Fatalf("challenging produced %d frames and not the challenge plus the reveal", len(chalOut))
	}

	out := deliverKind(t, back, terms, schema.KindChallenge, *chal)
	var backSecrets *schema.Secrets
	for _, o := range out {
		if body, ok := o.body.(schema.Secrets); ok {
			backSecrets = &body
		}
	}
	if backSecrets == nil {
		t.Fatal("the restarted peer did not reveal from its stored bundle")
	}

	// Both sides audit: the live peer from the restarted one's reveal, and
	// the restarted one from the live peer's.
	deliverKind(t, b, terms, schema.KindSecrets, *backSecrets)
	deliverKind(t, back, terms, schema.KindSecrets, *bSecrets)
	for name, p := range map[string]*plugin{"live": b, "restarted": back} {
		ch := challengeOf(t, p, terms.SID, 1)
		if ch == nil || ch.Verdict != "clean" {
			t.Fatalf("the %s peer never recomputed the hand clean: %+v", name, ch)
		}
	}
}

// A table whose coin is out deletes its hand secrets; one still holding coin
// keeps them.
func TestAFinishedTableDeletesItsHandSecrets(t *testing.T) {
	st := newStore(t.TempDir())
	if err := st.saveHand("ab12", 1, []byte(`{}`)); err != nil {
		t.Fatalf("save: %v", err)
	}
	if blob, err := st.loadHand("ab12", 1); err != nil || blob == nil {
		t.Fatalf("load: %v %v", blob, err)
	}
	if err := st.deleteHands("ab12"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if blob, _ := st.loadHand("ab12", 1); blob != nil {
		t.Fatal("deleted hand secrets are still on disk")
	}
	if err := st.saveHand("../evil", 1, []byte(`{}`)); err == nil {
		t.Fatal("a hostile session id built a path")
	}
	if err := st.deleteHands("../evil"); err == nil {
		t.Fatal("a hostile session id deleted a path")
	}
}

// A hand that recomputed clean is not challenged again.
//
// Without this the loop is free: challenge, everybody reveals, clean, close,
// challenge again - and each turn of it blocks the payout while the challenger
// owes nothing at any point, because it discharges its own reveal every round.
// The one-at-a-time rule bounds how many are open, not how many there can be.
func TestAnAnsweredHandIsNotChallengedAgain(t *testing.T) {
	h := newHub(t)
	a, b, terms := dealingTable(t, h)
	waitBetting(t, a, b)
	playHand(t, h, terms.SID, checkOrCall, a, b)
	waitSettled(t, terms.SID, 1, a, b)

	if code, body := post(t, a, "/table/challenge", map[string]any{"sid": terms.SID, "hand": 1}); code != http.StatusOK {
		t.Fatalf("/table/challenge returned %d: %s", code, body)
	}
	waitAudited(t, h, terms.SID, 1, a, b)

	// The challenger's slot is free again, and the hand is still not reopenable.
	code, body := post(t, a, "/table/challenge", map[string]any{"sid": terms.SID, "hand": 1})
	if code == http.StatusOK {
		t.Fatal("a hand that recomputed clean was challenged again, which blocks the payout for nothing")
	}
	if !strings.Contains(body, "already been answered for") {
		t.Fatalf("the refusal does not say why: %s", body)
	}

	// And the other seat refuses a repeat of the challenge frame too, so the
	// bound does not depend on who asks.
	b.tables.mu.Lock()
	btbl := b.tables.m[terms.SID]
	err := btbl.recordChallenge(1, uint32(0))
	b.tables.mu.Unlock()
	if err == nil {
		t.Fatal("the other seat would reopen a hand it had already recomputed")
	}

	// The verdict survives a restart, or the bound lasts only as long as the
	// process does.
	dir := filepath.Dir(a.tables.store.dir)
	back := h.restart(t, dir, "tok-a")
	back.tables.mu.Lock()
	got := back.tables.m[terms.SID].judged[1]
	back.tables.mu.Unlock()
	if got != verdictClean {
		t.Fatalf("after a restart hand 1's verdict is %q, want %q", got, verdictClean)
	}
}

// Taking one refuser's bond does not close a hand another seat is still short
// on: one refuser being paid for must not launder the rest of them.
func TestTakingOneBondDoesNotLaunderTheOtherRefusers(t *testing.T) {
	h := newHub(t)
	inv := testInvite(3)
	terms := inviteTerms(inv)
	a := h.join(t, "tok-a")
	b := h.join(t, "tok-b")
	c := h.join(t, "tok-c")
	peers := []*plugin{a, b, c}
	for _, p := range peers {
		acceptInvite(t, p, inv)
	}
	waitFor(t, membership.Settled, peers...)

	beacon := make([]byte, 32)
	for i := range beacon {
		beacon[i] = byte(i + 11)
	}
	for _, p := range peers {
		p.tables.seat(terms.SID, beacon)
	}

	// A hand this peer holds a record of, built directly: the point here is
	// which challenges close, and playing three seats out to a showdown would
	// be a different test.
	a.tables.mu.Lock()
	atbl := a.tables.m[terms.SID]
	mine, _ := atbl.form.OurSeat()
	seats, _ := atbl.form.Seats()
	atbl.bundles = map[uint64]*handBundle{1: {
		hand:     &deck.Hand{Match: "m", Hand: 1, Pubs: make([]kyber.Point, len(seats))},
		revealed: map[uint32]*deck.Secrets{},
		view:     &schema.HandRecordView{Own: &schema.Secrets{}},
	}}
	atbl.openChal = map[uint64]uint32{1: mine}

	// Two other seats have not revealed. One of their bonds is taken.
	others := []uint32{}
	for seat := range uint32(len(seats)) {
		if seat != mine {
			others = append(others, seat)
		}
	}
	atbl.closeChallengesAfterTake(others[0])
	stillOpen := atbl.challengeOpen()
	a.tables.mu.Unlock()

	if !stillOpen {
		t.Fatal("taking one refuser's bond closed a hand another seat is still short on")
	}

	// And once every outstanding seat has paid, it closes.
	a.tables.mu.Lock()
	atbl.closeChallengesAfterTake(others[1])
	closed := !atbl.challengeOpen()
	verdict := atbl.judged[1]
	a.tables.mu.Unlock()
	if !closed {
		t.Fatal("every outstanding seat paid its bond and the hand is still challenged")
	}
	if verdict != verdictUnanswered {
		t.Fatalf("the closed hand's verdict is %q, want %q", verdict, verdictUnanswered)
	}
}

// A hand that failed to reproduce is not paid out, even though its challenge is
// closed and nobody can be named for it.
//
// The closed challenge is right: every reveal is in and nobody owes anything, so
// nobody is accused. What must not follow is the payout - there is no bond to
// take for an unattributed break, so refusing to settle is the whole of the
// answer, and each seat's stake goes back the way it always can.
func TestAHandThatDidNotReproduceIsNotPaidOut(t *testing.T) {
	h := newHub(t)
	a, b, terms := dealingTable(t, h)
	waitBetting(t, a, b)

	sayWhereToPay(t, h, a, b)
	playHand(t, h, terms.SID, checkOrCall, a, b)
	waitSettled(t, terms.SID, 1, a, b)
	getUp(t, h, terms.SID, a)
	waitOver(t, h, terms.SID, a, b)

	a.tables.mu.Lock()
	defer a.tables.mu.Unlock()
	atbl := a.tables.m[terms.SID]

	// The verdict a failed recomputation leaves, with no challenge open and no
	// seat named: exactly the state the audit reaches on a *deck.Wrong.
	atbl.judged = map[uint64]string{1: verdictWrong}
	if atbl.challengeOpen() {
		t.Fatal("this is meant to test a closed challenge")
	}
	if !atbl.resultInDoubt() {
		t.Fatal("a hand that did not reproduce leaves the result settled")
	}
	if out := atbl.proposeSettlement(); len(out) != 0 {
		t.Fatal("a table with a hand that did not reproduce proposed a payout")
	}
	if out := atbl.proposeReleases(); len(out) != 0 {
		t.Fatal("a table with a hand that did not reproduce released a bond")
	}

	// An inconclusive audit is the other case and must not block anything: it
	// says nothing about the hand, only that this peer could not finish asking.
	atbl.judged = map[uint64]string{1: verdictInconclusive}
	if atbl.resultInDoubt() {
		t.Fatal("an audit that could not run is holding the table's money")
	}
	if out := atbl.proposeSettlement(); len(out) == 0 {
		t.Fatal("an inconclusive audit blocked the payout")
	}
}

// A named cheat holds the payout too. Withholding only that seat's bond while
// paying out the stacks its rigged hand produced answers the smaller half.
func TestAProvenCheatIsNotPaidOutEither(t *testing.T) {
	h := newHub(t)
	a, b, terms := dealingTable(t, h)
	waitBetting(t, a, b)

	sayWhereToPay(t, h, a, b)
	playHand(t, h, terms.SID, checkOrCall, a, b)
	waitSettled(t, terms.SID, 1, a, b)
	getUp(t, h, terms.SID, a)
	waitOver(t, h, terms.SID, a, b)

	a.tables.mu.Lock()
	defer a.tables.mu.Unlock()
	atbl := a.tables.m[terms.SID]
	atbl.judged = map[uint64]string{1: verdictCheat}
	atbl.cheats = map[uint32]bool{theirSeat(t, atbl): true}

	if out := atbl.proposeSettlement(); len(out) != 0 {
		t.Fatal("a table with a proven crooked hand proposed a payout")
	}
	if out := atbl.proposeReleases(); len(out) != 0 {
		t.Fatal("a table with a proven crooked hand released a bond")
	}
}
