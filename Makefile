# sfync development tasks. Tests and benchmarks run entirely locally: test
# servers bind 127.0.0.1 only, use throwaway keys and temp dirs, and never
# touch ~/.ssh, ~/.config/sfync, ssh-agent, or launchd. See TESTING.md.

TESTENV  := testenv
RESULTS  := $(TESTENV)/bench/results
BENCHSTAT := go run golang.org/x/perf/cmd/benchstat@latest

# Benchmark knobs (override on the command line, e.g. make bench NETS=wan)
SERVERS  ?= openssh,ftp
NETS     ?= local,lan,wan
FIXTURES ?= website,mixed,deep,large
BENCH    ?= .
COUNT    ?= 6
TIMES    ?= 3x
NAME     ?= $(shell git rev-parse --short HEAD)$(shell git diff --quiet || echo -dirty)

BENCH_ENV = SFYNC_BENCH_SERVERS=$(SERVERS) SFYNC_BENCH_NETS=$(NETS) SFYNC_BENCH_FIXTURES=$(FIXTURES)

# Keep macOS from idle-sleeping during benchmarks: an iteration that spans a
# sleep is inflated by however long the machine slept, silently corrupting
# results. Empty where caffeinate doesn't exist.
CAFFEINATE := $(shell command -v caffeinate >/dev/null 2>&1 && echo caffeinate -i)

.PHONY: help build test test-integration test-all fixtures bench bench-quick bench-compare vet

help:
	@echo "make build             build ./sfync"
	@echo "make test              unit tests (race detector)"
	@echo "make test-integration  end-to-end tests vs real OpenSSH + in-process SFTP/FTP"
	@echo "make test-all          both"
	@echo "make fixtures          pre-generate benchmark file trees"
	@echo "make bench             full benchmark matrix -> $(RESULTS)/<NAME>.txt"
	@echo "make bench-quick       one pass, local network only"
	@echo "make bench-compare OLD=a NEW=b   benchstat two saved results"
	@echo ""
	@echo "bench knobs: SERVERS=$(SERVERS) NETS=$(NETS) FIXTURES=$(FIXTURES)"
	@echo "             BENCH=<regexp> COUNT=$(COUNT) TIMES=$(TIMES) NAME=$(NAME)"

build:
	go build -o sfync .

vet:
	go vet ./...
	cd $(TESTENV) && go vet ./...

test:
	go test -race -timeout 5m ./...

test-integration:
	cd $(TESTENV) && go test -race -timeout 10m ./...

test-all: test test-integration

fixtures:
	cd $(TESTENV) && go run ./cmd/genfixtures

bench: fixtures
	@mkdir -p $(RESULTS)
	cd $(TESTENV) && $(BENCH_ENV) $(CAFFEINATE) go test ./bench -run '^$$' -bench '$(BENCH)' \
		-benchtime $(TIMES) -count $(COUNT) -timeout 0 | tee bench/results/$(NAME).txt
	@echo "saved $(RESULTS)/$(NAME).txt"

bench-quick: fixtures
	cd $(TESTENV) && SFYNC_BENCH_SERVERS=$(SERVERS) SFYNC_BENCH_NETS=local \
		SFYNC_BENCH_FIXTURES=$(FIXTURES) $(CAFFEINATE) go test ./bench -run '^$$' -bench '$(BENCH)' -benchtime 2x

bench-compare:
	@test -n "$(OLD)" -a -n "$(NEW)" || { echo "usage: make bench-compare OLD=<name> NEW=<name>"; exit 2; }
	$(BENCHSTAT) $(RESULTS)/$(OLD).txt $(RESULTS)/$(NEW).txt
