// Package cli is Ferry's command surface.
//
// Commands that only touch local state open the database directly. Commands
// that need the running daemon (sync, status) go through the control socket
// so two processes never write to the same database, and fall back to doing
// the work themselves when no daemon is running.
package cli

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"

	"github.com/spf13/cobra"

	"github.com/LucasStbnr/ferry/internal/account"
	"github.com/LucasStbnr/ferry/internal/config"
	"github.com/LucasStbnr/ferry/internal/secrets"
	"github.com/LucasStbnr/ferry/internal/store"
)

// Build information, set by the linker at release time.
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

// env carries what every command needs, built lazily so `ferry version` and
// `ferry --help` never touch the disk.
type env struct {
	dataDir  string
	logLevel string
	logJSON  bool

	cfg config.Config
	db  *store.DB
	sec secrets.Store
	mgr *account.Manager
	log *slog.Logger

	out io.Writer
	err io.Writer
}

// Execute runs the CLI.
func Execute(ctx context.Context) error {
	e := &env{out: os.Stdout, err: os.Stderr}

	root := &cobra.Command{
		Use:   "ferry",
		Short: "Bridge a Resend account into Apple Mail over IMAP and SMTP",
		Long: `Ferry exposes each Resend account as a local IMAP and SMTP server, so
Apple Mail, or any mail client, can read, search, reply to and send mail
that Resend would otherwise only show in its dashboard.

Everything Resend does not model is kept locally: read state, folders, drafts
and deletions. Deleting a message in Mail removes it here and records a
tombstone; Resend's own copy is never touched.

Getting started:

  ferry account add mysite     add a Resend account and print its app password
  ferry trust                  tell macOS to trust Ferry's local certificate
  ferry mail-profile           write a profile that configures Apple Mail
  ferry serve                  run the daemon in the foreground`,
		SilenceUsage:  true,
		SilenceErrors: true,
		CompletionOptions: cobra.CompletionOptions{
			HiddenDefaultCmd: true,
		},
	}

	root.PersistentFlags().StringVar(&e.dataDir, "data-dir", "",
		"where Ferry keeps its database, blobs and certificates (default: platform application-support directory, or $FERRY_DATA_DIR)")
	root.PersistentFlags().StringVar(&e.logLevel, "log-level", "", "log level: debug, info, warn or error")
	root.PersistentFlags().BoolVar(&e.logJSON, "log-json", false, "write logs as JSON")

	root.AddCommand(
		newServeCmd(e),
		newAccountCmd(e),
		newSyncCmd(e),
		newStatusCmd(e),
		newDoctorCmd(e),
		newTrustCmd(e),
		newMailProfileCmd(e),
		newVersionCmd(e),
	)

	root.SetOut(e.out)
	root.SetErr(e.err)
	return root.ExecuteContext(ctx)
}

// loadConfig resolves the data directory and configuration. It is safe to call
// more than once.
func (e *env) loadConfig() error {
	if e.cfg.Dir != "" {
		return nil
	}
	cfg, err := config.Load(e.dataDir)
	if err != nil {
		return err
	}
	if e.logLevel != "" {
		cfg.LogLevel = e.logLevel
	}
	if e.logJSON {
		cfg.LogFormat = "json"
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	if err := cfg.EnsureDirs(); err != nil {
		return err
	}
	e.cfg = cfg
	e.log = newLogger(e.err, cfg)
	return nil
}

// open connects to the database and the secret store.
func (e *env) open(ctx context.Context) error {
	if e.db != nil {
		return nil
	}
	if err := e.loadConfig(); err != nil {
		return err
	}
	db, err := store.Open(ctx, e.cfg.DBPath(), e.cfg.BlobDir())
	if err != nil {
		return err
	}
	e.db = db
	e.sec = secrets.Open(e.cfg.Dir)
	e.mgr = account.NewManager(db, e.sec, e.log)
	return nil
}

// close releases the database. Commands defer it.
func (e *env) close() {
	if e.db != nil {
		_ = e.db.Close()
		e.db = nil
	}
}

func newLogger(w io.Writer, cfg config.Config) *slog.Logger {
	level := slog.LevelInfo
	switch cfg.LogLevel {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	opts := &slog.HandlerOptions{Level: level}
	if cfg.LogFormat == "json" {
		return slog.New(slog.NewJSONHandler(w, opts))
	}
	return slog.New(slog.NewTextHandler(w, opts))
}

// printf writes to the command's output. A failed write to stdout is not
// something the CLI can usefully report or recover from.
func (e *env) printf(format string, args ...any) {
	_, _ = fmt.Fprintf(e.out, format, args...)
}

func newVersionCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the version",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			e.printf("ferry %s (commit %s, built %s)\n", version, commit, date)
			return nil
		},
	}
}

// Version returns the build version, for the daemon's status output.
func Version() string { return version }
