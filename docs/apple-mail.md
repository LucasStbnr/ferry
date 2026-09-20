# Setting up Apple Mail

The short version:

```bash
ferry trust
ferry mail-profile --open
```

Approve the profile in **System Settings → General → Device Management**, and
Mail has the account.

## What the profile does

It carries three things: Ferry's CA certificate, the IMAP account and the SMTP
account. macOS marks it "Unverified" because it is not signed by an Apple
developer certificate, which is expected for a profile generated on your own machine
from your own data.

The app password is **not** included unless you ask for it:

```bash
ferry mail-profile --with-password mysite=abcd-efgh-ijkl-mnop-qrst
```

Without it Mail asks once and stores the password in your keychain. With it
the file itself becomes a credential, which usually means a credential sitting
in your Downloads folder, so the default is to leave it out.

## Setting it up by hand

If you would rather not install a profile, add the account manually. In
**Mail → Settings → Accounts → + → Other Mail Account**:

| Field | Value |
|---|---|
| Email address | The account's address (`ferry account list` shows it) |
| User name | The account name, e.g. `mysite` |
| Password | The app password from `ferry account add` |

Then, under **Server Settings**, turn off "Automatically manage connection
settings" for both servers and enter:

| | Incoming (IMAP) | Outgoing (SMTP) |
|---|---|---|
| Host | `localhost` | `localhost` |
| Port | `1993` | `1465` |
| Use TLS/SSL | Yes | Yes |
| Authentication | Password | Password |

Run `ferry trust` first, or Mail will refuse the certificate.

## What works

Everything a mail client normally does: reading, replying, forwarding,
flagging, moving between folders, drafts, server-side search, and push
updates through IDLE.

Mail's Sent, Drafts, Trash, Junk and Archive buttons bind to the right folders
because Ferry advertises SPECIAL-USE.

## What to expect

**Your history downloads in the background.** A large account takes a while;
`ferry status` shows the progress. Mail is usable throughout.

**The Sent folder holds everything Resend sent**, including transactional mail
from your website. That is not a bug; it is what the account actually sent.

**Deleting is local.** A message you delete goes into Trash, and expunging it
removes it from Ferry for good. Resend's copy is untouched, and Ferry
remembers not to download it again.

**Junk is empty and stays empty.** Ferry has no spam filter; the folder exists
so Mail has somewhere to put anything you mark by hand.

## Troubleshooting

Run `ferry doctor` first. It checks the certificate, the credentials and the
listeners, and says what to do about whatever it finds.

**"The certificate is invalid"**: run `ferry trust`, then quit and reopen
Mail. macOS caches trust decisions per process.

**Mail keeps asking for the password**: the app password may have been
replaced. Issue a new one with `ferry account passwd <name>` and update Mail.

**Sending fails**: the error text in the Outbox says why. Usually an
unverified From domain, or an exhausted Resend quota.

**Mail is slow to notice new messages**: Ferry polls every 60 seconds by
default. Set up [webhooks](../README.md#instant-delivery) for instant
delivery.
