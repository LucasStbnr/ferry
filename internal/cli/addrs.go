package cli

import (
	"context"

	"github.com/LucasStbnr/ferry/internal/control"
)

// effectiveAddrs reports where the mail servers actually are.
//
// `ferry serve` accepts --imap-addr and --smtp-addr, so the running daemon can
// be listening somewhere config.json does not mention. Commands that connect
// to it, or that tell Apple Mail where to connect, must use the live values or
// they will confidently point at the wrong port. When no daemon is running the
// configuration is the best answer available.
func (e *env) effectiveAddrs(ctx context.Context) (imapAddr, smtpAddr string) {
	imapAddr, smtpAddr = e.cfg.IMAP.Addr, e.cfg.SMTP.Addr

	socket := e.cfg.ControlSocket()
	if !control.Available(ctx, socket) {
		return imapAddr, smtpAddr
	}
	st, err := control.Dial(socket).Status(ctx)
	if err != nil {
		return imapAddr, smtpAddr
	}
	if st.IMAPAddr != "" {
		imapAddr = st.IMAPAddr
	}
	if st.SMTPAddr != "" {
		smtpAddr = st.SMTPAddr
	}
	return imapAddr, smtpAddr
}
