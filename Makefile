# vdb build gates. The check target grows with the project:
#   S1: lint + build + test + repo-lint   (this file)
#   S4: buf.yaml appears -> generate runs for real (+ drift check joins check)
#   S6: e2e/ appears -> e2e runs for real (and joins check)
GO ?= go

.PHONY: lint build test generate e2e check repo-lint generate-drift

lint:
	@out=$$(gofmt -l . 2>/dev/null); if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi
	$(GO) vet ./...

build:
	$(GO) build ./...

test:
	$(GO) test ./... -race -count=1

generate:
	@if [ -f buf.yaml ]; then PATH="$$HOME/go/bin:$$PATH" buf generate; else echo "generate: no buf.yaml yet (pre-proto stage)"; fi

generate-drift:
	@if [ -f buf.yaml ]; then \
		PATH="$$HOME/go/bin:$$PATH" buf generate && \
		git diff --exit-code -- internal/gen/ proto/ || { echo "codegen drift: regenerate and commit (make generate)"; exit 1; }; \
	else echo "generate-drift: no buf.yaml yet"; fi

e2e:
	@if [ -d e2e ]; then $(GO) test --tags e2e ./e2e/ -race -count=1; else echo "e2e: no e2e/ yet"; fi

repo-lint:
	bash scripts/repo-lint.sh

check: lint build test generate-drift repo-lint e2e
	@echo "check: all gates green"
