# llmgate and its jail. GNU make and BSD make alike; plain `make` prints the
# targets. `make jail` places the two binaries the Bastille template copies.

GOOS ?= freebsd
GOARCH ?= amd64
# FreeBSD names a Go by its release (pkg install go127), as LogWisp's make does.
GO_DEFAULT != uname -s | grep -q FreeBSD && sed -n 's/^go \([0-9]*\)\.\([0-9]*\).*/go\1\2/p' go.mod 2>/dev/null | grep . || echo go
GO ?= $(GO_DEFAULT)
# LOGWISP: a LogWisp checkout, where lw is built, by default the one beside
# this one. SCHEMA, optional: the schema.json of the engine the drafts are
# for, which lw must then match.
LOGWISP ?= ../logwisp
SCHEMA ?=
BIN := jail/usr/local/bin

help:
	@echo "make jail      llmgate and lw for the jail ($(GOOS)/$(GOARCH)) in $(BIN): [LOGWISP=] [SCHEMA=]"
	@echo "make llmgate   llmgate alone"
	@echo "make lw        lw from the LogWisp checkout LOGWISP, held to SCHEMA when it is given"
	@echo "make testdata  the tests' schema, from the lw make lw built"
	@echo "make check     gofmt, vet and the tests; the examples through lw once make lw ran"
	@echo "make clean     remove what these place"

jail: llmgate lw

llmgate:
	CGO_ENABLED=0 GOOS=$(GOOS) GOARCH=$(GOARCH) $(GO) build -trimpath -ldflags "-s -w" -o $(BIN)/llmgate .

# lw must speak the language of the engine the drafts are for, or the ones
# it passes would not load there: given SCHEMA, a native build's schema is held to it,
# byte for byte, before the jail's is built from the same tree.
lw:
	@test -n "$(LOGWISP)" && test -f "$(LOGWISP)/cmd/lw/main.go" || { echo "LOGWISP=$(LOGWISP):" \
		"name a LogWisp checkout, the one the drafts' engine is built from" >&2; exit 1; }
	@test -z "$(SCHEMA)" || test -f "$(SCHEMA)" || { echo "SCHEMA=$(SCHEMA): no such file" >&2; exit 1; }
	$(MAKE) -C $(LOGWISP) release GO=$(GO) GOOS= GOARCH= BIN_DIR="$$(pwd)/bin"
	@test -z "$(SCHEMA)" || bin/lw --schema | cmp -s - $(SCHEMA) || { \
		echo "LOGWISP=$(LOGWISP) is not the LogWisp of the drafts' engine:" >&2; \
		echo "  lw --schema says $$(bin/lw --schema | sed -n 's/^ *"version": *"\(.*\)".*/\1/p' | head -n 1)," \
			"$(SCHEMA) says $$(sed -n 's/^ *"version": *"\(.*\)".*/\1/p' $(SCHEMA) | head -n 1)" >&2; exit 1; }
	GOOS=$(GOOS) GOARCH=$(GOARCH) $(MAKE) -C $(LOGWISP) release GO=$(GO) BIN_DIR="$$(pwd)/$(BIN)"

testdata:
	@test -x bin/lw || { echo "no bin/lw: make lw LOGWISP=... first" >&2; exit 1; }
	bin/lw --schema > apps/lwdescribe/testdata/schema.json

check:
	@f=$$("$$($(GO) env GOROOT)/bin/gofmt" -l .); test -z "$$f" || { echo "$$f" >&2; exit 1; }
	$(GO) vet ./...
	if [ -x bin/lw ]; then LW="$$(pwd)/bin/lw" $(GO) test ./...; else $(GO) test ./...; fi

clean:
	rm -f $(BIN)/llmgate $(BIN)/lw bin/lw

.PHONY: help jail llmgate lw testdata check clean
