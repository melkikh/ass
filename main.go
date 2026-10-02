package main

import (
	"context"
	_ "embed"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"
)

//go:embed ass.zsh
var zsh string

const usage = `ass — Agent Session Search

setup: eval "$(command ass --zsh)"

ass          search and resume a session (zsh function)
ass -a       add tool matches and internal/untitled sessions
ass --help   show this help
ass --zsh    print zsh integration

Environment:
ASS_DEFAULT_AGENT  first opener (default: codex app)
ASS_CODEX_BIN      Codex executable (default: codex)
ASS_OPENCODE_BIN   OpenCode executable (default: opencode)
ASS_CURSOR_DB      Cursor history database
ASS_OPENCODE_DB    OpenCode history database
CODEX_HOME         Codex directory (default: ~/.codex)
CLAUDE_CONFIG_DIR  Claude directory (default: ~/.claude)
XDG_DATA_HOME      data directory (default: ~/.local/share)
XDG_CACHE_HOME     cache directory (default: ~/.cache; ass uses cs/)

Tool fields: up to 64 KiB each. Relative paths start at the caller's directory.
fzf defaults are ignored.
needs: macOS, zsh, fzf 0.74+, your agents
`

func writeZsh(w io.Writer, executable string) error {
	_, err := fmt.Fprintf(w, "typeset -g _ASS_BIN=%s\n%s", shellQuote(executable), zsh)
	return err
}

func unixMillis(n int64) time.Time { return time.UnixMilli(n) }
func run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("enable zsh integration: eval \"$(command ass --zsh)\"")
	}
	command := args[0]
	switch command {
	case "--help", "-h":
		_, err := fmt.Fprint(os.Stdout, usage)
		return err
	case "--zsh":
		executable, err := os.Executable()
		if err != nil {
			return err
		}
		return writeZsh(os.Stdout, executable)
	case "-a":
		return fmt.Errorf("enable zsh integration: eval \"$(command ass --zsh)\"")
	case "cache":
		c, err := settings()
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(os.Stdout, c.Cache)
		return err
	case "metadata", "projects", "import":
		return launchCommand(ctx, command, args[1:], os.Stdout)
	case "pick", "update", "search", "preview", "export", "client":
	default:
		return fmt.Errorf("unknown command %q; see ass --help", command)
	}
	if command == "client" {
		return client(ctx, args[1:])
	}
	fs := flag.NewFlagSet(command, flag.ContinueOnError)
	all := fs.Bool("all", false, "")
	query := fs.String("query", "", "")
	source := fs.String("source", "", "")
	id := fs.String("id", "", "")
	out := fs.String("out", "", "")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	c, err := settings()
	if err != nil {
		return err
	}
	s, err := openStore(c)
	if err != nil {
		return err
	}
	defer s.db.Close()
	switch command {
	case "update":
		start := time.Now()
		n, e := s.update(ctx)
		fmt.Fprintf(os.Stderr, "ass: %d sessions updated in %s\n", n, time.Since(start).Round(time.Millisecond))
		return e
	case "search":
		return s.search(ctx, *query, *all, os.Stdout)
	case "preview":
		return s.preview(ctx, *source, *id, *query, *all, os.Stdout)
	case "export":
		return s.export(ctx, *source, *id, *out)
	case "pick":
		return s.pick(ctx, *all, os.Stdout)
	}
	return nil
}
func main() {
	syscall.Umask(0077)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:]); err != nil {
		if err == errCanceled || ctx.Err() != nil {
			os.Exit(130)
		}
		fmt.Fprintln(os.Stderr, "ass:", err)
		os.Exit(1)
	}
}
