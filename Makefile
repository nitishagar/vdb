# vdb build gates (make check): gofmt+vet, build, race tests, codegen drift
# (buf generate -> git diff on internal/gen/ and proto/), repo-lint (also
# fails naming any missing gate input: buf.yaml, buf.gen.yaml, the proto,
# e2e tests), and the e2e suite. CI (.github/workflows/ci.yml) runs make
# check on every push; buf and the protoc plugins are installed there.
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
	@PATH="$$HOME/go/bin:$$PATH" buf generate

# Two steps, separately reported: a buf failure (missing buf, bad buf.yaml)
# is not the same failure as codegen drift.
generate-drift:
	@PATH="$$HOME/go/bin:$$PATH" buf generate
	@git diff --exit-code -- internal/gen/ proto/ || { echo "codegen drift: regenerate and commit (make generate)"; exit 1; }

e2e:
	@$(GO) test --tags e2e ./e2e/ -race -count=1

repo-lint:
	bash scripts/repo-lint.sh

check: lint build test generate-drift repo-lint e2e
	@echo "check: all gates green"
