#!/bin/zsh
# Launches no agents or apps. Menu/import functions and executables are stubbed.
emulate -LR zsh
setopt err_return
repo=${0:A:h:h}
source "$repo/ass.zsh"
local_root=$(mktemp -d "${TMPDIR:-/tmp}/ass-test.XXXXXXXX")
local_root=${local_root:A}
trap 'cd /; rm -rf -- "$local_root"' EXIT
mkdir -p "$local_root/project" "$local_root/resolved cache/cs" "$local_root/bin"
export XDG_CACHE_HOME=$local_root/cache
export ASS_TEST_CACHE_PATH="$local_root/resolved cache/cs"
export ASS_TEST_PROJECT_DIR=$local_root/project
unset ASS_DEFAULT_AGENT ASS_CODEX_BIN
export FZF_DEFAULT_OPTS='--print-query --accept-nth=2'
export FZF_DEFAULT_OPTS_FILE=$local_root/fzfrc
print -- '--read0 --print0' > "$FZF_DEFAULT_OPTS_FILE"
test_id=12345678-1234-1234-1234-123456789abc
unicode_char=$'\u044e'
test_file=$repo/tests/fixtures/session.jsonl
test_mode=codex
test_choice=''
test_cancel=0
test_call=''
# ass requires an executable, so use this fixture helper script.
_ASS_BIN=$repo/tests/index-stub.zsh
export ASS_TEST_LOG=$local_root/index-args
export ASS_TEST_SOURCE_FILE=$test_file ASS_TEST_ID=$test_id ASS_TEST_MODE=$test_mode
export ASS_OPENCODE_BIN=$repo/tests/opencode-stub.zsh
export ASS_TEST_LAUNCH_LOG=$local_root/native-launch
export ASS_TEST_IMPORT_ARGS=$local_root/import-args
export ASS_TEST_CODEX_LOG=$local_root/codex-launch
fzf() {
  [[ -z "$FZF_DEFAULT_OPTS" && -z "$FZF_DEFAULT_OPTS_FILE" ]] || {
    print -u2 -- 'fzf inherited output-changing defaults'; return 1
  }
  cat > "$local_root/menu"
  (( test_cancel )) && return 130
  if [[ -n "$test_choice" ]]; then
    print -r -- "$test_choice"
  else
    head -n 1 "$local_root/menu"
  fi
}
codex() { test_call="codex:$*"; }
claude() { test_call="claude:$*"; test_claude_home=$CLAUDE_CONFIG_DIR; }
_ass_codex_app_open() { test_call="app:$1"; }
_ass_choose_project /tmp "$local_root"
[[ "$REPLY" == $'-\t'"$local_root/project" ]] || { print -u2 -- 'project menu damaged its row'; exit 1; }
imported=$(_ass_import cursor "$test_file" "$local_root/project" -)
[[ "$imported" == "$test_id" && "$(< "$ASS_TEST_IMPORT_ARGS")" == $'import\n--source\ncursor\n--file\n'"$test_file"$'\n--cwd\n'"$local_root/project"$'\n--project\n-' ]] || {
  print -u2 -- 'import arguments changed'; exit 1
}
_ass_choose_project() { REPLY=$'-\t'"$local_root/project"; }
_ass_import() { [[ -f "$2" && "$3" == "$local_root/project" && "$4" == - ]] || return 1; print -r -- "$*" >> "$local_root/imports"; print -r -- "$test_id"; }
jq() { print -u2 -- 'jq must not be called'; return 99; }
check() { [[ "$test_call" == "$1" ]] || { print -u2 -- "$ASS_TEST_MODE launched $test_call, expected $1"; return 1; }; }
menu_is() { [[ "$(< "$local_root/menu")" == "$1" ]] || { print -u2 -- "$ASS_TEST_MODE has wrong opener choices"; return 1; }; }

# Enter opens every supported source in Codex.app. Only foreign sources import.
for source_kind in codex claude cursor opencode; do
  export ASS_TEST_MODE=$source_kind
  : > "$local_root/imports"
  ass
  check "app:$test_id"
  case $source_kind in
    codex|cursor) menu_is $'codex app\ncodex' ;;
    claude) menu_is $'codex app\ncodex\nclaude' ;;
    opencode) menu_is $'codex app\ncodex\nopencode' ;;
  esac
  if [[ "$source_kind" == codex ]]; then
    [[ ! -s "$local_root/imports" ]] || { print -u2 -- 'native Codex session was imported again'; exit 1; }
  else
    [[ -s "$local_root/imports" ]] || { print -u2 -- "$source_kind skipped the Codex import"; exit 1; }
  fi
done
[[ "$(< "$ASS_TEST_LOG")" == 'pick' ]] || { print -u2 -- 'default picker flags changed'; exit 1; }

# Explicit CLI choices keep the exact launch arguments.
test_choice=codex
for source_kind in codex claude cursor opencode; do
  export ASS_TEST_MODE=$source_kind
  ass
  check "codex:resume -- $test_id"
done
export ASS_TEST_MODE=claude
test_choice=claude
ass
check "claude:--resume $test_id"
export ASS_TEST_MODE=opencode
test_choice=opencode
: > "$local_root/imports"
ass
[[ "$(< "$ASS_TEST_LAUNCH_LOG")" == 'opencode:/tmp --session ses_synthetic_123' ]] || { print -u2 -- 'native OpenCode launch flags changed'; exit 1; }
[[ ! -s "$local_root/imports" ]] || { print -u2 -- 'native OpenCode opening imported into Codex'; exit 1; }

# Relative executable overrides remain anchored to the caller when ass changes cwd.
cat > "$local_root/bin/codex '$unicode_char" <<'STUB'
#!/bin/zsh
printf '%s\n' "$PWD" "$CODEX_HOME" "$XDG_CACHE_HOME" "$@" > "$ASS_TEST_CODEX_LOG"
STUB
chmod 700 "$local_root/bin/codex '$unicode_char"
export ASS_CODEX_BIN="bin/codex '$unicode_char"
export CODEX_HOME=profiles/codex CLAUDE_CONFIG_DIR=profiles/claude XDG_DATA_HOME=profiles/data XDG_CACHE_HOME=cache
test_choice=codex
for source_kind in codex claude cursor opencode; do
  cd -- "$local_root"
  export ASS_TEST_MODE=$source_kind
  ass
  expected_cwd=$local_root/project
  [[ "$source_kind" == codex ]] && expected_cwd=/tmp
  [[ "$(< "$ASS_TEST_CODEX_LOG")" == "$expected_cwd"$'\n'"$local_root/profiles/codex"$'\n'"$local_root/cache"$'\nresume\n--\n'"$test_id" ]] || {
    print -u2 -- "$source_kind ignored the Codex executable override: $(< "$ASS_TEST_CODEX_LOG") (cwd: $expected_cwd)"; exit 1
  }
done
unset ASS_CODEX_BIN
export ASS_TEST_MODE=claude
test_choice=claude
cd -- "$local_root"
ass
[[ "$test_claude_home" == "$local_root/profiles/claude" ]] || { print -u2 -- 'relative Claude home was lost after cd'; exit 1; }
cp "$repo/tests/opencode-stub.zsh" "$local_root/bin/opencode '$unicode_char"
print -r -- 'print -r -- "$XDG_DATA_HOME" > "$ASS_TEST_CODEX_LOG"' >> "$local_root/bin/opencode '$unicode_char"
chmod 700 "$local_root/bin/opencode '$unicode_char"
export ASS_OPENCODE_BIN="bin/opencode '$unicode_char"
export ASS_TEST_MODE=opencode
test_choice=opencode
cd -- "$local_root"
ass
[[ "$(< "$ASS_TEST_LAUNCH_LOG")" == 'opencode:/tmp --session ses_synthetic_123' ]] || {
  print -u2 -- 'relative OpenCode executable was lost after cd'; exit 1
}
[[ "$(< "$ASS_TEST_CODEX_LOG")" == "$local_root/profiles/data" ]] || { print -u2 -- 'relative OpenCode data directory was lost after cd'; exit 1; }
[[ "$CODEX_HOME" == profiles/codex && "$CLAUDE_CONFIG_DIR" == profiles/claude && "$XDG_DATA_HOME" == profiles/data && "$XDG_CACHE_HOME" == cache ]] || {
  print -u2 -- 'launcher changed the caller source directories'; exit 1
}

# Preferences reorder available choices, never add an unsupported destination.
test_choice=''
export ASS_TEST_MODE=claude
export ASS_DEFAULT_AGENT=claude
ass
check "claude:--resume $test_id"
menu_is $'claude\ncodex app\ncodex'
export ASS_TEST_MODE=codex
ass
check "app:$test_id"
menu_is $'codex app\ncodex'
export ASS_DEFAULT_AGENT=codex
ass
check "codex:resume -- $test_id"
menu_is $'codex\ncodex app'
export ASS_DEFAULT_AGENT=unavailable
ass
check "app:$test_id"
menu_is $'codex app\ncodex'
unset ASS_DEFAULT_AGENT
export ASS_TEST_MODE=opencode
export ASS_OPENCODE_BIN=$local_root/missing-opencode
ass -a
check "app:$test_id"
menu_is $'codex app\ncodex'
[[ "$(< "$ASS_TEST_LOG")" == 'pick --all' ]] || { print -u2 -- '-a no longer enables all sessions'; exit 1; }

# Cancel launches nothing and still removes the selected temporary export.
test_call=''
test_cancel=1
if ass; then print -u2 -- 'canceled opener returned success'; exit 1; fi
check ''
ass --help
check ''
[[ "$(< "$ASS_TEST_LOG")" == '--help' ]] || { print -u2 -- 'help opened the picker'; exit 1; }
remaining=( "$ASS_TEST_CACHE_PATH"/selection.*(N) )
(( $#remaining == 0 )) || { print -u2 -- 'selection export leaked'; exit 1; }
[[ "$FZF_DEFAULT_OPTS" == '--print-query --accept-nth=2' && "$FZF_DEFAULT_OPTS_FILE" == "$local_root/fzfrc" ]] || {
  print -u2 -- 'launcher changed the caller fzf defaults'; exit 1
}
print 'zsh launch/import/menu tests passed'
