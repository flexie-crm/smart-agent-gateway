# Flexie SAG, single entry point for development and CI.
#
# The pipeline calls these same targets, one per step, so "works locally" and
# "passes the pipeline" run the same code. The two lists are not identical and
# say why: `make ci` is what to run before calling a change done, and the
# pipeline adds `test-race`, `build` and `docker-build` on top of it.

ORCHESTRATOR := orchestrator
CHAT_UI := chat-ui
ADMIN_UI := admin-ui
GOLANGCI_VERSION := v2.12.2
AIR_VERSION := v1.63.0

# The Go toolchain comes from go.mod, so there is one place to bump it.
# golangci-lint refuses to analyse a module targeting a newer Go than the
# one it was built with, and `go install` would otherwise build it with the
# older toolchain the linter itself requires. Forcing GOTOOLCHAIN fixes
# that, and stamping the binary path with both versions means a bump to
# either one installs a fresh binary instead of silently reusing a stale,
# incompatible one.
GO_TOOLCHAIN := $(shell awk '/^go /{print "go"$$2}' $(ORCHESTRATOR)/go.mod)
TOOLS_DIR := $(CURDIR)/.tools
LINT_BIN := $(TOOLS_DIR)/golangci-lint-$(GOLANGCI_VERSION)-$(GO_TOOLCHAIN)
AIR_BIN := $(TOOLS_DIR)/air-$(AIR_VERSION)


# Point at a scratch database to run the store and API suites. Without it
# they skip, and only the pure-unit tests run.
export SAG_TEST_DSN ?=

# The same, for the SQL Server driver and the query tool's policy on it. It
# carries more weight than the other two: this dialect's parser is not the
# server's own, so it is the only place the two readings can be shown to agree.
export SAG_TEST_MSSQL_DSN ?=

# The same, for the PostgreSQL driver and the query tool's policy on it.
# Those suites are the only ones that can find out whether this side reads a
# statement the way the server does, so a run without this proves less than
# it looks like it does.
export SAG_TEST_PG_DSN ?=

# An SSH bastion and a database only it can reach, for the end-to-end tunnel
# test. Without both, that test skips: it cannot tell a working tunnel from no
# tunnel at all unless the database is genuinely out of reach otherwise.
export SAG_TEST_TUNNEL ?=
export SAG_TEST_TUNNEL_DB ?=

.PHONY: help
help: ## Show the available targets
	@grep -hE '^[a-z-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'

.PHONY: ci
# The BUILDS are in here on purpose. Unit tests run in jsdom, which never
# processes the CSS and never resolves an asset: a missing stylesheet import is
# invisible to them and fatal in a browser. That is exactly what happened, so the
# thing that would have caught it now runs every time.
# `tidy-check` is in here because it is nearly free and catches a go.mod that
# drifted, which is otherwise found by the pipeline after the push. `test-race`
# is NOT: it roughly doubles the suite, the pipeline runs it, and a change that
# touches concurrency is a change whose author should run `make test-race`
# deliberately rather than pay for it on every unrelated edit.
ci: fmt-check tidy-check vet lint test ui-lint ui-test ui-build admin-lint admin-test admin-build ## Backend, chat UI and console: what to run before calling a change done

.PHONY: build
build: ## Build the orchestrator binary
	cd $(ORCHESTRATOR) && go build -o bin/sag ./cmd/sag

# --- chat ui ---------------------------------------------------------------

$(CHAT_UI)/node_modules:
	cd $(CHAT_UI) && npm ci

.PHONY: ui-lint
ui-lint: $(CHAT_UI)/node_modules ## Typecheck and lint the chat UI (strict)
	cd $(CHAT_UI) && npm run typecheck && npm run lint

.PHONY: ui-test
ui-test: $(CHAT_UI)/node_modules ## Run the chat UI suite
	cd $(CHAT_UI) && npm test

.PHONY: ui-build
ui-build: $(CHAT_UI)/node_modules ## Build the chat UI
	cd $(CHAT_UI) && npm run build
	# The built page is checked, not trusted. This app is served under /chat/ by
	# the gateway and at the root of a host behind a proxy, and an ABSOLUTE asset
	# path works in exactly one of those: mounted anywhere else every asset falls
	# through the single-page fallback, comes back as index.html, and the browser
	# refuses it for its MIME type. The page is blank and nothing failed.
	#
	# It is checked here rather than trusted to the config because that is how it
	# broke: the config was right, and a build that did not set an environment
	# variable quietly produced the other artifact.
	@grep -q 'src="\./assets/' $(CHAT_UI)/dist/app/index.html || { \
		echo "ui-build: the chat was built for a fixed mount point, not a relative one."; \
		echo "  Its assets would 404 into the console's page wherever it is not served at that path."; \
		grep -o 'src="[^"]*"' $(CHAT_UI)/dist/app/index.html; \
		exit 1; \
	}

.PHONY: ui-dev
ui-dev: $(CHAT_UI)/node_modules ## Run the chat UI dev server
	cd $(CHAT_UI) && npm run dev

.PHONY: e2e
e2e: $(CHAT_UI)/node_modules ## Browser end-to-end gate (scripted server + Playwright). Separate from ci; needs a browser and a local database.
	cd $(CHAT_UI) && bash e2e/run-e2e.sh

# One gate, two harnesses. The SPECS are shared and platform independent: they
# drive a browser at a running gateway. What differs is the script that boots the
# built payload around them, because run.sh is bash and asks the gateway to stop
# with a signal, neither of which exists on Windows.
ifeq ($(OS),Windows_NT)
DESKTOP_E2E := powershell -NoProfile -ExecutionPolicy Bypass -File desktop/personal/windows/e2e.ps1
DESKTOP_BUILD := powershell -NoProfile -ExecutionPolicy Bypass -File desktop/personal/windows/build.ps1
else
DESKTOP_E2E := bash desktop/personal/e2e/run.sh
DESKTOP_BUILD := ./desktop/personal/build.sh
endif

# The two halves of the machine link, against each other.
#
# Every other test of the link has a Go server talking to a Go stand-in, which
# proves one understanding of the protocol is self-consistent. The half that
# ships is Rust on a different websocket library, and what has to agree between
# them is what a shared assumption cannot check: whether an empty binary message
# survives as the end of a direction, whether a refusal parses, whether a
# dial-back finds its ticket.
#
# Outside `make ci` for the reason `make node-ci` is: it needs a Rust toolchain.
.PHONY: link-e2e
link-e2e: ## The machine link end to end: the real Rust client against the real server
	cd desktop && cargo build -p sag-desktop --example link_client
	cd orchestrator && SAG_LINK_E2E=1 go test ./internal/link/ -count=1 -v -timeout 600s

# ---------------------------------------------------------------- the products
#
# ONE name per thing you can want. If two targets would build the same product,
# there is one target. This block used to hold four overlapping ways to build the
# two applications (desktop-build, desktop-install, build-personal, and a copy of
# the enterprise steps written out again inside rebuild-all), which is how an
# afternoon goes into working out which one you were supposed to have run.
#
# What each of them produces, and where it lands, is in DEPLOY.md section 0.

.PHONY: desktop-e2e
desktop-e2e: $(CHAT_UI)/node_modules ## Gate: drive the BUILT personal application, first run to ready
	$(DESKTOP_E2E)

# The personal edition's development loop, and the reason it exists: building and
# installing the application to see a label move is most of ten minutes, nearly
# all of it Rust. This runs the same stack from source with the pages served by
# their own build tool, through the gateway so the origin is the one the
# application has, and a saved file is a reload. `make build-personal` is for
# proving the APPLICATION; this is for building the product inside it.
.PHONY: dev-personal
dev-personal: $(ADMIN_UI)/node_modules $(CHAT_UI)/node_modules ## Run the personal edition from source, pages reloading as you edit them
	./desktop/personal/dev.sh $(DEV_ARGS)

DESKTOP_APP = desktop/.local/out/personal/SAG Personal.app
ENTERPRISE_APP = desktop/.local/out/enterprise/SAG Enterprise.app

# Building what we ship: one target per edition, each from A to Z, each ending by
# PROVING that what landed is what this repository is.
#
# They exist because "build it" meant different sequences for different editions,
# remembered rather than written down, and a day went into an application that
# had been rebuilt but not reinstalled and a window that had been reinstalled but
# not reloaded. A build that cannot say what it produced has not finished.
.PHONY: build-web
build-web: $(CHAT_UI)/node_modules $(ADMIN_UI)/node_modules ## Build the WEB pages and gateway into this working copy
	@echo "==> the chat"
	cd $(CHAT_UI) && npm run build
	@echo "==> the console"
	cd $(ADMIN_UI) && npm run build
	@echo "==> the gateway"
	cd $(ORCHESTRATOR) && go build -o ../desktop/.local/sag-dev ./cmd/sag
	@./scripts/assert-current.sh "the web chat" "$(CHAT_UI)/dist/app"
	@./scripts/assert-current.sh "the web console" "$(ADMIN_UI)/dist"
	@echo "  the pages are served from disk: a browser needs a reload, nothing else"

.PHONY: build-personal
build-personal: $(CHAT_UI)/node_modules $(ADMIN_UI)/node_modules ## Build + gate + install SAG Personal into /Applications (this machine's architecture)
# The node_modules prerequisites are not decoration: build.sh runs `npm run
# build` in both front ends and installs nothing (grep it: there is no npm
# install in that script). Without these, the one command DEPLOY.md gives a
# newcomer for "try the real application" dies partway through a long build on a
# fresh clone, with an npm error rather than one of this system's refusals.
#
# $(DESKTOP_BUILD), never the script by name: on Windows that variable is the
# PowerShell build and naming the shell script here would make this target work
# on one platform and quietly do nothing on the other.
	$(DESKTOP_BUILD) $(BUILD_ARGS)
	$(MAKE) desktop-e2e
# The application this builds, from where this build put it. Both were stale
# once: the name was "SAG Assistant" before the rename and the path was out/
# before the split into out/personal, so this built the new one, gated the new
# one, then installed a day-old application under the old name and reported a
# passing gate. It is checked to exist first, because copying nothing succeeds
# quietly.
	@test -d "$(DESKTOP_APP)" || { echo "build-personal: nothing built at $(DESKTOP_APP)" >&2; exit 1; }
	rm -rf "/Applications/SAG Personal.app"
	cp -R "$(DESKTOP_APP)" /Applications/
	xattr -dr com.apple.quarantine "/Applications/SAG Personal.app"
	@./scripts/assert-current.sh "SAG Personal.app (chat)" "/Applications/SAG Personal.app/Contents/Resources/chat"
	@./scripts/assert-current.sh "SAG Personal.app (console)" "/Applications/SAG Personal.app/Contents/Resources/console"
	@echo "  installed after a passing gate: $(DESKTOP_APP)"

.PHONY: build-enterprise
build-enterprise: ## Build + install SAG Enterprise into /Applications (this machine's architecture)
	./desktop/enterprise/build.sh $(BUILD_ARGS)
# No gate of its own, and the reason is what this edition IS: it carries a window
# and the page that asks for a server, and everything a person sees after that
# comes from the server they named. There is no first run to prove, no database
# to create, and the chat inside it is whichever one that server is serving. What
# `make e2e` proves about the pages proves it here too.
	@test -d "$(ENTERPRISE_APP)" || { echo "build-enterprise: nothing built at $(ENTERPRISE_APP)" >&2; exit 1; }
	rm -rf "/Applications/SAG Enterprise.app"
	cp -R "$(ENTERPRISE_APP)" /Applications/
	xattr -dr com.apple.quarantine "/Applications/SAG Enterprise.app"
	@echo "  installed: $(ENTERPRISE_APP)"
	@echo "  it carries no pages: it serves whatever its server serves"
	@echo "  an application already open keeps the pages it loaded. Cmd-R."

.PHONY: build-all
build-all: build-web build-personal build-enterprise ## Build and install all three products here
	@$(MAKE) versions

.PHONY: rebuild-all
rebuild-all: build-all ## build-all, and restart the running dev gateway on top
	$(MAKE) dev-restart
	@echo "rebuilt: pages, gateway, and both applications in /Applications"

.PHONY: versions
versions: ## What every surface is built from, and whether it is current
	@./scripts/versions.sh

.PHONY: fmt
fmt: ## Format the Go sources
	cd $(ORCHESTRATOR) && gofmt -w .

.PHONY: fmt-check
fmt-check: ## Fail when sources are not formatted
	@cd $(ORCHESTRATOR) && out=$$(gofmt -l .); \
	if [ -n "$$out" ]; then echo "not formatted:"; echo "$$out"; exit 1; fi

# lib/ holds third-party code we keep ourselves (lib/sqlserver/README.md), most
# of it machine-generated. vet reports there on decisions nobody made, and the
# only honest fix for anything it finds is a change to the grammar it came from,
# never an edit to the output.
#
# What is filtered is the OUTPUT and not the input, because leaving the package
# out of the list does nothing: go vet follows imports and prints a dependency's
# diagnostics alongside the importer's. Measured both ways round rather than
# assumed: vetting ONLY internal/sqlguard/sqlserver still prints every lib/ line,
# and vetting internal/store, which does not import it, is clean. Anything vet
# says about code somebody wrote still fails the build.
#
# golangci-lint needs none of this, and is left alone: it honours the
# "Code generated ... DO NOT EDIT." line by itself.
.PHONY: vet
vet: ## Run go vet
	@cd $(ORCHESTRATOR) && out=$$(go vet ./... 2>&1 | grep -v '^lib/' || true); \
	if [ -n "$$out" ]; then echo "$$out"; exit 1; fi

.PHONY: lint
lint: $(LINT_BIN) ## Run the linters
	# GOTOOLCHAIN here as well as on the install below, and for a different
	# reason. Installing it pins what the binary is BUILT with; running it pins
	# which standard library it READS. Without this the linter takes whatever Go
	# is on the machine, so a Homebrew upgrade to a Go newer than go.mod's turns
	# every run into a panic from inside the type checker:
	#
	#   panic: file requires newer Go version go1.27 (application built with go1.26)
	#
	# which names neither the file nor the real problem. Nothing in the
	# repository changed; the machine did.
	cd $(ORCHESTRATOR) && GOTOOLCHAIN=$(GO_TOOLCHAIN) $(LINT_BIN) run ./...

$(LINT_BIN):
	@mkdir -p $(TOOLS_DIR)
	GOTOOLCHAIN=$(GO_TOOLCHAIN) GOBIN=$(TOOLS_DIR) \
		go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_VERSION)
	mv $(TOOLS_DIR)/golangci-lint $@

.PHONY: test-db-up
test-db-up: ## Start the databases the suite runs against (ports 3307, 5433 and 1434)
	cd $(ORCHESTRATOR) && docker compose -f docker-compose.test-db.yml up -d --wait
	@echo 'Point the suite at them:'
	@echo "  export SAG_TEST_DSN='root:sagtest-root@tcp(127.0.0.1:3307)/flexie_sag_test?parseTime=true'"
	@echo "  export SAG_TEST_PG_DSN='postgres://sagtest:sagtest@127.0.0.1:5433/postgres'"
	@echo "  export SAG_TEST_MSSQL_DSN='sqlserver://sa:SagTest!2026pw@127.0.0.1:1434'"
	@echo "  export SAG_TEST_TUNNEL='ssh://jump:jumppass@127.0.0.1:2222/test-db-postgres:5432'"
	@echo "  export SAG_TEST_TUNNEL_DB='postgres://sagtest:sagtest@ignored/postgres'"

.PHONY: test-db-down
test-db-down: ## Stop the test database and throw its data away
	cd $(ORCHESTRATOR) && docker compose -f docker-compose.test-db.yml down -v

.PHONY: test
test: ## Run the full test suite
	cd $(ORCHESTRATOR) && go test ./... -count=1 -timeout 30m

.PHONY: test-race
test-race: ## Run the suite under the race detector
	cd $(ORCHESTRATOR) && go test ./... -count=1 -race -timeout 45m

.PHONY: test-live
test-live: ## Call the real vendor APIs (needs SAG_LIVE_*_KEY; costs money, not run by CI)
	cd $(ORCHESTRATOR) && go test ./internal/provider/ -count=1 -run Live -v

.PHONY: tidy-check
tidy-check: ## Fail when go.mod or go.sum are stale
	@cd $(ORCHESTRATOR) && cp go.mod go.mod.bak && cp go.sum go.sum.bak && \
	go mod tidy && \
	if ! diff -q go.mod go.mod.bak >/dev/null || ! diff -q go.sum go.sum.bak >/dev/null; then \
		mv go.mod.bak go.mod; mv go.sum.bak go.sum; \
		echo "go.mod/go.sum are not tidy; run: cd $(ORCHESTRATOR) && go mod tidy"; exit 1; \
	fi; \
	rm -f go.mod.bak go.sum.bak

.PHONY: migrate
migrate: ## Apply database migrations
	cd $(ORCHESTRATOR) && go run ./cmd/sag migrate up

# Editing the server and seeing the change are two different things, and the
# gap between them is where "I fixed that" comes from. `make dev` closes it: it
# watches the Go sources, rebuilds, and restarts, the same way the two UIs
# already reload themselves.
#
# The restart is the careful part. The server finishes what it is doing on
# SIGTERM (server.go), so the watcher is told to interrupt rather than kill and
# to WAIT for it. Cutting it off mid-request costs somebody their session: the
# browser retries a refresh the old process had already spent, and reuse
# detection revokes the family, exactly as it should (KB/08).
# Rebuild the gateway binary and restart what is serving :8080 with it.
#
# For the loop this repository is actually worked in: a built binary running in
# the background, rather than `make dev` watching files. Doing it by hand is
# four steps with one trap in each, and every one of them has been stepped in:
# the environment has to come from orchestrator/.env (scraping it off the
# running process with `ps eww` produces unquoted values, and a DSN contains
# parentheses, which bash refuses to source); the old process has to be stopped
# BEFORE the binary is replaced, because macOS will not overwrite a running
# executable; the new one has to be started with that environment or every page
# is a 404; and something has to wait for it to answer, or the next command
# talks to a port with nothing behind it.
.PHONY: dev-restart
dev-restart: ## Rebuild the gateway and restart it on :8080, with orchestrator/.env
	@test -f $(ORCHESTRATOR)/.env || { echo "dev-restart: no $(ORCHESTRATOR)/.env (copy .env.example)" >&2; exit 1; }
	cd $(ORCHESTRATOR) && go build -o ../desktop/.local/sag-dev.new ./cmd/sag
	@PID=$$(lsof -nP -iTCP:8080 -sTCP:LISTEN -t | head -1); \
	if [ -n "$$PID" ]; then \
		echo "stopping the gateway on :8080 ($$PID)"; kill -TERM $$PID; \
		for i in $$(seq 1 60); do kill -0 $$PID 2>/dev/null || break; sleep 1; done; \
	fi
	mv desktop/.local/sag-dev.new desktop/.local/sag-dev
	@set -a; . ./$(ORCHESTRATOR)/.env; set +a; \
	nohup ./desktop/.local/sag-dev dev >> desktop/.local/sag-dev.log 2>&1 & \
	until curl -sf -o /dev/null http://localhost:8080/v1/meta; do sleep 1; done; \
	echo "gateway restarted on :8080 (log: desktop/.local/sag-dev.log)"

.PHONY: dev
dev: $(AIR_BIN) ## Run the API server, rebuilding and restarting on every change
	cd $(ORCHESTRATOR) && $(AIR_BIN) -c .air.toml

$(AIR_BIN):
	@mkdir -p $(TOOLS_DIR)
	GOTOOLCHAIN=$(GO_TOOLCHAIN) GOBIN=$(TOOLS_DIR) go install github.com/air-verse/air@$(AIR_VERSION)
	mv $(TOOLS_DIR)/air $@

.PHONY: run
run: ## Run the API server
	cd $(ORCHESTRATOR) && go run ./cmd/sag server

.PHONY: run-worker
run-worker: ## Run the background worker
	cd $(ORCHESTRATOR) && go run ./cmd/sag worker

.PHONY: docker-build
docker-build: ## Build the orchestrator container image
	# The context is the repository root and not $(ORCHESTRATOR): the image
	# carries the console too, so the build needs both front end and server.
	docker build -t flexie-sag-orchestrator -f $(ORCHESTRATOR)/Dockerfile .

.PHONY: clean
clean: ## Remove build artifacts and installed tools
	rm -rf $(ORCHESTRATOR)/bin $(TOOLS_DIR) $(CHAT_UI)/dist

# --- schema ---------------------------------------------------------------------
#
# The schema is DECLARED in orchestrator/schema: one CREATE TABLE per file, and
# the file is the definition. `sag schema` compares a database with it.
#
#   sag schema --dump-sql   print the SQL that would bring this database to it
#   sag schema --update     run it (refuses to destroy data without --allow-destructive)
#   sag schema --pull       rewrite schema/ from the database
#
# This is NOT the migration mechanism. Migrations (`sag migrate`) upgrade a
# database progressively and do the things a schema differ never can: backfill a
# column, delete orphans before a constraint can go on, transform rows. The two
# meet in one assertion, which the test suite makes: what the migrations produce
# is what we declared.

.PHONY: schema-check
schema-check: ## Print what would change to bring the dev database to the declared schema
	cd $(ORCHESTRATOR) && go run ./cmd/sag schema --dump-sql

# --- admin console ---------------------------------------------------------------

$(ADMIN_UI)/node_modules: $(ADMIN_UI)/package.json
	cd $(ADMIN_UI) && npm install
	@touch $@

.PHONY: admin-dev
admin-dev: $(ADMIN_UI)/node_modules ## Run the console against a local orchestrator
	cd $(ADMIN_UI) && npm run dev

.PHONY: admin-lint
admin-lint: $(ADMIN_UI)/node_modules ## Lint and typecheck the console
	cd $(ADMIN_UI) && npm run lint && npm run typecheck

.PHONY: admin-test
admin-test: $(ADMIN_UI)/node_modules ## Run the console suite
	cd $(ADMIN_UI) && npm test

.PHONY: admin-build
admin-build: $(ADMIN_UI)/node_modules ## Build the console
	cd $(ADMIN_UI) && npm run build

# --- inference node --------------------------------------------------------
#
# Its own gate, NOT part of `make ci`, for the same reason `make e2e` is not:
# a cold build of the engine is minutes of compiling a large dependency tree,
# and it would be paid on every unrelated Go or frontend edit by everybody,
# including people without a Rust toolchain. The pipeline runs `node-ci`
# alongside `ci` as a separate step; a change under `inference/` is a change
# whose author runs it deliberately.
#
# `node-check` is the fast loop: it builds the control plane (the catalogue,
# the pulls, the settings form, the disk rules) against the scripted engine,
# in seconds rather than minutes. `node-ci` is the whole thing, engine and all,
# and is what says a change is done.

INFERENCE := inference

.PHONY: node-check
node-check: ## Fast loop: test the node's control plane without building the engine
	cd $(INFERENCE) && cargo test --no-default-features

.PHONY: node-fmt
node-fmt: ## Format the node sources
	cd $(INFERENCE) && cargo fmt

.PHONY: node-lint
node-lint: ## Lint the node, engine included, with warnings fatal
	cd $(INFERENCE) && cargo clippy --features engine --all-targets -- -D warnings

.PHONY: node-test
node-test: ## Run the node suite, engine included
	cd $(INFERENCE) && cargo test --features engine

.PHONY: node-build
node-build: ## Build the node for this machine (add ACCEL=cuda, metal, mkl or accelerate)
	cd $(INFERENCE) && cargo build --release $(if $(ACCEL),--features $(ACCEL),)

.PHONY: node-run
node-run: ## Run the node (needs SAG_NODE_KEY)
	cd $(INFERENCE) && cargo run --features engine -- serve

.PHONY: node-ci
node-ci: ## The node's gate: format, lint, the full suite and the packaging
	cd $(INFERENCE) && cargo fmt --check
	$(MAKE) node-lint node-test node-packaging

.PHONY: node-packaging
node-packaging: ## Check the unit file and the installer that put the node on a machine
	$(INFERENCE)/packaging/packaging_test.sh

.PHONY: node-boot
node-boot: ## Boot the node under a real init and watch it survive (Linux, needs docker)
	$(INFERENCE)/packaging/boot_test.sh $(TARBALL)

# Cutting a release, in ONE command, because it was five environment variables
# and two scripts remembered in the right order. EDITION says which product.
#
#   make release EDITION=personal
#
# It refuses rather than producing something that cannot be shipped: an unsigned
# build gets rejected by the notary, and a build for one architecture strands
# every machine of the other. What it needs is in DEPLOY.md section 4.
.PHONY: release
release: ## Build, sign, notarise and publish a release. EDITION=personal|enterprise
	@test "$(EDITION)" = "personal" -o "$(EDITION)" = "enterprise" || \
		{ echo "release: say which one, EDITION=personal or EDITION=enterprise" >&2; exit 2; }
# ALL of them, named one by one, because the build SKIPS IN SILENCE what it has
# no key for. Checking only the signing identity let a signed but UNNOTARISED
# application be published, which macOS refuses to open and Apple silicon
# refuses to execute: the three API_* variables are what notarising needs, and
# without them the notary step prints nothing at all and the build succeeds.
# (TAURI_SIGNING_PRIVATE_KEY_PASSWORD is deliberately not here: empty is a
# legitimate value for it.)
	@missing=""; \
	for v in APPLE_SIGNING_IDENTITY APPLE_API_KEY APPLE_API_ISSUER APPLE_API_KEY_PATH TAURI_SIGNING_PRIVATE_KEY_PATH; do \
		eval "val=\$$$$v"; [ -n "$$val" ] || missing="$$missing $$v"; \
	done; \
	if [ -n "$$missing" ]; then \
		echo "release: these are not set:$$missing" >&2; \
		echo "  Without them the build skips signing or notarising IN SILENCE" >&2; \
		echo "  and publishes something nobody can run. DEPLOY.md section 4 has" >&2; \
		echo "  the five lines to export." >&2; \
		exit 2; \
	fi
# Checked HERE and not only inside publish.sh, which checks the same thing: there
# it is discovered after a twenty minute build, and the build is the expensive
# half. Same two ways in as publish.sh reads them, and no third invented one.
	@test -n "$$SAG_REPO_SSH" -o -f "$${SAG_RELEASE_ENV:-deploy/release.env}" || \
		{ echo "release: nowhere to publish to." >&2; \
		  echo "  cp deploy/release.env.example deploy/release.env   and fill it in," >&2; \
		  echo "  or set SAG_REPO_SSH for this one command." >&2; exit 2; }
	./desktop/$(EDITION)/build.sh --universal
	./desktop/publish.sh $(EDITION)

# EDITION is required here too, and not defaulted to personal as it once was.
# `make release` refuses without it and this quietly chose one, so the same
# omission behaved two ways: forget the flag in the middle of an enterprise
# release and this published personal, saying so only in a line nobody reads
# twice.
.PHONY: release-publish
release-publish: ## Publish an ALREADY BUILT release. EDITION=personal|enterprise
	@test "$(EDITION)" = "personal" -o "$(EDITION)" = "enterprise" || \
		{ echo "release-publish: say which one, EDITION=personal or EDITION=enterprise" >&2; exit 2; }
	./desktop/publish.sh $(EDITION)
