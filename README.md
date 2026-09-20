# Ferry

**Ferry makes a Resend account work in a normal mail client.**

Resend delivers your mail and shows it in a dashboard. It has no IMAP and no
SMTP, so Apple Mail, Thunderbird, Outlook and your phone all can't touch it.
Ferry runs on your machine, keeps a full copy of the account, and serves it as
an ordinary mail account.

Read, reply, send, search, folders, flags, drafts. It behaves like mail,
because to your client it *is* mail.

[![CI](https://github.com/LucasStbnr/ferry/actions/workflows/ci.yml/badge.svg)](https://github.com/LucasStbnr/ferry/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/LucasStbnr/ferry.svg)](https://pkg.go.dev/github.com/LucasStbnr/ferry)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

---

## Install

```bash
brew install --cask LucasStbnr/tap/ferry
```

Then, for each Resend account:

```bash
ferry account add mysite      # paste your Resend API key; note the app password
ferry trust                   # so clients accept Ferry's local certificate
ferry service install         # run it in the background from now on
```

`ferry account add` prints the host, the ports and the credentials. Point any
mail client at them and your history is already downloading into it.

On macOS there is a shortcut for Apple Mail, one file and one double-click:

```bash
ferry mail-profile --open
```

<details>
<summary>Other ways to install</summary>

**From source**

```bash
go install github.com/LucasStbnr/ferry/cmd/ferry@latest
```

**A release archive**: download from
[Releases](https://github.com/LucasStbnr/ferry/releases), unpack, and put
`ferry` on your `PATH`.

**Docker**: see [Self-hosting](#self-hosting).

`ferry service install` works with any of these: it registers Ferry with
launchd or systemd directly, so it does not depend on how Ferry was
installed.

</details>

---

## What it actually does

Resend is an append-only archive. It has no concept of read and unread, no
folders, no drafts, and no delete. A mail client expects all four.

So Ferry keeps them. Everything your client changes lives in a local SQLite
database and a content-addressed message store; everything Resend knows is
pulled down and never pushed back.

```
  Mail client                  Ferry                      Resend
  ┌──────────┐          ┌──────────────────┐          ┌───────────┐
  │          │  IMAP    │  imapd           │          │           │
  │  read    │─────────▶│    flags         │          │  received │
  │  search  │   TLS    │    folders       │◀─────────│  sent     │
  │  flag    │          │    drafts        │   poll   │  domains  │
  │          │          │    search (FTS5) │  webhook │           │
  │          │  SMTP    │  smtpd           │          │           │
  │  send    │─────────▶│    → send API    │─────────▶│  send     │
  └──────────┘   TLS    └──────────────────┘          └───────────┘
                             │        ▲
                             ▼        │
                        SQLite + .eml blobs
                        (the source of truth
                         for everything Resend
                         does not model)
```

Each Resend account becomes a **separate account** in your client, with its
own login, its own mailbox tree and no way for one to see another's messages.

### Deliberate choices worth knowing about

**Your history is downloaded in full, immediately.** Resend's raw-message and
attachment links are signed and expire. Ferry therefore stores every message
completely the first time it sees it: original MIME where Resend still offers
it, faithfully reconstructed from the structured fields where it does not. The
backfill starts the moment you add the account, and resumes exactly where it
stopped if interrupted.

**Deleting means deleting.** Resend has no delete, so a message you remove in
your client is removed locally and recorded as a tombstone. The next sync
recognises it and does not bring it back. Your Resend account is never
modified.

**Sending is synchronous.** When your client hands Ferry a message, Ferry
waits for Resend to accept it and returns the real answer on the SMTP
transaction. Over quota, unverified domain, too many recipients: you see it
in the outbox, in plain words, at the moment it happens. There is no hidden
queue and no message that claims to be sent but is not.

**Your Sent folder is everything Resend sent**, including the transactional
mail your website sends. Replies you write are stored byte for byte as you
composed them.

---

## Commands

| Command | What it does |
|---|---|
| `ferry account add <name>` | Add a Resend account; prints the app password once |
| `ferry account list` | Show configured accounts |
| `ferry account passwd <name>` | Issue a new app password |
| `ferry account refresh <name>` | Re-read verified sending domains from Resend |
| `ferry account webhook <name>` | Store the webhook signing secret |
| `ferry account remove <name>` | Remove an account and its local mail |
| `ferry serve` | Run the daemon in the foreground |
| `ferry service install` | Run it in the background under launchd or systemd |
| `ferry service start` / `stop` / `restart` / `status` | Control the background service |
| `ferry sync [account]` | Fetch now instead of waiting for the next poll |
| `ferry status` | Daemon state, per-account counts, last sync |
| `ferry doctor [--repair]` | Check everything and say what to fix |
| `ferry trust` | Trust Ferry's local certificate authority |
| `ferry mail-profile` | Write an Apple configuration profile (macOS, iOS) |

Run `ferry <command> --help` for the details.

---

## Configuration

Ferry works with no configuration. To change something, create
`config.json` in the data directory:

- macOS: `~/Library/Application Support/ferry/`
- Linux: `~/.local/share/ferry/` (or `$XDG_DATA_HOME/ferry`)
- Docker: `/data`

```jsonc
{
  "imap": { "addr": "127.0.0.1:1993" },
  "smtp": { "addr": "127.0.0.1:1465" },

  "sync": {
    "interval": "60s",           // "0s" disables polling
    "requests_per_second": 4,    // Resend allows 10/s per team
    "backfill_page_size": 100,
    "max_message_bytes": 67108864
  },

  // Off unless an address is set. See "Instant delivery" below.
  "webhook": { "addr": "", "path": "/webhooks/resend", "tls": false },

  // Only for self-hosting behind a real hostname.
  "tls": { "cert_file": "", "key_file": "", "hostnames": [] },

  "log_level": "info",
  "log_format": "text"
}
```

The default of 4 requests per second is half of Resend's per-team budget, so a
backfill never starves the website that shares the account.

---

## Instant delivery

Polling every 60 seconds is fine. Webhooks are better: mail appears the moment
Resend has it, and bounces become something you can actually see.

1. In Resend, create a webhook endpoint for `email.received`, `email.bounced`
   and `email.complained`, pointing at your Ferry instance.
2. Store the signing secret:

   ```bash
   ferry account webhook mysite
   ```

3. Enable the receiver in `config.json` (`"webhook": { "addr": ":8443" }`) and
   restart.

Every request must carry a valid signature; there is no way to turn that off.
Bounces and spam complaints are filed into your Inbox as flagged delivery
notices, so a failed send cannot pass unnoticed.

---

## Self-hosting

The same binary runs in a container:

```bash
docker run -d --name ferry \
  -v ferry-data:/data \
  -e FERRY_SECRET_API_KEY_MYSITE=re_your_key \
  -p 993:9993 -p 465:9465 \
  ghcr.io/lucasstbnr/ferry:latest
```

Secrets come from the environment (`FERRY_SECRET_API_KEY_<ACCOUNT>`) or from
Docker secrets mounted at `/run/secrets`. The image is distroless and runs as
a non-root user.

To serve beyond loopback, give Ferry a certificate clients can verify:

```jsonc
{
  "imap": { "addr": "0.0.0.0:9993" },
  "smtp": { "addr": "0.0.0.0:9465" },
  "tls": {
    "cert_file": "/data/tls/fullchain.pem",
    "key_file": "/data/tls/privkey.pem",
    "hostnames": ["mail.example.com"]
  }
}
```

Ferry warns if it is exposed with only its locally generated certificate.
Telling a mail client to ignore certificate errors is worse than not running
the service at all.

---

## Limits you should know about

These come from Resend, not from Ferry, and Ferry reports them rather than
hiding them.

| Limit | What it means |
|---|---|
| **Send quota** | Free tier: 100/day, 3,000/month. Over quota, sending fails with a permanent SMTP error naming the quota. |
| **Recipients** | 50 per message, counting To, Cc and Bcc. |
| **Attachments** | 40 MB per message. |
| **S/MIME and PGP** | Not possible. Resend's send API takes structured content, not raw MIME, so a signature cannot survive it. Ferry **refuses** such a message rather than silently sending it unsigned. |
| **Rate limit** | 10 requests/second per team, shared with everything else using the key. Ferry defaults to 4. |
| **Expiring links** | Raw messages and attachments have signed URLs that expire, which is why Ferry downloads everything at first sight. |

---

## Back up your data

**The data directory is the only copy of your read state, folders and drafts.**
Resend does not store any of it. If you lose the directory, Ferry can
re-download the messages but not what you did with them.

```bash
# macOS
tar czf ferry-backup.tar.gz -C ~/Library/Application\ Support ferry
```

Keep FileVault on. Ferry relies on full-disk encryption rather than encrypting
the store itself; see [SECURITY.md](SECURITY.md) for the full threat model.

---

## Troubleshooting

**Start here.**

```bash
ferry doctor
```

It checks the data directory, the database, the certificate, every account's
credentials, and whether the servers actually answer a TLS connection, and
tells you what to do about anything it finds.

<details>
<summary>The client says the certificate is not trusted</summary>

Run `ferry trust`, then quit and reopen the client, since trust decisions are
usually cached per process. On macOS, if you installed the configuration
profile, approve it in **System Settings → General → Device Management**; the
profile carries the certificate with it.

Some clients keep their own trust store rather than the system's. Thunderbird
is one: add Ferry's CA under **Settings → Privacy & Security → Certificates →
View Certificates → Authorities → Import**, using the file that
`ferry trust --print > ferry-ca.crt` gives you.
</details>

<details>
<summary>The client cannot connect at all</summary>

Check the daemon is running (`ferry status`) and that the ports match what the
client is configured with. `ferry status` reports the ports the daemon is
actually listening on, which is not necessarily what is in `config.json` if
`ferry serve` was given flag overrides.
</details>

<details>
<summary>Sending fails</summary>

The SMTP error text in your client's outbox says why. The usual causes are an
unverified From domain (`ferry account refresh <name>` after verifying it in
Resend) and an exhausted send quota.
</details>

<details>
<summary>Mail is missing, or history looks incomplete</summary>

`ferry status` shows whether the backfill has finished. To force a full
re-walk:

```bash
ferry sync --backfill
```

Messages already stored are skipped and messages you deleted stay deleted, so
this costs API calls but never creates duplicates.
</details>

---

## Development

```bash
make build      # build the binary
make check      # vet, lint, and tests under the race detector
make test       # tests only
```

No test touches the network: `internal/testutil/fakeresend` imitates the
Resend API, including its rate limits and expiring URLs. The IMAP and SMTP
suites drive the real servers with real clients, and the end-to-end suite
builds the binary, starts a daemon and talks to it over TLS.

See [CONTRIBUTING.md](CONTRIBUTING.md), [docs/architecture.md](docs/architecture.md)
and [docs/mail-clients.md](docs/mail-clients.md).

---

## License

MIT. See [LICENSE](LICENSE).

Ferry is not affiliated with Resend.
