# Homebrew formula for a2abridge.
#
# Goes into a separate tap repo: github.com/vbcherepanov/homebrew-tap
# at Formula/a2abridge.rb. This file lives here only as the canonical
# source — copy it into the tap repo on each release and update the
# version + sha256 fields.
#
# After publishing the tap:
#   brew tap vbcherepanov/tap
#   brew install a2abridge

class A2abridge < Formula
  desc "Open A2A 1.0 mesh for Claude Code, Codex, Cursor, Cline, Continue and Gemini"
  homepage "https://github.com/vbcherepanov/a2abridge"
  version "2.0.0"
  license "MIT"

  on_macos do
    on_arm do
      url "https://github.com/vbcherepanov/a2abridge/releases/download/v2.0.0/a2abridge_2.0.0_darwin_arm64.tar.gz"
      sha256 "50ba658946523565eaa44404a1051ff3fb54162423fef4425c68670046d83bf6"
    end
    on_intel do
      url "https://github.com/vbcherepanov/a2abridge/releases/download/v2.0.0/a2abridge_2.0.0_darwin_amd64.tar.gz"
      sha256 "618e9de8c49bfa48d1f164c116f75be8d9908b1d4135de82a30ecf845ec41014"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/vbcherepanov/a2abridge/releases/download/v2.0.0/a2abridge_2.0.0_linux_arm64.tar.gz"
      sha256 "ab01832a2aed649d68796b4cf5ef291c5f96a22195c1370511efc18c5ca89b38"
    end
    on_intel do
      url "https://github.com/vbcherepanov/a2abridge/releases/download/v2.0.0/a2abridge_2.0.0_linux_amd64.tar.gz"
      sha256 "e37d305ecc489da65a8e4ce82a0f5d6ce5f01f43476bb2f125ab438c1fa4aa49"
    end
  end

  def install
    bin.install "a2abridge"
    generate_completions_from_executable(bin/"a2abridge", "completion")
  end

  service do
    run [opt_bin/"a2abridge", "directory", "-addr", "127.0.0.1:7777"]
    keep_alive true
    log_path var/"log/a2abridge.log"
    error_log_path var/"log/a2abridge.err.log"
  end

  test do
    assert_match "a2abridge #{version}", shell_output("#{bin}/a2abridge version")
  end
end
