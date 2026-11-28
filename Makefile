GO ?= go

.PHONY: build test demo
build:
	mkdir -p bin
	$(GO) build -o bin/pto-center ./cmd/pto-center
	$(GO) build $(if $(AGENT_VERSION),-ldflags '-X github.com/pypto-tools/pto-center/internal/agent.BuildVersion=$(AGENT_VERSION)') -o bin/pto-agent ./cmd/pto-agent

test:
	$(GO) test ./...
	python3 -B tests/test_pilot_adapters.py
	node --check internal/center/app.js
	node --check internal/center/topology.js
	node --check internal/center/fleet-views.js
	node --check internal/center/workflows.js

demo: build
	bash scripts/demo.sh
