-- Ferry schema, version 1.
--
-- The database is the source of truth for everything Resend does not model:
-- read/unread, folders, drafts, deletions. Message bodies live outside as
-- content-addressed .eml blobs; this file holds the index and the IMAP state.

CREATE TABLE meta (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
) WITHOUT ROWID;

CREATE TABLE accounts (
    id            INTEGER PRIMARY KEY,
    name          TEXT    NOT NULL UNIQUE,
    address       TEXT    NOT NULL DEFAULT '',  -- primary From address
    display_name  TEXT    NOT NULL DEFAULT '',  -- name a client shows on outgoing mail
    domains       TEXT    NOT NULL DEFAULT '',  -- space-separated verified sending domains
    password_hash TEXT    NOT NULL DEFAULT '',  -- bcrypt of the generated app password
    created_at    INTEGER NOT NULL
);

CREATE TABLE mailboxes (
    id           INTEGER PRIMARY KEY,
    account_id   INTEGER NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    name         TEXT    NOT NULL,
    special_use  TEXT    NOT NULL DEFAULT '',   -- IMAP SPECIAL-USE attribute, e.g. \Sent
    uid_validity INTEGER NOT NULL,
    uid_next     INTEGER NOT NULL DEFAULT 1,
    subscribed   INTEGER NOT NULL DEFAULT 1,
    UNIQUE (account_id, name)
);

CREATE TABLE messages (
    id            INTEGER PRIMARY KEY,
    account_id    INTEGER NOT NULL REFERENCES accounts(id)  ON DELETE CASCADE,
    mailbox_id    INTEGER NOT NULL REFERENCES mailboxes(id) ON DELETE CASCADE,
    uid           INTEGER NOT NULL,
    resend_id     TEXT    NOT NULL DEFAULT '',  -- Resend email id, when it came from the API
    message_id    TEXT    NOT NULL DEFAULT '',  -- RFC 5322 Message-ID, for dedupe
    blob_hash     TEXT    NOT NULL,             -- sha256 of the .eml, hex
    size          INTEGER NOT NULL,
    internal_date INTEGER NOT NULL,             -- IMAP INTERNALDATE, unix seconds
    sent_date     INTEGER NOT NULL DEFAULT 0,   -- Date: header, unix seconds
    subject       TEXT    NOT NULL DEFAULT '',
    from_addr     TEXT    NOT NULL DEFAULT '',
    to_addr       TEXT    NOT NULL DEFAULT '',
    UNIQUE (mailbox_id, uid)
);

CREATE INDEX messages_by_mailbox   ON messages (mailbox_id, uid);
CREATE INDEX messages_by_resend_id ON messages (account_id, resend_id)  WHERE resend_id  <> '';
CREATE INDEX messages_by_message_id ON messages (account_id, message_id) WHERE message_id <> '';
CREATE INDEX messages_by_blob       ON messages (account_id, blob_hash);

CREATE TABLE message_flags (
    message_id INTEGER NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
    flag       TEXT    NOT NULL,                -- canonical case: \Seen, \Flagged, or a keyword
    PRIMARY KEY (message_id, flag)
) WITHOUT ROWID;

-- Reference-counted blob index. The file itself is at
-- <data>/blobs/<account>/<hash[0:2]>/<hash>.eml and is removed when refs hits 0.
CREATE TABLE blobs (
    account_id INTEGER NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    hash       TEXT    NOT NULL,
    size       INTEGER NOT NULL,
    refs       INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (account_id, hash)
) WITHOUT ROWID;

-- A message deleted locally must not reappear on the next sync. Resend has no
-- delete, so the tombstone is the only record that the user removed it.
CREATE TABLE tombstones (
    account_id INTEGER NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    resend_id  TEXT    NOT NULL,
    message_id TEXT    NOT NULL DEFAULT '',
    deleted_at INTEGER NOT NULL,
    PRIMARY KEY (account_id, resend_id)
) WITHOUT ROWID;

-- One row per (account, direction). Incremental sync stops at newest_id;
-- backfill walks older than backfill_cursor until it runs out.
CREATE TABLE sync_state (
    account_id      INTEGER NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    kind            TEXT    NOT NULL,           -- 'received' | 'sent'
    newest_id       TEXT    NOT NULL DEFAULT '',
    backfill_cursor TEXT    NOT NULL DEFAULT '',
    backfill_done   INTEGER NOT NULL DEFAULT 0,
    last_sync_at    INTEGER NOT NULL DEFAULT 0,
    last_error      TEXT    NOT NULL DEFAULT '',
    PRIMARY KEY (account_id, kind)
) WITHOUT ROWID;

-- Contentless FTS index; rowid mirrors messages.id.
CREATE VIRTUAL TABLE message_fts USING fts5 (
    subject, addrs, body,
    content = '',
    tokenize = 'unicode61 remove_diacritics 2'
);
