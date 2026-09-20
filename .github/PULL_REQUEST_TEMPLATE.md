## What this changes

<!-- What does it do, and why? -->

## How it was tested

<!--
`make check` is the baseline. If this touches the mail path, say what you
tried by hand and in which client; the suite drives a real IMAP client, but
it is not a mail program.
-->

- [ ] `make check` passes
- [ ] Tested against a real mail client, if this affects the mail path (say which)

## Checklist

- [ ] No code path can lose, skip or overwrite a message without a test proving otherwise
- [ ] Secrets stay out of logs, errors and the database
- [ ] User-visible errors say what to do about the problem
- [ ] Anything non-obvious is explained in a comment saying *why*
