"""Exercise tool previews, selection and burst input using synthetic sessions."""
import fcntl
import json
import os
from pathlib import Path
import pty
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


def pick(base, cache, env, cancel):
    reader, writer = os.pipe()
    pid, terminal = pty.fork()
    if pid == 0:
        os.close(reader)
        os.dup2(writer, 1)
        os.close(writer)
        os.chdir(base)
        os.execve(str(binary), [str(binary), "pick"] + ([] if cancel else ["--all"]), env)
    os.close(writer)
    fcntl.ioctl(terminal, termios.TIOCSWINSZ, struct.pack("HHHH", 30, 120, 0, 0))
    status = None
    query = "missing-session-0123456789abcdef" if cancel else "tool-preview-output"
    stage = 0
    screen = b""
    tool_preview = False
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
                tool_preview |= b"T: tool-preview-output" in screen
                if b"\x1b[6n" in data:
                    os.write(terminal, b"\x1b[1;1R")
            state = picker_state(cache)
            if stage == 0 and state.get("matchCount") == 1:
                os.write(terminal, query.encode())
                stage = 1
            elif stage == 1 and state.get("query") == query and state.get("matchCount") == (0 if cancel else 1):
                if cancel or tool_preview:
                    os.write(terminal, b"\x1b" if cancel else b"\r")
                    stage = 2
            done, result = os.waitpid(pid, os.WNOHANG)
            if done:
                status = result
                break
    finally:
        if status is None:
            os.killpg(pid, signal.SIGKILL)
            _, status = os.waitpid(pid, 0)
        os.close(terminal)
        with os.fdopen(reader, "rb") as output:
            selected = output.read()
    assert stage == 2, "picker did not load the session or lost the typed query"
    assert cancel or tool_preview, "all-mode preview omitted the matching tool output"
    assert os.waitstatus_to_exitcode(status) == (130 if cancel else 0), "wrong picker exit status"
    return selected.decode()


with tempfile.TemporaryDirectory(prefix="ass-picker-") as temporary:
    base = Path(temporary).resolve()
    source = base / "codex '\u044e" / "sessions" / "fixture.jsonl"
    source.parent.mkdir(parents=True, mode=0o700)
    source.write_text("\n".join(json.dumps(row) for row in [
        {"type": "session_meta", "payload": {"id": session_id, "cwd": str(base)}},
        {"type": "response_item", "payload": {"type": "message", "role": "user", "content": [{"type": "input_text", "text": "synthetic picker test"}]}},
        {"type": "response_item", "payload": {"type": "function_call_output", "output": "tool-preview-output"}},
    ]) + "\n")
    source.chmod(0o600)
    options = base / "fzfrc"
    options.write_text("--read0 --print0\n")
    env = dict(os.environ, CODEX_HOME="codex '\u044e", CLAUDE_CONFIG_DIR="missing-claude",
               XDG_CACHE_HOME="cache '\u044e", ASS_CURSOR_DB="missing-cursor.db", ASS_OPENCODE_DB="missing-opencode.db",
               TERM="xterm-256color", FZF_API_KEY="synthetic-test",
               FZF_DEFAULT_OPTS="--print-query --accept-nth=2", FZF_DEFAULT_OPTS_FILE=str(options))
    cache = Path(subprocess.check_output([binary, "cache"], cwd=base, env=env, text=True).rstrip("\n"))
    assert cache == base / "cache '\u044e" / "cs" and not cache.exists(), "cache lookup changed its identity or created files"
    subprocess.run([binary, "update"], cwd=base, env=env, check=True, capture_output=True)
    for cancel in (False, True):
        selected = pick(base, cache, env, cancel)
        if cancel:
            assert selected == "", "Escape returned a selection"
        else:
            assert selected.count("\n") == 1 and "\0" not in selected, "fzf defaults changed the output framing"
            fields = selected.rstrip("\n").split("\t")
            assert len(fields) == 7 and fields[:3] == [str(source), session_id, "codex"], "fzf defaults changed the selected session"
        assert not list(cache.glob("run-*")), "picker left its sockets behind"
print("fzf tool preview/selection/query/Escape tests passed")
