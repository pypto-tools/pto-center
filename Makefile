GO ?= go

.PHONY: build test demo
build:
	mkdir -p bin
	$(GO) build -o bin/pto-center ./cmd/pto-center
	$(GO) build -o bin/pto-agent ./cmd/pto-agent

test:
	$(GO) test ./...
	python3 -B tests/test_pilot_adapters.py
	node --check internal/center/app.js

demo: build
	bash scripts/demo.sh
