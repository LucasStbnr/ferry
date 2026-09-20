# Architecture

Ferry is one binary. This is what is inside it and why.

## The problem

Resend is an append-only archive with an HTTP API. A mail client expects an
IMAP server. The gap is not the protocol; it is the state:

| A mail client needs | Resend has |
|---|---|
| Read / unread | No |
| Folders | No |
| Drafts | No |
| Delete | No |
| Stable per-mailbox UIDs | No |
| Flags and keywords | No |
| The raw message, forever | A signed URL that expires |

Everything in the left column has to live somewhere. That somewhere is Ferry's
local store, and it is the central design decision: **the local store is the
source of truth, and the sync is one-directional.**

## Packages

```
cmd/ferry/              entry point
internal/cli/           cobra commands
internal/daemon/        lifecycle: starts and stops everything together
internal/config/        paths, settings, defaults
internal/secrets/       Keychain / environment / Docker secrets / file
internal/account/       account lifecycle, app passwords, authentication
internal/resend/        API client: rate limiting, retries, error classification
internal/store/         SQLite + blobs: mailboxes, UIDs, flags, tombstones, FTS5
internal/mailmime/      RFC 5322 ⇄ Resend's structured JSON
internal/mailsync/      backfill and incremental polling
internal/imapd/         IMAP server (go-imap v2)
internal/smtpd/         SMTP submission (go-smtp)
internal/webhook/       Svix-verified event receiver
internal/tlsutil/       local CA and leaf certificate
internal/mobileconfig/  Apple configuration profile (macOS and iOS only)
internal/service/       launchd and systemd integration
internal/control/       Unix socket between the CLI and the daemon
internal/testutil/      fake Resend API for tests
```

Dependencies point one way: `store` knows nothing about IMAP, `imapd` knows
nothing about Resend, and `mailsync` is the only thing that talks to both.

## The store

One SQLite database, one row per message, and the message body on disk as a
content-addressed `.eml` blob.

Splitting the body out matters. A FETCH of flags must not read a megabyte of
HTML off disk, and a COPY of a message into another folder must not duplicate
it. Blobs are named by SHA-256 and reference-counted, so a message filed in
two mailboxes costs one file.

**UIDs.** IMAP requires per-mailbox UIDs that never repeat, even after a
message is deleted. `mailboxes.uid_next` is bumped inside the same transaction
that inserts the message, which makes allocation atomic under concurrent
appends from a sync and a client.

**Tombstones.** An expunge writes `(account_id, resend_id)` into `tombstones`.
The sync checks it before storing anything. This is the only thing standing
between "I deleted that" and "it came back an hour later", because Resend will
keep offering the message forever.

**Search.** A contentless FTS5 table whose rowid mirrors `messages.id`. User
input is quoted into phrases rather than interpreted as FTS5 syntax, so a
search for `NEAR(` is a search for that text, not a syntax error.

**Isolation.** `DB.Account()` returns an `*AccountStore`, and every query it
issues is scoped to that account id. An IMAP session holds exactly one, so
cross-account access is not a rule to remember; it is unrepresentable.

## The sync

Two passes share one cursor-paginated, newest-first listing:

- **incremental** walks from the newest row down until it reaches a message
  already stored;
- **backfill** walks from the oldest row fetched so far further into the past.

Both cursors live in `sync_state`, so an interrupted first run of a large
history resumes exactly where it stopped.

Messages are stored oldest-first, so UIDs increase with arrival time, which
is what makes a mail client's default sort match reality.

### Why the watermark holds back on failure

If fetching a message fails, the incremental watermark does **not** advance
past it. Nothing ever looks above the watermark again, so stepping over a
failure would lose that message permanently. Held back, the next poll simply
retries it; everything newer is already stored and is skipped by the dedupe
check without refetching a body. A permanently broken message costs one failed
request per poll, which is much better than a silently missing one.

The backfill cursor does advance, because it walks a fixed history exactly
once and holding it back would spin forever on one bad message. The failure is
reported, and `ferry sync --backfill` re-walks.

### Materialising a message

Resend offers the original raw message through a signed URL that expires, and
sometimes not at all. So:

1. Try the raw download: it is the message as it actually arrived.
2. If it is absent or expired, rebuild it from `html`, `text` and `headers`,
   and fetch each attachment individually.
3. If an attachment is gone too, embed a note in its place, so the message
   still records that something was attached.

Nothing is ever stored partially. That is why the backfill runs eagerly at
`account add`: the links are expiring while you read this.

## IMAP

Built on `emersion/go-imap/v2`'s `imapserver`, which is beta, so the version
is pinned and the capabilities clients depend on are covered by tests that
drive a real client: NAMESPACE, SPECIAL-USE, UIDPLUS, MOVE, IDLE,
LIST-EXTENDED, ESEARCH, SEARCHRES.

State is shared per `(account, mailbox)` across connections, so a message the
sync files reaches every idling client. Each mailbox keeps its ordered UID
list in memory (IMAP sequence numbers are positional, and recomputing them
from SQL per command would be absurd) while bodies stay on disk.

`reload()` re-reads a mailbox and diffs it against what the tracker last
announced, turning a background sync into an unsolicited `EXISTS` on an IDLE
connection.

Two subtleties that look like bugs if you remove them:

- **`QueueNumMessages` is only called when the count grew.** go-imap's tracker
  treats a count of zero as "no update" and panics on an all-zero update;
  shrinking is expressed by the EXPUNGEs already queued.
- **MOVE does not write its own EXPUNGE responses.** Every removal goes
  through the tracker, which is the only thing that knows each session's view
  of the sequence numbers. Writing them inline as well would double-report them
  to the connection that issued the MOVE and leave the tracker believing the
  messages were still there.

## SMTP

Submission only, implicit TLS, AUTH PLAIN and LOGIN. (LOGIN is not in any RFC
and `go-sasl` ships only a client for it, but several mail clients still offer
it, so Ferry implements the server side.)

The send path:

1. Parse the submitted MIME.
2. Reject a From domain the account has not verified, checking both the
   envelope and the visible header, since only the latter is what a recipient
   sees.
3. Refuse anything Resend's structured API would silently destroy, such as an
   S/MIME signature.
4. Map to the send payload, carrying `In-Reply-To` and `References` so
   threading survives, and derive Bcc from the envelope recipients, since
   clients strip Bcc from the message body.
5. Send with an `Idempotency-Key` derived from the message, so a retry after a
   timeout cannot send twice.
6. File the submitted bytes verbatim in Sent, tagged with the Resend id so the
   next sync recognises rather than duplicates it.

Errors map onto SMTP codes by whether retrying can help: quota is permanent
(5xx), rate limiting and API outages are temporary (4xx). The message text is
user-facing copy; clients show it verbatim in the outbox.

## TLS

Ferry generates a private CA and a leaf for `localhost` and `127.0.0.1` on
first run, and `ferry trust` adds the CA to the system trust store. The leaf lasts
825 days, the maximum Apple platforms accept, and renews automatically when it
is within 30 days of expiry.

There is no cleartext mode, not even on loopback: an app password on a local
socket is still a password any process on the machine could read.

## Running as a service

`ferry service` writes and controls a per-user LaunchAgent on macOS or a
`systemctl --user` unit on Linux. Ferry does this itself rather than relying on
`brew services`, which only works for Homebrew formulae, and a formula is the
wrong artifact for a pre-built binary, which is why Homebrew and GoReleaser
both point at casks now. Doing it in the binary means the service works the
same however Ferry was installed.

Per-user, not system-wide: the data directory is in the user's home and the
API keys are in the user's login keychain, neither of which a root daemon
could reach.

On launchd the agent is registered with `KeepAlive`, so stopping it means
booting it out of the domain rather than signalling it, since a signal just gets
the process restarted a second later.

## Concurrency

- The daemon starts every component together and stops them together; a
  failure in one takes the rest down rather than leaving a half-running
  service that looks healthy.
- SQLite runs in WAL mode with a busy timeout, so the CLI can read while the
  daemon writes.
- Only one daemon may own a data directory; the control socket enforces it.
- The whole suite runs under `-race` in CI.
