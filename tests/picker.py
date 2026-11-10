"""Exercise startup, tool previews and burst input using synthetic sessions."""
import fcntl
import json
import os
from pathlib import Path
import pty
import re
import select
import signal
import socket
import struct
import subprocess
import tempfile
import termios
import time

binary = Path(__file__).resolve().parents[1] / "ass"
session_id = "12345678-1234-1234-1234-123456789abc"


def picker_state(cache):
    for run in cache.glob("run-*"):
        previous = os.getcwd()
        try:
            os.chdir(run)
            with socket.socket(socket.AF_UNIX) as conn:
                conn.settimeout(0.2)
                conn.connect("ui.sock")
                conn.sendall(b"GET /?limit=0 HTTP/1.1\r\nHost: localhost\r\nX-API-Key: synthetic-test\r\nConnection: close\r\n\r\n")
                response = b""
                while True:
                    chunk = conn.recv(65536)
                    if not chunk:
                        break
                    response += chunk
                if response.startswith(b"HTTP/1.1 200"):
                    return json.loads(response.split(b"\r\n\r\n", 1)[1])
        except OSError:
            pass
        finally:
            os.chdir(previous)
    return {}


def pick(base, cache, env, cancel, update_lock=None):
    reader, writer = os.pipe()
    pid, terminal = pty.fork()
    if pid == 0:
        os.close(reader)
        os.dup2(writer, 1)
        os.close(writer)
        os.chdir(base)
        os.execve(str(binary), [str(binary), "pick"] + ([] if cancel else ["--all"]), env)
    os.close(writer)
    fcntl.ioctl(terminal, termios.TIOCSWINSZ, struct.pack("HHHH", 30, 80, 0, 0))
    status = None
    query = "missing-session-0123456789abcdef" if cancel else "@codex tool-preview-output"
    stage = 0
    screen = b""
    chat_preview = False
    copy_hint = False
    compact_row = False
    preview_error = False
    loading_markers = (b"|\\ ||", b"/| |\\", b"__-__-__-", b"_-__-__-_")
    seen_markers = set()
    loading_label = False
    try:
        deadline = time.monotonic() + 15
        while time.monotonic() < deadline:
            ready, _, _ = select.select([terminal], [], [], 0.05)
            if ready:
                try:
                    data = os.read(terminal, 65536)
                except OSError:
                    data = b""
                screen = (screen + data)[-16384:]
                compact_row |= "\u25c6 synthetic picker test".encode() in re.sub(rb"\x1b\[[0-9;]*m", b"", screen)
                copy_hint |= b"Ctrl-Y copies chat link/ID" in screen
                chat_preview |= all(text in screen for text in (
                    b"YOU", b"CODEX", b"TOOL", b"tool-preview-output", b"boundarytoken"
                )) and b"abc boundarytoken" not in screen
                preview_error |= b"Preview unavailable:" in screen or b"sql: no rows" in screen
                loading_label |= all(text in screen for text in (
                    b"Loading chats", b"Building cache.", b"First run may take a bit."
                ))
                seen_markers.update(marker for marker in loading_markers if marker in screen)
                if b"\x1b[6n" in data:
                    os.write(terminal, b"\x1b[1;1R")
            state = picker_state(cache)
            source_hint = b"Tab  @codex @claude" in screen and b"@cursor @opencode" in screen
            if stage == 0 and state.get("matchCount") == (1 if cancel else 0) and (cancel or source_hint):
                os.write(terminal, query.encode() if cancel else b"@c\t")
                stage = 3 if cancel else 1
            elif stage == 1 and state.get("query") == "@c" and b"Try  @codex @claude @cursor" in screen:
                os.write(terminal, b"o\t")
                stage = 2
            elif stage == 2 and state.get("query") == "@codex ":
                os.write(terminal, b"tool-preview-output")
                stage = 3
            elif stage == 3 and update_lock and state.get("query") == query and state.get("matchCount") == 0:
                # Hold the refresh until both animation and input have progressed.
                if loading_label and len(seen_markers) == len(loading_markers):
                    if cancel:
                        os.write(terminal, b"\x1b")
                        stage = 5
                    else:
                        fcntl.flock(update_lock, fcntl.LOCK_UN)
                        update_lock.close()
                        update_lock = None
            elif stage == 3 and state.get("query") == query and state.get("matchCount") == (0 if cancel else 1):
                if cancel:
                    os.write(terminal, b"\x1b")
                    stage = 5
                elif chat_preview:
                    os.write(terminal, b"\x19")
                    stage = 4
            elif stage == 4 and b"Copied chat reference" in screen:
                os.write(terminal, b"\r")
                stage = 5
            done, result = os.waitpid(pid, os.WNOHANG)
            if done:
                status = result
                break
    finally:
        if update_lock:
            update_lock.close()
        if status is None:
            os.killpg(pid, signal.SIGKILL)
            _, status = os.waitpid(pid, 0)
        os.close(terminal)
        with os.fdopen(reader, "rb") as output:
            selected = output.read()
    assert stage == 5, f"picker completion, query, copy or selection stalled at stage {stage}"
    assert loading_label and len(seen_markers) == len(loading_markers), "startup walk or road did not animate"
    assert cancel or chat_preview, "preview lost role labels or tool output, or failed to wrap at half width"
    assert cancel or copy_hint, "preview omitted the copy shortcut label"
    assert cancel or compact_row, "picker put more than one space between the badge and title"
    assert not preview_error, "empty selection displayed a preview error"
    assert os.waitstatus_to_exitcode(status) == (130 if cancel else 0), "wrong picker exit status"
    return selected.decode()


with tempfile.TemporaryDirectory(prefix="ass-picker-") as temporary:
    base = Path(temporary).resolve()
    source = base / "codex '\u044e" / "sessions" / "fixture.jsonl"
    source.parent.mkdir(parents=True, mode=0o700)
    source.write_text("\n".join(json.dumps(row) for row in [
        {"type": "session_meta", "payload": {"id": session_id, "cwd": str(base)}},
        {"type": "response_item", "payload": {"type": "message", "role": "user", "content": [{"type": "input_text", "text": "synthetic picker test"}]}},
        {"type": "response_item", "payload": {"type": "message", "role": "assistant", "content": [{"type": "output_text", "text": "abc " * 7 + "boundarytoken"}]}},
        {"type": "response_item", "payload": {"type": "function_call_output", "output": "tool-preview-output"}},
    ]) + "\n")
    source.chmod(0o600)
    options = base / "fzfrc"
    options.write_text("--read0 --print0\n")
    clipboard = base / "clipboard"
    bin_dir = base / "bin"
    bin_dir.mkdir()
    stub = bin_dir / "pbcopy"
    stub.write_text('#!/bin/sh\n/bin/cat > "$ASS_TEST_CLIPBOARD"\n')
    stub.chmod(0o700)
    (bin_dir / "wl-copy").symlink_to(stub)
    env = dict(os.environ, CODEX_HOME="codex '\u044e", CLAUDE_CONFIG_DIR="missing-claude",
               XDG_CACHE_HOME="cache '\u044e", ASS_CURSOR_DB="missing-cursor.db", ASS_OPENCODE_DB="missing-opencode.db",
               TERM="xterm-256color", FZF_API_KEY="synthetic-test",
               PATH=str(bin_dir) + os.pathsep + os.environ["PATH"], ASS_TEST_CLIPBOARD=str(clipboard),
               FZF_DEFAULT_OPTS="--print-query --accept-nth=2", FZF_DEFAULT_OPTS_FILE=str(options))
    cache = Path(subprocess.check_output([binary, "cache"], cwd=base, env=env, text=True).rstrip("\n"))
    assert cache == base / "cache '\u044e" / "cs" and not cache.exists(), "cache lookup changed its identity or created files"
    cache.mkdir(parents=True, mode=0o700)
    for cancel in (False, True):
        update_lock = os.fdopen(os.open(cache / "index.lock", os.O_WRONLY | os.O_CREAT, 0o600), "w")
        fcntl.flock(update_lock, fcntl.LOCK_EX)
        selected = pick(base, cache, env, cancel, update_lock)
        if cancel:
            assert selected == "", "Escape returned a selection"
        else:
            assert clipboard.read_text() == "codex://threads/" + session_id, "wrong clipboard content"
            assert selected.count("\n") == 1 and "\0" not in selected, "fzf defaults changed the output framing"
            fields = selected.rstrip("\n").split("\t")
            assert len(fields) == 7 and fields[:3] == [str(source), session_id, "codex"], "fzf defaults changed the selected session"
        assert not list(cache.glob("run-*")), "picker left its sockets behind"
print("fzf startup animation/Tab completion/tool preview/copy/selection/query/locked Escape tests passed")
