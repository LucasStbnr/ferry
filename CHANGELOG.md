# Changelog

All notable changes to Ferry are recorded here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow
[Semantic Versioning](https://semver.org/spec/v2.0.0.html).

Release notes from v0.1.0 onwards are generated from commit messages by
GoReleaser; this file carries the human summary.

## [Unreleased]

### Fixed

- Deleting a message did nothing, silently. Apple Mail builds mailbox paths as
  prefix + delimiter + name, and the generated configuration profile set an
  empty `IncomingMailServerIMAPPathPrefix`, so Mail asked for `/Trash` and the
  server answered NONEXISTENT. Because Mail's delete is a move to Trash, it
  failed with nothing shown to the user. The profile no longer sets the key,
  and the IMAP server now accepts a leading delimiter from any client.
- Sent mail never synced. Resend returns `created_at` as a Postgres-style
  timestamp on the sent listing, which is not RFC 3339, so the whole response
  failed to decode and the backfill never finished. Timestamps are now parsed
  leniently.
- The end-to-end tests read and wrote the real user's credential store. The OS
  keyring is machine-global and keyed by service name alone, so pointing a test
  at a throwaway data directory did not isolate it; the suite overwrote and
  then deleted a real Resend API key. `FERRY_SECRET_STORE` now selects the
  backend explicitly and the tests force a file store.
- `ferry service restart` bounced the process without re-reading the service
  definition, so an edited unit was silently ignored.

### Added

- IMAP server (`internal/imapd`) with the capabilities mail clients rely on:
  NAMESPACE, SPECIAL-USE, UIDPLUS, MOVE, IDLE, LIST-EXTENDED, ESEARCH and
  SEARCHRES. Full-text search is answered from SQLite's FTS5 index.
- SMTP submission server (`internal/smtpd`) with synchronous sends, so a
  failure appears in the client's outbox instead of disappearing into a queue.
  From-domain checks run against the account's verified Resend domains.
- Sync engine (`internal/mailsync`) with a resumable full-history backfill,
  incremental polling, dedupe by Resend id and Message-ID, and tombstones so a
  locally deleted message is never resurrected.
- Local store (`internal/store`): SQLite plus content-addressed blobs, holding
  the read state, folders, drafts and deletions that Resend does not model.
- Webhook receiver (`internal/webhook`) with mandatory Svix signature
  verification. Bounces and spam complaints become flagged Inbox messages.
- Local certificate authority and `ferry trust`, so mail clients connect over
  TLS without a warning.
- `ferry service`, which installs Ferry as a per-user LaunchAgent on macOS or
  a `systemctl --user` unit on Linux, whatever way Ferry was installed.
- `ferry mail-profile`, which writes an Apple configuration profile that sets
  up Mail in one double-click on macOS and iOS.
- `ferry account set-key`, to rotate a Resend API key or restore a lost one
  without disturbing stored mail or the app password a client is using.
- `ferry account identity`, to set the From address and the sender name shown
  to recipients. A profile-managed account is read-only in most clients, so
  these have to come from Ferry.
- `ferry serve --trace-protocol`, which logs the raw IMAP and SMTP
  conversation. It contains credentials and is for debugging only, but it is
  the only way to see what a client actually sends.
- `ferry doctor`, which checks the installation end to end and reports what to
  do about anything wrong.
- Homebrew cask and a multi-architecture distroless container image for
  self-hosting.

[Unreleased]: https://github.com/LucasStbnr/ferry/compare/v0.1.0...HEAD
