# Recording the demo asciinema cast

The README has a placeholder for an asciinema cast at the top. To
record one (~30-60 seconds, the magic of "look how short this is"):

```bash
brew install asciinema   # or: pipx install asciinema

# Start recording in a clean state
asciinema rec demo.cast --idle-time-limit 1.5 --title "a2abridge in 60 seconds"

# Inside the recording session:
echo "# Install a2abridge"
curl -fsSL https://raw.githubusercontent.com/vbcherepanov/a2abridge/main/install.sh | bash
echo
echo "# Verify"
a2abridge doctor
echo
echo "# Start the directory daemon"
a2abridge service install
echo
echo "# Open Claude Code in another window — it auto-registers. Check:"
a2abridge directory   # GET /agents will show it
echo
exit  # Ctrl+D ends the recording
```

Upload:

```bash
asciinema upload demo.cast
# → prints a URL like https://asciinema.org/a/xyz123
```

Then add to README, right under the H1/tagline:

```md
[![asciicast](https://asciinema.org/a/xyz123.svg)](https://asciinema.org/a/xyz123)
```

Tips:
- Use `unset PROMPT_COMMAND` and a minimal `PS1='$ '` before recording so
  the prompts are clean.
- Type slower than you usually do — viewers can't pause keystrokes the
  way they pause video. ~150 ms idle delay is sweet spot.
- Keep the cast under 90 seconds. If install + smoke takes longer,
  stub the parts that don't change behaviour and let the viewer assume
  they ran.
- `--idle-time-limit 1.5` cuts long pauses out of the playback.

Alternatives:
- **GIF**: `agg demo.cast --output demo.gif` — works in Twitter / Reddit
  cards where asciinema embeds don't render.
- **Video**: record with QuickTime / OBS — heavier, but you can voice-
  over which converts on YouTube.
