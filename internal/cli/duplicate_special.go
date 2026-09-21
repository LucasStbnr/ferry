package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/LucasStbnr/ferry/internal/control"
	"github.com/LucasStbnr/ferry/internal/store"
)

// clientAliases are the folder names mail clients create when they decline a
// server's SPECIAL-USE mailbox and fall back to the names they ship with.
// Apple Mail uses the first of each pair, Outlook the second.
//
// Ferry already offers a mailbox for each of these roles, so a folder with one
// of these names is a duplicate: mail deleted in the client lands in it while
// the real Trash stays empty, and the account ends up with two of everything.
var clientAliases = map[string]string{
	"deleted messages": `\Trash`,
	"deleted items":    `\Trash`,
	"sent messages":    `\Sent`,
	"sent items":       `\Sent`,
	"junk e-mail":      `\Junk`,
	"junk email":       `\Junk`,
}

// checkDuplicateSpecialUse reports mailboxes that duplicate a special-use role
// and, with --repair, merges them back.
//
// The repair is refused while the daemon is running. imapd keeps each
// mailbox's message list in memory, so moving messages underneath it would
// leave connected clients reading a mailbox that no longer matches the
// database.
func (e *env) checkDuplicateSpecialUse(ctx context.Context, as *store.AccountStore, repair bool) []check {
	name := "Account " + as.Name() + " mailboxes"

	boxes, err := as.Mailboxes(ctx)
	if err != nil {
		return []check{{name: name, status: statusFail, detail: err.Error()}}
	}

	// Index the mailboxes that hold each special use, so a duplicate is only
	// reported when there is somewhere to merge it into.
	canonical := make(map[string]store.Mailbox, len(boxes))
	for _, b := range boxes {
		if b.SpecialUse != "" {
			canonical[b.SpecialUse] = b
		}
	}

	var checks []check
	for _, b := range boxes {
		if b.SpecialUse != "" {
			continue
		}
		use, ok := clientAliases[strings.ToLower(b.Name)]
		if !ok {
			continue
		}
		target, ok := canonical[use]
		if !ok || target.Name == b.Name {
			continue
		}

		msgs, err := as.Messages(ctx, b.ID)
		if err != nil {
			checks = append(checks, check{name: name, status: statusFail, detail: err.Error()})
			continue
		}

		c := check{
			name:   name,
			status: statusWarn,
			detail: fmt.Sprintf("%q duplicates %s (%s) and holds %d message(s)",
				b.Name, target.Name, use, len(msgs)),
		}

		if !repair {
			c.fix = fmt.Sprintf("move them into %s in your mail client, or stop the daemon and run `ferry doctor --repair`",
				target.Name)
			checks = append(checks, c)
			continue
		}

		if control.Available(ctx, e.cfg.ControlSocket()) {
			c.fix = "stop the daemon first (`ferry service stop`), then run `ferry doctor --repair` again"
			c.detail += "; not repaired while the daemon is running"
			checks = append(checks, c)
			continue
		}

		if len(msgs) > 0 {
			ids := make([]int64, len(msgs))
			for i := range msgs {
				ids[i] = msgs[i].ID
			}
			if _, err := as.Move(ctx, ids, target.ID); err != nil {
				c.status = statusFail
				c.detail = fmt.Sprintf("could not move %d message(s) out of %q: %v", len(ids), b.Name, err)
				checks = append(checks, c)
				continue
			}
		}
		// Only now is it safe to delete: DeleteMailbox tombstones whatever is
		// still inside, which for these messages would mean never seeing them
		// again.
		if err := as.DeleteMailbox(ctx, b.Name); err != nil {
			c.status = statusFail
			c.detail = fmt.Sprintf("moved %d message(s) into %s but could not remove %q: %v",
				len(msgs), target.Name, b.Name, err)
			checks = append(checks, c)
			continue
		}

		c.status = statusOK
		c.detail = fmt.Sprintf("merged %d message(s) from %q into %s and removed the duplicate",
			len(msgs), b.Name, target.Name)
		checks = append(checks, c)
	}

	return checks
}
