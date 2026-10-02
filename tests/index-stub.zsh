#!/bin/zsh
if [[ "$1" == pick ]]; then
  print -r -- "$*" > "$ASS_TEST_LOG"
  printf '%s\t%s\t%s\t1\tbadge\ttitle\tsnippet\n' "$ASS_TEST_SOURCE_FILE" "$ASS_TEST_ID" "$ASS_TEST_MODE"
elif [[ "$1" == export ]]; then
  shift
  while (( $# )); do
    if [[ "$1" == --out ]]; then cp "$ASS_TEST_SOURCE_FILE" "$2"; exit; fi
    shift
  done
elif [[ "$1" == metadata ]]; then
  printf '/tmp\0ses_synthetic_123\0'
elif [[ "$1" == cache ]]; then
  print -r -- "$ASS_TEST_CACHE_PATH"
elif [[ "$1" == projects ]]; then
  printf -- '-\t%s\tSynthetic project\n' "$ASS_TEST_PROJECT_DIR"
elif [[ "$1" == import ]]; then
  printf '%s\n' "$@" > "$ASS_TEST_IMPORT_ARGS"
  print -r -- "$ASS_TEST_ID"
elif [[ "$1" == --help ]]; then
  print -r -- "$*" > "$ASS_TEST_LOG"
fi
