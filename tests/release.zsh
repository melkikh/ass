#!/bin/zsh
# Uses a disposable Git repository; never publishes or contacts a remote.
emulate -LR zsh
setopt err_return
repo=${0:A:h:h}
local_root=$(mktemp -d "${TMPDIR:-/tmp}/ass-release.XXXXXXXX")
trap 'cd /; rm -rf -- "$local_root"' EXIT
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1
unset VERSION MAKEFLAGS MFLAGS MAKEOVERRIDES
git -C "$local_root" init -q
git -C "$local_root" -c user.name=Fixture -c user.email=fixture@example.test -c commit.gpgsign=false commit -q --allow-empty -m fixture
cd "$local_root"
check_version() {
  local expected=$1
  shift
  local actual=$(make --no-print-directory -s -f "$repo/Makefile" MODULE=example.test/fixture BIN="$local_root/bin" version "$@")
  [[ "$actual" == "$expected" ]] || { print -u2 -- "version: expected $expected, got $actual"; exit 1; }
}
check_version v0.1.0
git tag v0.1.0
check_version v0.1.1
git tag v0.1.9
git tag v0.1.10
git tag v9.0.0-rc.1
git tag docs/v9.0.0
check_version v0.1.11
git tag v0.2.0
check_version v0.2.1
check_version v1.0.0 VERSION=v1.0.0
before=$(git show-ref)
make --no-print-directory -n -f "$repo/Makefile" MODULE=example.test/fixture BIN="$local_root/bin" publish > "$local_root/dry-run"
[[ "$(git show-ref)" == "$before" ]] || { print -u2 -- 'dry run changed release refs'; exit 1; }
print 'release version tests passed'
