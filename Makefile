ARGS    ?=
MODULE  := $(shell go list -m)
VERSION ?= v0.1.0
BIN     := $(shell go env GOBIN)
ifeq ($(BIN),)
BIN     := $(shell go env GOPATH)/bin
endif

.PHONY: all build run test test-ui check guard publish install install-local update clean
.NOTPARALLEL:
.DEFAULT_GOAL := build

all: build publish

build:
	go build -buildvcs=false -o ass .

run: build
	zsh -f -c 'eval "$$(./ass --zsh)"; ass "$$@"' ass $(ARGS)

test:
	@test -z "$$(gofmt -l .)" || { echo "gofmt wants:"; gofmt -l .; exit 1; }
	go vet ./...
	go test -race ./...
	zsh -n ass.zsh
	zsh -f tests/shell.zsh

test-ui: build
	python3 tests/picker.py

check: test
	@for arch in arm64 amd64; do \
		printf 'darwin/%-6s ' $$arch; \
		GOOS=darwin GOARCH=$$arch go build -buildvcs=false -o /dev/null . || exit 1; echo ok; \
	done

guard:
	@git rev-parse --verify HEAD >/dev/null 2>&1 || { echo "no commits yet"; exit 1; }
	@test -z "$$(git status --porcelain)" || { echo "working tree is dirty:"; git status --short; exit 1; }
	@test "$$(git branch --show-current)" = master || { echo "not on master"; exit 1; }
	@git fetch -q origin
	@if git show-ref --verify --quiet refs/remotes/origin/master; then \
		git merge-base --is-ancestor origin/master HEAD || { echo "master is behind origin/master or has diverged"; exit 1; }; \
	fi
	@if git rev-parse -q --verify refs/tags/$(VERSION) >/dev/null; then echo "tag $(VERSION) already exists"; exit 1; fi

publish: guard check
	git tag $(VERSION)
	git push origin master
	git push origin $(VERSION)
	@curl -sf -o /dev/null $(PROXY)/@v/$(VERSION).info || true
	@echo "waiting for the proxy to pick up $(VERSION)"
	@for i in $$(seq 1 60); do \
		curl -s $(PROXY)/@v/list | grep -qx $(VERSION) && { echo "$(VERSION) is on the proxy"; exit 0; }; \
		sleep 5; \
	done; echo "the proxy never served $(VERSION)"; exit 1

install:
	go install $(MODULE)@latest
	@go version -m "$(BIN)/ass" | awk '$$1=="mod"{print "installed " $$2 " " $$3}'

update: install

install-local:
	go install -buildvcs=false .

clean:
	rm -f -- ass

PROXY := https://proxy.golang.org/$(MODULE)
