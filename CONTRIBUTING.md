# Contributing to Ferry

Thanks for taking a look. Ferry is small and deliberately conservative: it
holds someone's mail, and the worst bug it can have is one that loses a
message quietly.

## Getting set up

```bash
git clone https://github.com/LucasStbnr/ferry.git
cd ferry
make build
make check     # vet, lint and the tests under the race detector
```

You need Go (the version in `go.mod`) and, for linting,
[golangci-lint](https://golangci-lint.run).

No test touches the network. `internal/testutil/fakeresend` is an in-memory
imitation of the Resend API, including its rate limits and its expiring
download URLs, and everything runs against that.

## What a good change looks like

**Never lose a message.** If a code path can drop, skip or overwrite mail, it
needs a test that proves it does not. The sync engine holds its watermark back
rather than stepping over a message it failed to fetch, and deletions are
tombstoned so they cannot be resurrected; changes in that area deserve extra
care.

**Fail loudly, in words the user can act on.** An SMTP rejection shows up in
the user's mail client verbatim, so its text is user-facing copy. "Resend's
sending quota is used up" is useful; "550 error" is not.

**Explain the non-obvious in comments.** Say why, not what. The comment worth
writing is the one that stops the next person from "simplifying" a subtlety
back into a bug, for example why the MOVE handler does not write EXPUNGE
responses itself.

**Keep secrets out of logs.** API keys, app passwords and signing secrets are
never logged, never put in error messages, and never written to the database
in the clear.

## Testing

```bash
make test         # everything
make test-short   # skips the end-to-end suite, which builds the binary
make test-race    # what CI runs
```

Against a real Resend account, set your own key in your own shell, and never
paste one into an issue or a pull request:

```bash
export FERRY_LIVE_KEY_A=re_...
```

## Commits and pull requests

Commit messages follow [Conventional Commits](https://www.conventionalcommits.org)
(`feat:`, `fix:`, `docs:`, `refactor:`, `test:`, `chore:`), because the release
changelog is generated from them.

Before opening a pull request, run `make check`. If your change affects how a
mail client behaves, say which client you tested by hand and what you tried;
the automated suite drives a real IMAP client, but it is not a mail program.

## Reporting a security issue

Please do not open a public issue. See [SECURITY.md](SECURITY.md).
