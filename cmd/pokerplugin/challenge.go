package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"

	"go.dedis.ch/kyber/v4"

	"github.com/vctt94/pokerbisonrelay/pkg/deck"
	"github.com/vctt94/pokerbisonrelay/pkg/driver"
	"github.com/vctt94/pokerbisonrelay/pkg/forfeit"
	"github.com/vctt94/pokerbisonrelay/pkg/gaming/schema"
	gwire "github.com/vctt94/pokerbisonrelay/pkg/gaming/wire"
	"github.com/vctt94/pokerbisonrelay/pkg/replay"
)

// Challenging a hand, and answering for one.
//
// Every proof in the dealing is verified the moment it arrives, and all of it
// rests on a library that has shipped an unsound proof before. The audit is the
// answer that owes the proofs nothing: any seat may demand a settled hand be
// recomputed from every seat's own secrets, and the recomputation either lands
// where the hand said it did or names the seat it did not land for.
//
// Honest play reveals nothing - that is the whole design, because revealing
// every hand would publish the muck forever. A challenge is scoped to one hand,
// obliges every seat at it, and is enforced twice over: refusing to reveal is a
// standing duty the pre-signed accusation answers, and while any challenge is
// open this peer signs no settlement and no release, so there is nothing to be
// paid until the table has answered for itself.
//
// A challenged hand gives up its muck to the table that doubted it. That is the
// price of being the hand somebody doubted, and it is paid by the challenger's
// own cards too.
//
// A hand is answered for once. Every way a challenge can end is recorded as a
// verdict and the hand is not reopened, because the settlement gate is what
// makes an unbounded challenge worth something: each round of challenge, reveal,
// clean, close would hold the table's money for a round trip while the
// challenger owed nothing anybody could claim against - it discharges its own
// reveal every time. One challenge per hand played is finite, and a losing
// player who would rather everybody recovered their deposit than settle has
// nothing to repeat.

// The verdicts a challenge can end on. Every one of them is final: a hand that
// has been answered for is not challenged again.
const (
	verdictClean = "clean"
	verdictCheat = "cheat"
	// verdictWrong is a replay that produced something that is not a deck,
	// with nobody to name for it. Nothing is paid out on one: there is no bond
	// to take, so refusing to settle is the whole of the answer - and every
	// peer holding the published material reaches it alike, which is what
	// makes refusing legible rather than suspicious.
	verdictWrong = "wrong"
	// verdictUnanswered is a hand nobody can recompute any more, because every
	// seat still owing it has had its bond taken instead.
	verdictUnanswered = "unanswered"
	// verdictInconclusive is this peer being unable to complete the question:
	// a transcript with a piece missing, or a local record that contradicts
	// arithmetic it cannot fault. It says nothing about the hand and holds
	// nothing up, because a finding no other peer can reach must not be able
	// to hold everybody's money.
	verdictInconclusive = "inconclusive"
)

// handBundle is one settled hand held ready to answer for: the transcript as
// this peer verified it, its own secrets, and whatever others have revealed.
type handBundle struct {
	hand     *deck.Hand
	own      *deck.Secrets
	view     *schema.HandRecordView
	revealed map[uint32]*deck.Secrets
	// cards is the audited hand, set only by a clean audit.
	cards []deck.Card
}

// harvestHands writes down every hand the driver has finished.
//
// Runs before the checkpoint that settles a hand leaves the process: the
// signature is what makes the hand challengeable, and this file is what
// answers. A hand that cannot be written down is said out loud, because its
// owner is one challenge away from an accusation nothing can answer.
func (tbl *table) harvestHands() {
	if tbl.play == nil {
		return
	}
	recs := tbl.play.TakeFinishedHands()
	if len(recs) == 0 {
		return
	}
	mine, ok := tbl.form.OurSeat()
	if !ok {
		return
	}
	for _, rec := range recs {
		sig, err := tbl.signSecrets(rec.Hand.Hand, rec.Secrets)
		if err != nil {
			log.Printf("pokerplugin: table %s: cannot sign hand %d's secrets: %v",
				tbl.terms.SID, rec.Hand.Hand, err)
			tbl.note(eventBlocked, fmt.Sprintf(
				"hand %d's secrets could not be signed, and a challenge on it cannot be answered",
				rec.Hand.Hand), "", nil)
			continue
		}
		view, err := schema.HandRecordFrom(rec, mine, sig)
		if err != nil {
			log.Printf("pokerplugin: table %s: cannot render hand %d: %v",
				tbl.terms.SID, rec.Hand.Hand, err)
			continue
		}
		b := &handBundle{hand: rec.Hand, own: rec.Secrets, view: view,
			revealed: map[uint32]*deck.Secrets{}}
		if tbl.bundles == nil {
			tbl.bundles = map[uint64]*handBundle{}
		}
		tbl.bundles[rec.Hand.Hand] = b
		if err := tbl.saveBundle(rec.Hand.Hand, b); err != nil {
			log.Printf("pokerplugin: table %s: cannot write down hand %d: %v",
				tbl.terms.SID, rec.Hand.Hand, err)
			tbl.note(eventBlocked, fmt.Sprintf(
				"hand %d could not be written down, and a challenge on it cannot be answered after a restart",
				rec.Hand.Hand), "", nil)
		}
	}
}

func (tbl *table) saveBundle(hand uint64, b *handBundle) error {
	if tbl.st == nil {
		return nil
	}
	blob, err := json.Marshal(b.view)
	if err != nil {
		return err
	}
	return tbl.st.saveHand(tbl.terms.SID, hand, blob)
}

// bundle is the hand, from memory or from disk.
func (tbl *table) bundle(hand uint64) *handBundle {
	if b, ok := tbl.bundles[hand]; ok {
		return b
	}
	if tbl.st == nil {
		return nil
	}
	blob, err := tbl.st.loadHand(tbl.terms.SID, hand)
	if err != nil || blob == nil {
		return nil
	}
	var view schema.HandRecordView
	if err := json.Unmarshal(blob, &view); err != nil {
		log.Printf("pokerplugin: table %s: hand %d's record is unreadable: %v",
			tbl.terms.SID, hand, err)
		return nil
	}
	h, own, err := view.Into()
	if err != nil {
		log.Printf("pokerplugin: table %s: hand %d's record does not decode: %v",
			tbl.terms.SID, hand, err)
		return nil
	}
	b := &handBundle{hand: h, own: own, view: &view, revealed: map[uint32]*deck.Secrets{}}
	for seat, sv := range view.Revealed {
		_, _, sec, _, err := sv.Into()
		if err != nil {
			continue
		}
		b.revealed[seat] = sec
	}
	if tbl.bundles == nil {
		tbl.bundles = map[uint64]*handBundle{}
	}
	tbl.bundles[hand] = b
	return b
}

// logKeyFor re-derives this seat's signing key, with its book, for a table
// whose driver may no longer exist.
func (tbl *table) logKeyFor() (*forfeit.LogKey, error) {
	match, ok := tbl.form.MatchID()
	if !ok {
		return nil, fmt.Errorf("this table has no settled match")
	}
	if tbl.logPriv == nil {
		return nil, fmt.Errorf("this table has no log key")
	}
	key, err := forfeit.LogKeyFrom(tbl.logPriv, match)
	if err != nil {
		return nil, err
	}
	key.Remember(&signBook{tbl: tbl})
	return key, nil
}

func (tbl *table) signSecrets(hand uint64, sec *deck.Secrets) ([]byte, error) {
	match, ok := tbl.form.MatchID()
	if !ok {
		return nil, fmt.Errorf("this table has no settled match")
	}
	mine, ok := tbl.form.OurSeat()
	if !ok {
		return nil, fmt.Errorf("this table holds no seat of ours")
	}
	digest, err := driver.SecretsDigest(match, hand, int(mine), sec)
	if err != nil {
		return nil, err
	}
	key, err := tbl.logKeyFor()
	if err != nil {
		return nil, err
	}
	return key.SignCommitted(forfeit.DomainSecrets, hand, digest[:])
}

func (tbl *table) signChallenge(hand uint64) ([]byte, error) {
	match, ok := tbl.form.MatchID()
	if !ok {
		return nil, fmt.Errorf("this table has no settled match")
	}
	mine, ok := tbl.form.OurSeat()
	if !ok {
		return nil, fmt.Errorf("this table holds no seat of ours")
	}
	digest := driver.ChallengeDigest(match, hand, int(mine))
	key, err := tbl.logKeyFor()
	if err != nil {
		return nil, err
	}
	return key.Sign(forfeit.DomainChallenge, hand, digest[:])
}

// challengeHand is this seat demanding a settled hand be recomputed.
func (tbl *table) challengeHand(hand uint64) ([]outgoing, error) {
	mine, ok := tbl.form.OurSeat()
	if !ok {
		return nil, fmt.Errorf("this table holds no seat of ours")
	}
	for open, by := range tbl.openChal {
		if by == mine && open != hand {
			return nil, fmt.Errorf("hand %d is already challenged; one at a time", open)
		}
	}
	if tbl.bundle(hand) == nil {
		return nil, fmt.Errorf("hand %d is not settled here, and only a settled hand can be challenged", hand)
	}
	if err := tbl.recordChallenge(hand, mine); err != nil {
		return nil, err
	}
	sig, err := tbl.signChallenge(hand)
	if err != nil {
		return nil, err
	}
	log.Printf("pokerplugin: table %s: challenging hand %d", tbl.terms.SID, hand)
	tbl.note(eventChallenged, fmt.Sprintf(
		"hand %d is challenged; every seat owes its deck secrets", hand), "", seatp(int(mine)))
	out := []outgoing{tbl.frame(schema.KindChallenge,
		schema.ChallengeFrom(mine, hand, sig), gwire.ClassDispute)}
	return append(out, tbl.answerChallenges()...), nil
}

// recordChallenge files an open challenge, in memory, in the driver when one is
// alive, and in the record.
func (tbl *table) recordChallenge(hand uint64, by uint32) error {
	if v := tbl.judged[hand]; v != "" {
		return fmt.Errorf("hand %d has already been answered for (%s), and is not challenged again", hand, v)
	}
	if tbl.play != nil {
		if err := tbl.play.OpenChallenge(hand, int(by)); err != nil {
			return err
		}
	}
	if tbl.openChal == nil {
		tbl.openChal = map[uint64]uint32{}
	}
	if _, open := tbl.openChal[hand]; !open {
		tbl.openChal[hand] = by
	}
	_ = tbl.save()
	return nil
}

// acceptChallenge is somebody demanding a hand this peer played be recomputed.
func (tbl *table) acceptChallenge(body schema.Challenge) []outgoing {
	seat, hand, sig, err := body.Into()
	if err != nil {
		return nil
	}
	match, ok := tbl.form.MatchID()
	if !ok {
		return nil
	}
	logSeats, ok := tbl.form.LogSeats()
	if !ok {
		return nil
	}
	key, ok := logSeats[seat]
	if !ok {
		return nil
	}
	digest := driver.ChallengeDigest(match, hand, int(seat))
	if err := driver.VerifySeatSig(key, digest, sig, int(seat)); err != nil {
		log.Printf("pokerplugin: table %s: a challenge that seat %d did not sign: %v",
			tbl.terms.SID, seat, err)
		return nil
	}
	if _, open := tbl.openChal[hand]; open {
		return tbl.answerChallenges()
	}
	for _, by := range tbl.openChal {
		if by == seat {
			log.Printf("pokerplugin: table %s: seat %d already has a challenge open; one at a time",
				tbl.terms.SID, seat)
			return nil
		}
	}
	if tbl.bundle(hand) == nil {
		log.Printf("pokerplugin: table %s: hand %d challenged, and this peer holds no record of it",
			tbl.terms.SID, hand)
		tbl.note(eventBlocked, fmt.Sprintf(
			"hand %d is challenged and this peer holds no record of it", hand), "", seatp(int(seat)))
		return nil
	}
	if err := tbl.recordChallenge(hand, seat); err != nil {
		log.Printf("pokerplugin: table %s: refusing seat %d's challenge of hand %d: %v",
			tbl.terms.SID, seat, hand, err)
		return nil
	}
	tbl.note(eventChallenged, fmt.Sprintf(
		"hand %d is challenged; every seat owes its deck secrets", hand), "", seatp(int(seat)))
	return tbl.answerChallenges()
}

// answerChallenges reveals this seat's secrets for every open challenge.
//
// Repeated every tick until the challenge closes, byte-identical, and stopped
// by that rather than by having said it once - this channel loses messages,
// and what is being repeated is the answer to an accusation.
//
// Deliberately not gated on finished, unlike every other repeat in tick. A
// challenge outlives the table the way the bond does: openChal is written down
// and restored with the receipt, a restarted peer still owes its reveal, and a
// seat that stops answering because it got up is exactly the refusal a
// challenge makes claimable. Do not add the gate here.
func (tbl *table) answerChallenges() []outgoing {
	if len(tbl.openChal) == 0 {
		return nil
	}
	mine, ok := tbl.form.OurSeat()
	if !ok {
		return nil
	}
	var out []outgoing
	for hand := range tbl.openChal {
		b := tbl.bundle(hand)
		if b == nil || b.own == nil || b.view == nil || b.view.Own == nil {
			tbl.note(eventBlocked, fmt.Sprintf(
				"hand %d is challenged and this peer's own secrets for it are gone", hand),
				"", seatp(int(mine)))
			continue
		}
		if tbl.play != nil {
			tbl.play.NoteRevealed(hand, int(mine))
		}
		out = append(out, tbl.frame(schema.KindSecrets, *b.view.Own, gwire.ClassDispute))
	}
	return out
}

// repeatChallenges says this seat's own open challenges again, byte-identical.
func (tbl *table) repeatChallenges() []outgoing {
	mine, ok := tbl.form.OurSeat()
	if !ok {
		return nil
	}
	var out []outgoing
	for hand, by := range tbl.openChal {
		if by != mine {
			continue
		}
		sig, err := tbl.signChallenge(hand)
		if err != nil {
			continue
		}
		out = append(out, tbl.frame(schema.KindChallenge,
			schema.ChallengeFrom(mine, hand, sig), gwire.ClassDispute))
	}
	return out
}

// acceptSecrets takes one seat's reveal, refuses what does not check, and runs
// the audit when the set completes.
func (tbl *table) acceptSecrets(body schema.Secrets) []outgoing {
	seat, hand, sec, sig, err := body.Into()
	if err != nil {
		return nil
	}
	mine, ok := tbl.form.OurSeat()
	if !ok || seat == mine {
		return nil
	}
	match, ok := tbl.form.MatchID()
	if !ok {
		return nil
	}
	logSeats, ok := tbl.form.LogSeats()
	if !ok {
		return nil
	}
	key, ok := logSeats[seat]
	if !ok {
		return nil
	}
	digest, err := driver.SecretsDigest(match, hand, int(seat), sec)
	if err != nil {
		return nil
	}
	if err := driver.VerifySeatSig(key, digest, sig, int(seat)); err != nil {
		log.Printf("pokerplugin: table %s: secrets for hand %d that seat %d did not sign: %v",
			tbl.terms.SID, hand, seat, err)
		return nil
	}
	b := tbl.bundle(hand)
	if b == nil {
		return nil
	}
	if _, held := b.revealed[seat]; held {
		return nil
	}

	err = deck.VerifySecrets(b.hand, int(seat), sec)
	var cheat *deck.Cheat
	if errors.As(err, &cheat) {
		// Signed secrets that do not produce the deck the same seat also
		// signed. That is the proof, and there is nothing left to wait for.
		log.Printf("pokerplugin: table %s: hand %d: %v", tbl.terms.SID, hand, err)
		tbl.note(eventCheat, fmt.Sprintf("hand %d: seat %d %s", hand, seat, cheat.Reason),
			"", seatp(int(seat)))
		if tbl.cheats == nil {
			tbl.cheats = map[uint32]bool{}
		}
		tbl.cheats[seat] = true
		tbl.closeChallenge(hand, verdictCheat)
		return nil
	}
	if err != nil {
		log.Printf("pokerplugin: table %s: hand %d: seat %d's reveal does not check: %v",
			tbl.terms.SID, hand, seat, err)
		return nil
	}

	b.revealed[seat] = sec
	if b.view.Revealed == nil {
		b.view.Revealed = map[uint32]schema.Secrets{}
	}
	b.view.Revealed[seat] = body
	if err := tbl.saveBundle(hand, b); err != nil {
		log.Printf("pokerplugin: table %s: cannot keep seat %d's reveal: %v", tbl.terms.SID, seat, err)
	}
	if tbl.play != nil {
		tbl.play.NoteRevealed(hand, int(seat))
	}
	tbl.maybeAudit(hand)
	return nil
}

// maybeAudit recomputes the hand once every seat's secrets are in.
func (tbl *table) maybeAudit(hand uint64) {
	b := tbl.bundles[hand]
	if b == nil || b.own == nil {
		return
	}
	if _, open := tbl.openChal[hand]; !open {
		return
	}
	mine, ok := tbl.form.OurSeat()
	if !ok {
		return
	}
	n := len(b.hand.Pubs)
	secrets := make([]*deck.Secrets, n)
	for seat := range n {
		if uint32(seat) == mine {
			secrets[seat] = b.own
			continue
		}
		sec, held := b.revealed[uint32(seat)]
		if !held {
			return // still short of somebody
		}
		secrets[seat] = sec
	}

	cards, err := deck.AuditedDeck(b.hand, secrets)
	verdict := verdictInconclusive
	var cheat *deck.Cheat
	var wrong *deck.Wrong
	switch {
	case err == nil:
		verdict = verdictClean
		b.cards = cards
		log.Printf("pokerplugin: table %s: hand %d recomputed clean", tbl.terms.SID, hand)
		tbl.note(eventAudited, fmt.Sprintf(
			"hand %d recomputed clean from every seat's secrets; every proof told the truth", hand),
			"", nil)
	case errors.As(err, &cheat):
		verdict = verdictCheat
		seat := seatOfPub(b.hand.Pubs, cheat.By)
		log.Printf("pokerplugin: table %s: hand %d: %v", tbl.terms.SID, hand, err)
		tbl.note(eventCheat, fmt.Sprintf("hand %d: %s", hand, cheat.Reason), "", seat)
		if seat != nil {
			if tbl.cheats == nil {
				tbl.cheats = map[uint32]bool{}
			}
			tbl.cheats[uint32(*seat)] = true
		}
	case errors.As(err, &wrong):
		// The audit ran and disagreed, and no seat's own secrets account for
		// it. Nobody's bond can answer that, so the only answer left is that
		// this hand is not paid out.
		verdict = verdictWrong
		log.Printf("pokerplugin: table %s: hand %d did not reproduce: %v", tbl.terms.SID, hand, err)
		tbl.note(eventWrong, fmt.Sprintf(
			"hand %d did not reproduce and no seat can be named for it: %s; nothing settles on it",
			hand, wrong.Reason), "", nil)
	default:
		// Not "the audit could not run" - it may well have run and found this
		// peer's own record of the hand at fault, which is a different thing
		// to say to whoever is reading it.
		log.Printf("pokerplugin: table %s: hand %d could not be answered for: %v",
			tbl.terms.SID, hand, err)
		tbl.note(eventBlocked, fmt.Sprintf(
			"hand %d could not be answered for, and nothing is held up by it: %v", hand, err),
			"", nil)
	}
	tbl.closeChallenge(hand, verdict)
}

// seatOfPub finds which seat committed a card key.
func seatOfPub(pubs []kyber.Point, who kyber.Point) *uint32 {
	if who == nil {
		return nil
	}
	for i, p := range pubs {
		if p != nil && p.Equal(who) {
			s := uint32(i)
			return &s
		}
	}
	return nil
}

// closeChallenge ends one challenge and records what answered it.
//
// The verdict is what stops a hand being challenged for ever. A hand that has
// been recomputed, or proven crooked, or paid for out of somebody's bond, has
// had its answer - and reopening it would cost the table another round of
// blocked settlement for nothing, which is a grief with no duty attached to it
// because the challenger discharges its own reveal every time.
func (tbl *table) closeChallenge(hand uint64, verdict string) {
	delete(tbl.openChal, hand)
	if tbl.judged == nil {
		tbl.judged = map[uint64]string{}
	}
	tbl.judged[hand] = verdict
	if tbl.play != nil {
		tbl.play.CloseChallenge(hand)
	}
	_ = tbl.save()
}

// outstanding is every seat a challenged hand is still waiting on.
func (tbl *table) outstanding(hand uint64) []uint32 {
	b := tbl.bundles[hand]
	if b == nil {
		return nil
	}
	mine, haveMine := tbl.form.OurSeat()
	var out []uint32
	for seat := range len(b.hand.Pubs) {
		s := uint32(seat)
		if haveMine && s == mine {
			if b.view != nil && b.view.Own != nil {
				continue
			}
			out = append(out, s)
			continue
		}
		if _, held := b.revealed[s]; !held {
			out = append(out, s)
		}
	}
	return out
}

// closeChallengesAfterTake ends the challenges a forfeited bond has answered.
//
// Only the hands whose every remaining reveal belongs to a seat whose bond has
// been taken: nothing more can arrive for those, so their audit can never
// complete and holding the table's money against them would be holding it for
// ever. A hand another seat is also short on stays open, and that seat stays
// owing - one refuser being paid for must not launder the rest of them.
func (tbl *table) closeChallengesAfterTake(seat uint32) {
	if tbl.forfeited == nil {
		tbl.forfeited = map[uint32]bool{}
	}
	tbl.forfeited[seat] = true

	for hand := range tbl.openChal {
		left := tbl.outstanding(hand)
		if len(left) == 0 {
			// Nothing is missing, so the audit is what closes this.
			continue
		}
		paid := true
		for _, s := range left {
			if !tbl.forfeited[s] {
				paid = false
				break
			}
		}
		if !paid {
			log.Printf("pokerplugin: table %s: hand %d stays challenged; seat %d's bond answered for "+
				"it but %v have still not revealed", tbl.terms.SID, hand, seat, left)
			continue
		}
		log.Printf("pokerplugin: table %s: hand %d's challenge closes; every seat still owing it "+
			"has had its bond taken", tbl.terms.SID, hand)
		tbl.note(eventClaimed, fmt.Sprintf(
			"hand %d was never recomputed, and every seat that refused has paid its bond for it",
			hand), "", seatp(int(seat)))
		tbl.closeChallenge(hand, verdictUnanswered)
	}
}

// challengeOpen reports whether any hand here is challenged and unanswered.
// While one is, this peer signs no settlement and no release: nothing gets
// paid until the table has answered for itself.
func (tbl *table) challengeOpen() bool { return len(tbl.openChal) > 0 }

// resultInDoubt reports whether anything here must not be paid out.
//
// A challenge still waiting on reveals, or a hand that has already been
// recomputed and disagreed. The second is not the same as the challenge being
// open - every reveal is in and the duty is discharged, so nobody is accused
// any more - but a result the table itself has disproved is not one to settle
// on, and the stakes go back the way they always can: each seat's own refund,
// after its own timelock.
//
// A named cheat counts too. Withholding only that seat's bond release, while
// paying out the stacks its rigged hand produced, would be answering the smaller
// half of it.
func (tbl *table) resultInDoubt() bool {
	if tbl.challengeOpen() {
		return true
	}
	// A shuffle dispute deliberately holds nothing here: its verdict is
	// instant and local - the complaint carries everything - so there is no
	// waiting state, and the verdicts do not join the loop below because a
	// dispute's hand moved no money and is voided. The boundary being paid
	// predates the wedge, and the named seat is answered by its withheld
	// bond release instead.
	for _, v := range tbl.judged {
		if v == verdictWrong || v == verdictCheat {
			return true
		}
	}
	return false
}

// caughtCheating reports whether the audit named this seat.
func (tbl *table) caughtCheating(seat uint32) bool { return tbl.cheats[seat] }

// challengeViews is every challenge this table has seen, for the panel.
// Requires the registry lock.
func (tbl *table) challengeViews() []challengeView {
	if len(tbl.openChal) == 0 && len(tbl.judged) == 0 {
		return nil
	}
	mine, haveMine := tbl.form.OurSeat()
	var out []challengeView
	seen := map[uint64]bool{}
	add := func(hand uint64, by uint32, open bool) {
		if seen[hand] {
			return
		}
		seen[hand] = true
		cv := challengeView{Hand: hand, By: by, Open: open}
		cv.Verdict = tbl.judged[hand]
		b := tbl.bundles[hand]
		if b != nil {
			cv.Needs = len(b.hand.Pubs)
			if haveMine && b.view != nil && b.view.Own != nil {
				cv.Revealed = append(cv.Revealed, mine)
			}
			for seat := range b.revealed {
				cv.Revealed = append(cv.Revealed, seat)
			}
			sort.Slice(cv.Revealed, func(i, j int) bool { return cv.Revealed[i] < cv.Revealed[j] })
			if b.cards != nil {
				cv.Cards = auditedCards(b, len(b.hand.Pubs))
			}
		}
		if cv.Verdict == verdictCheat {
			for seat := range tbl.cheats {
				s := seat
				cv.CheatSeat = &s
				break
			}
		}
		out = append(out, cv)
	}
	for hand, by := range tbl.openChal {
		add(hand, by, true)
	}
	// Closed ones still worth showing: every hand already answered for.
	for hand := range tbl.judged {
		add(hand, 0, false)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Hand < out[j].Hand })
	return out
}

// auditedCards lays the recomputed hand out with each slot's role, the way the
// driver dealt it.
func auditedCards(b *handBundle, seats int) []audCard {
	layout := replay.Layout{Seats: seats}
	board := map[int]bool{}
	if slots, err := layout.Board(); err == nil {
		for _, s := range slots {
			board[s] = true
		}
	}
	owner := map[int]uint32{}
	for seat := range seats {
		if slots, err := layout.Hole(seat); err == nil {
			for _, s := range slots {
				owner[s] = uint32(seat)
			}
		}
	}
	out := make([]audCard, 0, len(b.cards))
	for slot, card := range b.cards {
		c := audCard{Slot: slot, Card: card.String(), Board: board[slot]}
		if seat, ok := owner[slot]; ok {
			s := seat
			c.Seat = &s
		}
		out = append(out, c)
	}
	return out
}

// handleTableChallenge is the panel demanding a hand be recomputed.
func (p *plugin) handleTableChallenge(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		SID  string `json:"sid"`
		Hand uint64 `json:"hand"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "decode body: "+err.Error(), http.StatusBadRequest)
		return
	}
	sid := strings.ToLower(strings.TrimSpace(req.SID))

	p.tables.mu.Lock()
	tbl := p.tables.m[sid]
	if tbl == nil {
		p.tables.mu.Unlock()
		writeErr(w, http.StatusBadRequest, fmt.Errorf("not at a table under session %s", sid))
		return
	}
	out, err := tbl.challengeHand(req.Hand)
	p.tables.mu.Unlock()
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	p.publish(r.Context(), out)
	p.notifyNow()
	writeJSON(w, map[string]any{"sid": sid, "hand": req.Hand, "challenged": true})
}
