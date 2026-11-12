# Common development tasks. Run `make help` for the list.

GO ?= go
PLATFORMS := linux/amd64 linux/arm64 darwin/arm64 darwin/amd64 windows/amd64
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

.PHONY: help demo demo-change demo-stop build test test-integration ui ui-dev release clean

help: ## Show this list
	@grep -E '^[a-z-]+:.*## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*## "}; {printf "  %-18s %s\n", $$1, $$2}'

demo: ## Try docsync: SQL Server in Docker with example data, then Studio in your browser
	@./scripts/demo.sh start

demo-change: ## Change some demo data, to watch documents update
	@./scripts/demo.sh change

demo-stop: ## Stop the demo's SQL Server container
	@./scripts/demo.sh stop

build: ## Build bin/docsync (the UI build in internal/studio/ui/dist is embedded)
	$(GO) build -ldflags "-X main.version=$(VERSION)" -o bin/docsync ./cmd/docsync

test: ## Vet and run the unit tests
	$(GO) vet ./...
	$(GO) test ./...

test-integration: ## Run the integration tests (needs DOCSYNC_TEST_DSN pointing at SQL Server's master database)
	@test -n "$$DOCSYNC_TEST_DSN" || (echo "set DOCSYNC_TEST_DSN, for example sqlserver://sa:...@127.0.0.1:1433?database=master&encrypt=disable" && exit 1)
	$(GO) test -tags integration ./test/integration/ -v -timeout 30m

ui: ## Rebuild the Studio UI into internal/studio/ui/dist (commit the result)
	cd web/studio && npm ci && npm run build

ui-dev: ## Studio UI with live reload; first run: docsync studio --token dev --no-browser
	cd web/studio && DOCSYNC_STUDIO_TOKEN=dev npm run dev

release: ## Cross-compile release binaries and checksums into dist/ (VERSION=v0.1.0)
	@mkdir -p dist
	@for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; ext=""; [ "$$os" = windows ] && ext=".exe"; \
		echo "  dist/docsync_$${os}_$${arch}$$ext"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch $(GO) build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o dist/docsync_$${os}_$${arch}$$ext ./cmd/docsync || exit 1; \
	done
	@cd dist && (command -v sha256sum >/dev/null && sha256sum docsync_* || shasum -a 256 docsync_*) > SHA256SUMS
	@echo "  dist/SHA256SUMS ($(VERSION))"

clean: ## Remove build output
	rm -rf bin dist
