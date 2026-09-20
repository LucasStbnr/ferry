// Command ferry bridges a Resend account into any IMAP/SMTP mail client.
//
// Resend has no IMAP or SMTP, so no mail client can talk to it. Ferry runs
// locally, keeps a full copy of the account's mail in SQLite, and serves it as
// an ordinary mail account: read, reply, send, search, folders and flags all
// work, and everything Resend cannot model (read state, folders, drafts,
// deletion) is kept here.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/LucasStbnr/ferry/internal/cli"
)

func main() {
	// run is a separate function so its deferred signal cleanup happens before
	// the process exits; os.Exit in main would skip it.
	os.Exit(run())
}

func run() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := cli.Execute(ctx); err != nil {
		// A Ctrl-C during `ferry serve` is a normal way to stop it, not a
		// failure worth printing a message about.
		if !errors.Is(err, context.Canceled) {
			fmt.Fprintln(os.Stderr, "ferry:", err)
		}
		return 1
	}
	return 0
}
