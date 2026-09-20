# Setting up a mail client

Ferry is an ordinary IMAP and SMTP server. Any client that can talk to one can
talk to Ferry, and nothing here is specific to a particular program.

## The settings

`ferry account add` prints everything you need, and `ferry status` prints it
again later:

| Setting | Incoming (IMAP) | Outgoing (SMTP) |
|---|---|---|
| Server | `localhost` | `localhost` |
| Port | `1993` | `1465` |
| Security | SSL/TLS (implicit, not STARTTLS) | SSL/TLS (implicit, not STARTTLS) |
| Username | the account name, e.g. `mysite` | the same |
| Password | the app password from `ferry account add` | the same |
| Authentication | Normal password | Normal password |

Two things trip people up:

**It is implicit TLS, not STARTTLS.** Ferry speaks TLS from the first byte on
both ports. A client set to "STARTTLS" will fail to connect. Choose "SSL/TLS",
which is what these ports have meant since 993 and 465 were assigned.

**Run `ferry trust` first.** Ferry generates its own certificate authority on
first run, and until you trust it, clients will refuse the connection or warn
about it.

## How your mail appears to recipients

The From address and the name beside it come from Ferry, not from the client:

```bash
ferry account identity mysite --display-name "Acme Support" --address contact@example.com
```

An account configured from a profile is managed, and most clients will not let
you edit those fields themselves, so set them here and reinstall the profile.

You do not need this to send from a *different* address occasionally: any
address on a verified domain is accepted, so add aliases in the client and
pick between them when composing. `identity` sets the default.

## Trusting the certificate

```bash
ferry trust
```

On macOS this adds Ferry's CA to your login keychain, with no administrator
rights, current user only. On Linux it prints the path and the command for
your distribution.

Some clients keep their own trust store rather than the system's, and need the
certificate added separately:

- **Thunderbird**: Settings → Privacy & Security → Certificates → View
  Certificates → Authorities → Import. Get the file with
  `ferry trust --print > ferry-ca.crt`.
- **Firefox-derived clients** generally behave the same way.

Everything that uses the system trust store (Apple Mail, Outlook, macOS and
iOS in general) is covered by `ferry trust` alone.

## Apple Mail

macOS and iOS can be configured from a profile, which saves typing all of the
above:

```bash
ferry mail-profile --open
```

Approve it in **System Settings → General → Device Management**. The profile
carries Ferry's CA certificate with it, so the account and the certificate are
set up together. macOS marks it "Unverified" because it is not signed by an
Apple developer certificate, which is expected for a profile generated on your own
machine from your own data.

The app password is **not** included unless you ask for it:

```bash
ferry mail-profile --with-password mysite=abcd-efgh-ijkl-mnop-qrst
```

Without it Mail asks once and stores the password in your keychain. With it
the file itself becomes a credential, which usually means a credential sitting
in your Downloads folder, so the default is to leave it out.

To set it up by hand instead, use **Mail → Settings → Accounts → + → Other
Mail Account**, then turn off "Automatically manage connection settings" under
**Server Settings** and enter the values from the table above.

## Thunderbird

Add the account with **Account Settings → Account Actions → Add Mail
Account**, then **Configure manually** and enter the values from the table.
Set both servers to **SSL/TLS** and **Normal password**.

Import Ferry's CA as described above, or Thunderbird will reject the
connection with `SEC_ERROR_UNKNOWN_ISSUER`.

## Outlook

Add the account manually (Outlook's autodiscovery will not find a loopback
server) and choose IMAP. Set encryption to **SSL/TLS** on both servers, not
STARTTLS.

## Mobile

A phone cannot reach `localhost` on your Mac, so this only works if Ferry is
reachable on the network, which means self-hosting it with a real certificate
rather than running it on your laptop. See
[Self-hosting](../README.md#self-hosting).

For iOS, `ferry mail-profile --host mail.example.com` writes a profile with
the right hostname, which you can email to yourself or serve over HTTPS.

## What works

Everything a mail client normally does: reading, replying, forwarding,
flagging, moving between folders, drafts, server-side search, and push updates
through IDLE.

Clients bind their Sent, Drafts, Trash, Junk and Archive buttons to the right
folders automatically, because Ferry advertises SPECIAL-USE.

## What to expect

**Your history downloads in the background.** A large account takes a while;
`ferry status` shows the progress. The account is usable throughout.

**The Sent folder holds everything Resend sent**, including transactional mail
from your website. That is not a bug; it is what the account actually sent.

**Deleting is local.** A message you delete goes to Trash, and expunging it
removes it from Ferry for good. Resend's copy is untouched, and Ferry
remembers not to download it again.

**Junk is empty and stays empty.** Ferry has no spam filter; the folder exists
so your client has somewhere to put anything you mark by hand.

## Troubleshooting

Run `ferry doctor` first. It checks the certificate, the credentials and the
listeners, and says what to do about whatever it finds.

**"The certificate is invalid"**: run `ferry trust`, then quit and reopen the
client. Trust decisions are usually cached for the life of the process.

**The client keeps asking for the password**: the app password may have been
replaced. Issue a new one with `ferry account passwd <name>` and update the
client.

**Sending fails**: the error text in the outbox says why. Usually an
unverified From domain, or an exhausted Resend quota.

**New mail is slow to appear**: Ferry polls every 60 seconds by default. Set
up [webhooks](../README.md#instant-delivery) for instant delivery.
