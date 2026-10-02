# ass

Agent Session Search. Search conversations across coding agents and pick one to continue.

macOS, zsh, Go 1.25+, fzf 0.74+. 
Install:
```zsh
brew install go fzf
brew install --cask codex
go install github.com/melkikh/ass@latest

# add to `~/.zshrc` (use your `GOBIN` if customized):
export PATH="$(go env GOPATH)/bin:$PATH"
eval "$(command ass --zsh)"
```

Try `ass` → type `login !test` in fzf (find `login`, exclude `test`) → `↑`/`↓` → `Enter` → choose an opener → `Enter`. `Esc` cancels.

`ass -a` includes internal sessions and tool traffic. See `ass --help` for settings.

Cache: `~/.cache/cs/index-v5.sqlite` by default — SQLite/FTS5 (unencrypted; owner-only: files `0600`, directories `0700`) with session metadata, dialogue and tool text, updated incrementally from local agent histories.

| Source → opener | Codex.app | Codex CLI | Claude CLI | OpenCode CLI |
|---|---|---|---|---|
| Codex | ✓ open | ✓ resume | ✗ | ✗ |
| Claude | ✓ import | ✓ import | ✓ resume | ✗ |
| Cursor | ✓ text import | ✓ text import | ✗ | ✗ |
| OpenCode | ✓ text import | ✓ text import | ✗ | ✓ resume |
