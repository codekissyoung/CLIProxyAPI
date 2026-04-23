APP := CLIProxyAPI
PROFILER_APP := cliproxy-profiler
CONFIG := ./config.yaml
CMD := ./cmd/server
PROFILER_CMD := ./cmd/cliproxy-profiler
DEFAULT_GO := $(HOME)/.local/opt/go1.26.2/bin/go
GO ?= $(if $(wildcard $(DEFAULT_GO)),$(DEFAULT_GO),$(shell command -v go))

.PHONY: build build-profiler run run-local clean

build:
	env -u GOROOT $(GO) build -o ./$(APP) $(CMD)

build-profiler:
	env -u GOROOT $(GO) build -o ./$(PROFILER_APP) $(PROFILER_CMD)

run: build
	./$(APP) -config $(CONFIG)

run-local: build
	./$(APP) -config $(CONFIG) -local-model

clean:
	rm -f ./$(APP) ./$(PROFILER_APP)
