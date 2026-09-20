package cli

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/LucasStbnr/ferry/internal/service"
)

func newServiceCmd(e *env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "service",
		Short: "Run Ferry in the background under launchd or systemd",
		Long: `Installs Ferry as a background service so it starts at login and stays
running, without needing a terminal open.

The service is per-user: a LaunchAgent on macOS, a "systemctl --user" unit on
Linux. That is deliberate. Ferry's data directory lives in your home folder
and its API keys are in your login keychain, neither of which a root daemon
could reach.

This works however Ferry was installed: Homebrew, go install, a release
archive or a local build.`,
	}
	cmd.AddCommand(
		newServiceInstallCmd(e),
		newServiceUninstallCmd(e),
		newServiceStartCmd(e),
		newServiceStopCmd(e),
		newServiceRestartCmd(e),
		newServiceStatusCmd(e),
	)
	return cmd
}

// withManager resolves the platform's service manager, turning the
// unsupported case into advice rather than a bare error.
func withManager(ctx context.Context, e *env, fn func(context.Context, service.Manager) error) error {
	if err := e.loadConfig(); err != nil {
		return err
	}
	mgr, err := service.New()
	if errors.Is(err, service.ErrUnsupported) {
		return fmt.Errorf("%w\n\nRun `ferry serve` directly, or supervise it with whatever "+
			"your system uses (a container restart policy, runit, an init script)", err)
	}
	if err != nil {
		return err
	}
	return fn(ctx, mgr)
}

func newServiceInstallCmd(e *env) *cobra.Command {
	var start bool
	cmd := &cobra.Command{
		Use:   "install",
		Short: "Install the background service and start it",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withManager(cmd.Context(), e, func(ctx context.Context, mgr service.Manager) error {
				cfg, err := service.DefaultConfig(e.cfg.Dir)
				if err != nil {
					return err
				}
				if err := mgr.Install(ctx, cfg); err != nil {
					return err
				}
				if !start {
					if err := mgr.Stop(ctx); err != nil {
						e.printf("Note: installed, but could not stop it again: %v\n", err)
					}
				}

				path, _ := mgr.UnitPath()
				e.printf("Ferry is installed as a %s service.\n\n", mgr.Name())
				e.printf("  Definition  %s\n", path)
				e.printf("  Binary      %s\n", cfg.Executable)
				e.printf("  Data        %s\n", cfg.DataDir)
				e.printf("  Log         %s\n", cfg.LogPath)
				if start {
					e.printf("\nIt is running now and will start again at login.\n")
					e.printf("Check it with `ferry status`.\n")
				} else {
					e.printf("\nIt is installed but not running. Start it with `ferry service start`.\n")
				}
				return nil
			})
		},
	}
	cmd.Flags().BoolVar(&start, "start", true, "start the service immediately")
	return cmd
}

func newServiceUninstallCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:   "uninstall",
		Short: "Stop the background service and remove it",
		Long: `Stops Ferry and removes the service definition.

Your accounts and mail are left alone; this only stops Ferry running in the
background. Use ` + "`ferry account remove`" + ` to delete an account's data.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withManager(cmd.Context(), e, func(ctx context.Context, mgr service.Manager) error {
				if err := mgr.Uninstall(ctx); err != nil {
					return err
				}
				e.printf("The %s service has been removed. Your mail is untouched.\n", mgr.Name())
				return nil
			})
		},
	}
}

func newServiceStartCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:   "start",
		Short: "Start the background service",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withManager(cmd.Context(), e, func(ctx context.Context, mgr service.Manager) error {
				if err := mgr.Start(ctx); err != nil {
					return err
				}
				e.printf("Ferry is starting. Check it with `ferry status`.\n")
				return nil
			})
		},
	}
}

func newServiceStopCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:   "stop",
		Short: "Stop the background service",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withManager(cmd.Context(), e, func(ctx context.Context, mgr service.Manager) error {
				if err := mgr.Stop(ctx); err != nil {
					return err
				}
				e.printf("Ferry has been stopped. Mail clients will not connect until it is started again.\n")
				return nil
			})
		},
	}
}

func newServiceRestartCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:   "restart",
		Short: "Restart the background service",
		Long:  "Restarts Ferry, which is what picks up a change to config.json.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withManager(cmd.Context(), e, func(ctx context.Context, mgr service.Manager) error {
				if err := mgr.Restart(ctx); err != nil {
					return err
				}
				e.printf("Ferry is restarting.\n")
				return nil
			})
		},
	}
}

func newServiceStatusCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show whether the background service is installed and running",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withManager(cmd.Context(), e, func(ctx context.Context, mgr service.Manager) error {
				st, err := mgr.Status(ctx)
				if err != nil {
					return err
				}
				switch {
				case !st.Installed:
					e.printf("Not installed as a service (%s).\n", st.Manager)
					e.printf("Install it with `ferry service install`, or run `ferry serve` in a terminal.\n")
				case st.Running:
					e.printf("Running under %s", st.Manager)
					if st.PID != 0 {
						e.printf(", pid %d", st.PID)
					}
					e.printf(".\n")
				default:
					e.printf("Installed under %s but not running.\n", st.Manager)
					e.printf("Start it with `ferry service start`.\n")
				}
				if st.Detail != "" {
					e.printf("  %s\n", st.Detail)
				}
				if st.UnitPath != "" {
					e.printf("  Definition: %s\n", st.UnitPath)
				}
				logPath := e.cfg.Path("ferry.log")
				if _, err := os.Stat(logPath); err == nil {
					e.printf("  Log:        %s\n", logPath)
				}
				return nil
			})
		},
	}
}

// serviceHint is what other commands suggest when the daemon is not running.
//
// It asks whether a supervisor is actually reachable rather than assuming one
// from the platform: inside a container there is no launchd and usually no
// systemd, and telling someone to run `ferry service start` there would send
// them after a command that cannot work.
func serviceHint() string {
	if _, err := service.New(); err != nil {
		return "run `ferry serve` (this system has no service manager Ferry can use)"
	}
	return "start it with `ferry service start`, or run `ferry serve` in a terminal"
}
