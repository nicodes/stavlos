SHELL := /bin/bash
.SHELLFLAGS := -eu -o pipefail -c
.DEFAULT_GOAL := help
.NOTPARALLEL:
.PHONY: help install lint test build check web dev stop clean
help:
	@echo 'make install: pinned toolchain and dependencies; make check: every existing gate and CLI build'
install:
	mise trust .mise.toml
	mise install go node python actionlint shellcheck
	mise exec -- go mod download
	mise exec -- npm --prefix web ci --no-audit --no-fund
lint:
	mise exec -- bash scripts/check.sh lint
	mise exec -- actionlint
	mise exec -- shellcheck scripts/check.sh
test:
	mise exec -- python3 scripts/check_gate_test.py
	mise exec -- bash scripts/check.sh test
build:
	mise exec -- bash scripts/check.sh build
check:
	mise exec -- python3 scripts/check_gate_test.py
	mise exec -- bash scripts/check.sh
	mise exec -- actionlint
	mise exec -- shellcheck scripts/check.sh
web:
	mise exec -- npm --prefix web ci --no-audit --no-fund
	mise exec -- npm --prefix web test
	mise exec -- npm --prefix web run build
dev stop:
	@echo '$@: unsupported: interactive CLI requires caller-owned configuration and sessions'
clean:
	mise exec -- python3 -c 'import shutil; [shutil.rmtree(p, ignore_errors=True) for p in (".artifacts", "web/node_modules")]'
