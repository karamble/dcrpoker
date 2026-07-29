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
}

export function OnChain({
  ledger,
  dealing,
  refreshed,
}: {
  ledger?: LedgerView
  dealing?: boolean
  refreshed?: boolean
}) {
  const events = [...(ledger?.events ?? [])].reverse()
  const claims = ledger?.claims ?? []
  const settlement = ledger?.settlement

  return (
    <section className="card">
      <h2>On the chain</h2>

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

      {dealing && !refreshed && (
        <p className="lede warn">
          The answers to a future claim are still being gathered. Until every seat holds
          one, a seat claimed against cannot answer — the signatures an answer needs
          include the accusers', and they will not give them once they have started
          accusing.
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
                <span className={e.kind === 'unanswerable' ? 'alarm' : undefined}>
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
