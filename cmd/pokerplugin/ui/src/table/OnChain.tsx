import type { ChainEvent, LedgerView } from '../api'
import { dcr, short } from '../format'

// What happened on the chain, and why.
//
// Every line here used to be a log.Printf inside a container with no shell.
//
// The rule this screen follows, and the one it would be easiest to get wrong:
// **a claim is reported, never prompted.** When somebody claims this seat's
// bond, the plugin broadcasts a pre-agreed answer immediately and asks nobody -
// the answer was signed while everybody was still cooperating, because it could
// not be gathered afterwards. So there is no dialog to build, no countdown, and
// no decision to offer. By the time a person could read this, it has happened.
//
// The one line that is allowed to alarm is `unanswerable`: claimed against, and
// no answer held. That is a bond actually going, and it is the only red thing
// in this interface.

const wording: Record<ChainEvent['kind'], string> = {
  proposed: 'Claim opened',
  cosigned: 'Claim co-signed',
  refused: 'Refused to sign',
  claimed: 'Bond taken',
  answered: 'Claim answered',
  unanswerable: 'No answer held',
  settled: 'Table paid out',
  blocked: 'Could not complete',
  challenged: 'Hand challenged',
  audited: 'Hand recomputed clean',
  cheat: 'Cheating proven',
  wrong: 'Hand did not reproduce',
}

export function OnChain({
  ledger,
  dealing,
  accused,
}: {
  ledger?: LedgerView
  dealing?: boolean
  accused?: boolean
}) {
  const events = [...(ledger?.events ?? [])].reverse()
  const claims = ledger?.claims ?? []
  const settlement = ledger?.settlement
  const challenges = ledger?.challenges ?? []
  const disputes = ledger?.disputes ?? []

  return (
    <section className="card">
      <h2>On the chain</h2>

      {disputes.map((d) => (
        <div key={`dp${d.hand}`} className="banner">
          <div>
            <strong className="alarm">
              Hand {d.hand}'s shuffle was disputed — seat {d.named}{' '}
              {d.named === d.shuffler
                ? 'signed a shuffle that does not verify'
                : 'refused a shuffle that verifies'}
              .
            </strong>{' '}
            Every peer reaches this verdict from the complaint alone. The hand is void,
            the table settles at the last signed boundary, and seat {d.named}'s bond
            release is withheld.
          </div>
        </div>
      ))}

      {challenges.map((ch) => (
        <div
          key={`ch${ch.hand}`}
          className={ch.verdict === 'cheat' || ch.verdict === 'wrong' ? 'banner' : 'banner calm'}
        >
          {ch.open ? (
            <>
              <div>
                <strong>Hand {ch.hand} is challenged.</strong> Every seat owes its deck
                secrets, and nothing gets paid out until the hand is recomputed.
              </div>
              <div className="event-meta">
                {ch.revealed?.length ?? 0} of {ch.needs} seats have revealed
              </div>
            </>
          ) : ch.verdict === 'wrong' ? (
            <div>
              <strong className="alarm">Hand {ch.hand} did not reproduce.</strong>{' '}
              Every seat's secrets matched what that seat published, and the hand still
              does not recompute to the result it was played as. Nobody can be named for
              it, so nothing settles on this table and each stake goes back to whoever
              paid it.
            </div>
          ) : ch.verdict === 'cheat' ? (
            <div>
              <strong className="alarm">
                Hand {ch.hand} did not recompute{ch.cheatSeat !== undefined ? ` — seat ${ch.cheatSeat} broke it` : ''}.
              </strong>{' '}
              The revealed secrets do not produce the deck that was played.
            </div>
          ) : (
            <>
              <div>
                <strong>Hand {ch.hand} recomputed clean.</strong> Every card, from every
                seat's own secrets, with no proof consulted — folds included, which is
                the price of being the hand somebody doubted.
              </div>
              {ch.cards && ch.cards.length > 0 && (
                <div className="event-meta">
                  {ch.cards
                    .filter((c) => c.board || c.seat !== undefined)
                    .map((c) =>
                      c.board ? `board ${c.card}` : `seat ${c.seat} ${c.card}`,
                    )
                    .join(' · ')}
                </div>
              )}
            </>
          )}
        </div>
      ))}

      {claims.length > 0 && (
        <>
          {claims.map((c, i) => (
            <div key={i} className={c.mine ? 'banner' : 'banner calm'}>
              <div>
                {c.mine ? (
                  <>
                    <strong>Seat {c.against} — that is you — is being claimed against.</strong>{' '}
                    Your answer was agreed in advance and is broadcast without asking. There
                    is nothing to decide here.
                  </>
                ) : (
                  <>
                    <strong>A claim is open against seat {c.against}.</strong>{' '}
                    {c.ours ? 'This peer has signed it.' : 'This peer has not signed it.'}
                  </>
                )}
              </div>
              <div className="event-meta">
                {c.says} · {c.signed} of {c.needs} signatures
                {c.atomsEach ? ` · ${dcr(c.atomsEach)} DCR each if it lands` : ''}
                {c.done ? ' · sent' : ''}
              </div>
            </div>
          ))}
        </>
      )}

      {settlement && (settlement.done || settlement.blocked || (settlement.signed?.length ?? 0) > 0) && (
        <div className="rows">
          <div className="row">
            <span>Payout at hand {settlement.hand}</span>
            <span>
              {settlement.done
                ? 'sent'
                : `${settlement.signed?.length ?? 0} of ${settlement.needs} signed`}
            </span>
          </div>
          {settlement.blocked && (
            <div className="row">
              <span>Not yet, because</span>
              <span className="warn">{settlement.blocked}</span>
            </div>
          )}
          {settlement.pays?.map((p) => (
            <div className="row" key={p.seat}>
              <span>seat {p.seat}</span>
              <span>
                {dcr(p.atoms)} DCR{p.address ? ` → ${short(p.address, 5)}` : ''}
              </span>
            </div>
          ))}
        </div>
      )}

      {dealing && !accused && (
        <p className="lede warn">
          The table is still agreeing what to do about a seat that stops. Until every
          seat has signed, a seat that goes quiet cannot be answered for at all - an
          accusation needs the signature of the seat it accuses, and nobody would give
          that once they were being accused. Answering one needs only your own key.
        </p>
      )}

      {events.length === 0 ? (
        <p className="empty">
          Nothing has happened on the chain at this table beyond the stakes and bonds
          already shown.
        </p>
      ) : (
        <ul className="events">
          {events.map((e, i) => (
            <li className="event" key={i}>
              <span className={`pip ${e.kind}`} />
              <span className="event-text">
                <span className={e.kind === 'unanswerable' || e.kind === 'cheat' || e.kind === 'wrong' ? 'alarm' : undefined}>
                  {wording[e.kind] ?? e.kind}
                </span>
                {' — '}
                {e.text}
                <span className="event-meta">
                  {e.at > 0 ? ` hand ${e.at}` : ''}
                  {e.txid ? (
                    <>
                      {' '}
                      · <span className="mono" title={e.txid}>{short(e.txid)}</span>
                    </>
                  ) : null}
                </span>
              </span>
            </li>
          ))}
        </ul>
      )}
    </section>
  )
}
