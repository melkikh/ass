# Emitted by ass --zsh, which pins _ASS_BIN to its own executable.

# Session ids read from JSON are data, but values beginning with '-' would be
# parsed as CLI options. Native Claude and Codex session ids are UUIDs, so reject
# anything else before starting either agent.
_ass_valid_session_id() {
  local id=$1
  local uuid_re='^[0-9A-Fa-f]{8}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{12}$'
  [[ "$id" =~ $uuid_re ]]
}

# Opens a native Codex thread in the desktop app. The app owns
# the codex:// URL scheme; targeting its bundle id avoids depending on the
# displayed application name, which has changed between releases.
_ass_codex_app_open() {
  emulate -L zsh

  local id=$1
  if ! _ass_valid_session_id "$id"; then
    print -u2 -- 'ass: invalid Codex thread id'
    return 1
  fi
  if [[ "$OSTYPE" != darwin* ]]; then
    print -u2 -- 'ass: Codex app opening is only supported on macOS'
    return 1
  fi

  command open -b com.openai.codex -u "codex://threads/$id" || {
    print -u2 -- 'ass: could not open Codex app; check the desktop app installation'
    return 1
  }
}

_ass_import() {
  "$_ASS_BIN" import --source "$1" --file "$2" --cwd "$3" --project "$4"
}

_ass_projects() {
  "$_ASS_BIN" projects --original "$1" --current "$2"
}

_ass_fzf() {
  emulate -L zsh
  # Output-changing defaults would corrupt the rows consumed by the launcher.
  local -x FZF_DEFAULT_OPTS='' FZF_DEFAULT_OPTS_FILE=''
  fzf "$@"
}

# Returns project-id<TAB>absolute-directory in REPLY, without changing cwd.
_ass_choose_project() {
  emulate -L zsh
  local rows selected
  rows=$(_ass_projects "$1" "$2") || return
  selected=$(print -r -- "$rows" | _ass_fzf --height=12 --layout=reverse --no-multi \
    --delimiter=$'\t' --with-nth=3,2 --prompt='import into> ') || return
  [[ -n "$selected" ]] || return 1
  REPLY="${selected%$'\t'*}"
}

_ass_choose_agent() {
  emulate -L zsh
  local preferred="${ASS_DEFAULT_AGENT:-codex app}" choice
  local -a choices
  for choice in "$@"; do
    [[ "$choice" == "$preferred" ]] && choices+=( "$choice" )
  done
  for choice in "$@"; do
    [[ "$choice" != "$preferred" ]] && choices+=( "$choice" )
  done
  printf '%s\n' "${choices[@]}" |
    _ass_fzf --height=$(( $#choices + 3 )) --layout=reverse --no-multi --prompt='open with> '
}

function ass {
  # Usage: ass [-a] -- include untitled/internal sessions and tool traffic.
  emulate -L zsh
  setopt local_options local_traps null_glob
  if [[ "$1" == --help || "$1" == -h || "$1" == --zsh ]]; then
    "$_ASS_BIN" "$@"
    return
  fi
  # Native agents inherit these after cd; relative overrides belong to the caller.
  local setting setting_value
  for setting in CODEX_HOME CLAUDE_CONFIG_DIR XDG_DATA_HOME XDG_CACHE_HOME; do
    setting_value=${(P)setting}
    [[ -n "$setting_value" ]] && local -x "$setting=${setting_value:a}"
  done
  local caller_cwd=$PWD
  local cache_root codex_bin=${ASS_CODEX_BIN:-codex}
  [[ "$codex_bin" == */* ]] && codex_bin=${codex_bin:a}
  local source_agent pick
  local -a index_options
  [[ "$1" == "-a" ]] && index_options+=( --all )
  [[ -x "$_ASS_BIN" ]] || {
    print -u2 -- "ass: indexer not found: $_ASS_BIN"
    return 1
  }
  cache_root=$("$_ASS_BIN" cache) || return
  pick=$("$_ASS_BIN" pick "${index_options[@]}") || return
  [[ -n "$pick" ]] || return

  local file="${pick%%$'\t'*}" rest="${pick#*$'\t'}"
  local id="${rest%%$'\t'*}"
  rest="${rest#*$'\t'}"
  source_agent="${rest%%$'\t'*}"
  if ! _ass_valid_session_id "$id"; then
    print -u2 -- 'ass: invalid session id in the selected session file'
    return 1
  fi
  if [[ "$source_agent" == "cursor" || "$source_agent" == "opencode" ]]; then
    # Materialize only the chosen conversation, and remove it when ass returns.
    # The source databases themselves are opened read-only by the indexer.
    local selection_dir
    selection_dir=$(mktemp -d "$cache_root/selection.XXXXXXXX") || return
    file="$selection_dir/$id.jsonl"
    trap "command rm -f -- ${(q)file}; command rmdir -- ${(q)selection_dir} 2>/dev/null" EXIT
    "$_ASS_BIN" export --source "$source_agent" --id "$id" --out "$file" || return
    local source_cwd imported_id source_choice metadata
    local -a meta
    local source_label=Cursor opencode_bin native_id
    local source_project_id project_cwd REPLY
    local -a source_choices
    source_choices=( 'codex app' codex )
    if [[ "$source_agent" == "opencode" ]]; then
      source_label=OpenCode
      opencode_bin=${ASS_OPENCODE_BIN:-opencode}
      [[ "$opencode_bin" == */* ]] && opencode_bin=${opencode_bin:a}
      if command -v "$opencode_bin" >/dev/null 2>&1; then
        source_choices+=( opencode )
      fi
    fi
    source_choice=$(_ass_choose_agent "${source_choices[@]}") || return
    metadata=$("$_ASS_BIN" metadata --source "$source_agent" --file "$file") || return
    meta=("${(@0)metadata}")
    source_cwd=$meta[1]
    if [[ "$source_choice" == "opencode" ]]; then
      native_id=$meta[2]
      [[ "$native_id" =~ '^ses_[A-Za-z0-9_-]+$' ]] || {
        print -u2 -- 'ass: invalid native OpenCode session id'
        return 1
      }
      [[ -d "$source_cwd" ]] || {
        print -u2 -- 'ass: OpenCode session directory no longer exists'
        return 1
      }
      cd -- "$source_cwd" || return
      command "$opencode_bin" "$source_cwd" --session "$native_id"
      return
    fi
    [[ "$source_choice" == "codex" || "$source_choice" == "codex app" ]] || return 1
    _ass_choose_project "$source_cwd" "$caller_cwd" || return
    source_project_id=${REPLY%%$'\t'*}
    project_cwd=${REPLY#*$'\t'}
    print -u2 -- "ass: importing $source_label conversation into Codex..."
    imported_id=$(_ass_import "$source_agent" "$file" "$project_cwd" "$source_project_id") || return
    if ! _ass_valid_session_id "$imported_id"; then
      print -u2 -- 'ass: Codex import returned an invalid thread id'
      return 1
    fi
    if [[ "$source_choice" == "codex app" ]]; then
      _ass_codex_app_open "$imported_id"
    else
      [[ -d "$project_cwd" ]] && cd -- "$project_cwd"
      "$codex_bin" resume -- "$imported_id"
    fi
    return
  fi
  local cwd metadata
  local -a meta
  metadata=$("$_ASS_BIN" metadata --source "$source_agent" --file "$file" 2>/dev/null) || metadata=''
  meta=("${(@0)metadata}")
  cwd=$meta[1]
  if [[ "$source_agent" == "codex" ]]; then
    local codex_choice
    codex_choice=$(_ass_choose_agent 'codex app' codex) || return
    if [[ "$codex_choice" == 'codex app' ]]; then
      _ass_codex_app_open "$id"
    elif [[ "$codex_choice" == codex ]]; then
      [[ -n "$cwd" && -d "$cwd" ]] && cd -- "$cwd"
      "$codex_bin" resume -- "$id"
    else
      return 1
    fi
    return
  fi

  local agent
  agent=$(_ass_choose_agent 'codex app' codex claude) || return

  if [[ "$agent" == "claude" ]]; then
    [[ -n "$cwd" && -d "$cwd" ]] && cd -- "$cwd"
    claude --resume "$id"
    return
  fi

  local codex_id project_id project_cwd REPLY
  _ass_choose_project "$cwd" "$caller_cwd" || return
  project_id=${REPLY%%$'\t'*}
  project_cwd=${REPLY#*$'\t'}
  codex_id=$(_ass_import claude "$file" "$project_cwd" "$project_id") || return
  if ! _ass_valid_session_id "$codex_id"; then
    print -u2 -- 'ass: Codex import returned an invalid thread id'
    return 1
  fi
  if [[ "$agent" == "codex app" ]]; then
    _ass_codex_app_open "$codex_id"
  else
    cd -- "$project_cwd" || return
    "$codex_bin" resume -- "$codex_id"
  fi
}
