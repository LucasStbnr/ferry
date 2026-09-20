# A build-from-source formula, kept in the repository so `ferry` can be
# installed before the first release and so the tap is not the only path in.
#
# Released versions are published to the tap by GoReleaser, which generates a
# bottle-style formula pointing at the release archives:
#
#     brew install LucasStbnr/tap/ferry
#
# To install from this file:
#
#     brew install --build-from-source ./Formula/ferry.rb
class Ferry < Formula
  desc "IMAP and SMTP bridge that puts Resend accounts into Apple Mail"
  homepage "https://github.com/LucasStbnr/ferry"
  url "https://github.com/LucasStbnr/ferry/archive/refs/tags/v0.1.0.tar.gz"
  sha256 "0000000000000000000000000000000000000000000000000000000000000000"
  license "MIT"
  head "https://github.com/LucasStbnr/ferry.git", branch: "main"

  depends_on "go" => :build

  def install
    ldflags = %W[
      -s -w
      -X github.com/LucasStbnr/ferry/internal/cli.version=#{version}
      -X github.com/LucasStbnr/ferry/internal/cli.commit=#{tap&.installed? ? Utils.git_head : "none"}
      -X github.com/LucasStbnr/ferry/internal/cli.date=#{time.iso8601}
    ]
    system "go", "build", *std_go_args(ldflags: ldflags), "./cmd/ferry"
  end

  service do
    run [opt_bin/"ferry", "serve"]
    keep_alive true
    working_dir var
    log_path var/"log/ferry.log"
    error_log_path var/"log/ferry.log"
  end

  def caveats
    <<~EOS
      Ferry runs as a local service and speaks IMAP and SMTP over TLS on
      loopback only.

      Getting started:

        ferry account add mysite      add a Resend account; the app password
                                      is printed once
        ferry trust                   trust Ferry's local certificate so Mail
                                      connects without a warning
        ferry mail-profile --open     configure Apple Mail in one double-click
        brew services start ferry     run it in the background

      Mail is stored under
        ~/Library/Application Support/ferry

      That directory is the only copy of your read state, folders and drafts:
      Resend does not store them. Back it up, and keep FileVault on.
    EOS
  end

  test do
    assert_match "ferry", shell_output("#{bin}/ferry version")

    # A fresh data directory must produce a usable, private installation.
    data = testpath/"data"
    output = shell_output("#{bin}/ferry --data-dir #{data} account list")
    assert_match "No accounts yet", output

    # doctor exits non-zero when something is actually wrong; with no accounts
    # it reports warnings only.
    shell_output("#{bin}/ferry --data-dir #{data} doctor", 0)
  end
end
