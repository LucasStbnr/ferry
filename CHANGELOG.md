# Changelog

All notable changes to Ferry are recorded here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow
[Semantic Versioning](https://semver.org/spec/v2.0.0.html).

Release notes from v0.1.0 onwards are generated from commit messages by
GoReleaser; this file carries the human summary.

## [Unreleased]

### Added

- IMAP server (`internal/imapd`) with the capabilities Apple Mail relies on:
  NAMESPACE, SPECIAL-USE, UIDPLUS, MOVE, IDLE, LIST-EXTENDED, ESEARCH and
  SEARCHRES. Full-text search is answered from SQLite's FTS5 index.
- SMTP submission server (`internal/smtpd`) with synchronous sends, so a
  failure appears in Mail's Outbox instead of disappearing into a queue.
  From-domain checks run against the account's verified Resend domains.
- Sync engine (`internal/mailsync`) with a resumable full-history backfill,
  incremental polling, dedupe by Resend id and Message-ID, and tombstones so a
  locally deleted message is never resurrected.
- Local store (`internal/store`): SQLite plus content-addressed blobs, holding
  the read state, folders, drafts and deletions that Resend does not model.
- Webhook receiver (`internal/webhook`) with mandatory Svix signature
  verification. Bounces and spam complaints become flagged Inbox messages.
- Local certificate authority and `ferry trust`, so Apple Mail connects over
  TLS without a warning.
- `ferry mail-profile`, which writes an Apple configuration profile that sets
  up Mail in one double-click.
- `ferry doctor`, which checks the installation end to end and reports what to
  do about anything wrong.
- Homebrew formula with a `brew services` definition, and a distroless
  container image for self-hosting.

[Unreleased]: https://github.com/LucasStbnr/ferry/compare/v0.1.0...HEAD
