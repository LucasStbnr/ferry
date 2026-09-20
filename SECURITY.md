# Security

## Reporting a vulnerability

Please report security issues privately through
[GitHub's private vulnerability reporting](https://github.com/LucasStbnr/ferry/security/advisories/new)
rather than in a public issue.

Include what you did, what happened, and what you expected. A proof of concept
helps. You can expect an acknowledgement within a few days.

Please do not include a real Resend API key, a real app password or real mail
in a report.

## What Ferry protects, and what it does not

Ferry holds a complete copy of an account's mail and the credentials to send
as that account. The threat model is worth stating plainly.

### Protected

- **Credentials in transit.** IMAP and SMTP use implicit TLS from the first
  byte. There is no cleartext phase and no STARTTLS to strip, so an app
  password never crosses a socket unprotected, not even on loopback, where any
  process on the machine could otherwise read it.
- **Credentials at rest.** The Resend API key and the webhook signing secret
  live in the OS credential store (the login Keychain on macOS), or come from
  the environment or a mounted secret when self-hosted. They are never written
  to the database and never logged. The app password is stored only as a
  bcrypt hash.
- **Network exposure.** Both servers bind to `127.0.0.1` by default. Nothing
  is reachable from the network unless you change that deliberately.
- **Authentication timing.** A failed login costs the same whether or not the
  account exists, so response times cannot be used to enumerate account names.
- **Webhook authenticity.** Every webhook request must carry a valid Svix
  signature over its exact body, within a five-minute window. There is no
  option to disable this: an unsigned endpoint would let anyone who found the
  URL inject messages into the Inbox.
- **The control socket.** The CLI reaches the daemon over a Unix socket with
  mode 0600. Authorisation is the filesystem's.

### Not protected

- **Mail at rest.** Messages are stored as ordinary files and rows, readable
  by your user account. Ferry relies on full-disk encryption (FileVault on
  macOS) rather than encrypting them itself. Anyone with your logged-in user
  session can read your mail, exactly as they could with Mail's own store.
- **Other processes on the machine.** A local loopback listener is reachable
  by any process running as any user on the host. The app password is the only
  barrier.
- **Ferry's local CA.** `ferry trust` installs a certificate authority into
  your keychain. Its private key sits in the data directory. Anyone who can
  read that key can issue certificates your machine will trust. This is the
  same trade-off `mkcert` makes, and the reason the key is mode 0600 and the
  CA is constrained to a path length of zero.
- **Resend itself.** Ferry cannot make Resend more private than it is. Your
  mail is in their system and Ferry only reads it.
- **End-to-end encryption.** Resend's send API takes structured content, not
  raw MIME, so S/MIME and PGP cannot survive it. Ferry refuses to send such a
  message rather than stripping the signature and sending it anyway.

## Self-hosting

Serving beyond loopback needs a certificate clients can actually verify. Set
`tls.cert_file` and `tls.key_file` in `config.json`; Ferry warns when it is
exposed with a locally generated certificate, because telling clients to
ignore certificate errors on a mail server is worse than not running it.

## Supported versions

The latest release is supported. Fixes are released as a new patch version.
