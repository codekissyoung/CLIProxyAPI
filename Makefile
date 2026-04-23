APP := CLIProxyAPI
PROFILER_APP := cliproxy-profiler
CONFIG := ./config.yaml
CMD := ./cmd/server
PROFILER_CMD := ./cmd/cliproxy-profiler
GO := $(HOME)/.local/sdk/go1.26.1/bin/go

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
