APP := CLIProxyAPI
CONFIG := ./config.yaml
CMD := ./cmd/server
DEFAULT_GO := $(HOME)/.local/opt/go1.26.2/bin/go
GO ?= $(if $(wildcard $(DEFAULT_GO)),$(DEFAULT_GO),$(shell command -v go))

.PHONY: build run run-local clean

build:
	env -u GOROOT $(GO) build -o ./$(APP) $(CMD)

run: build
	./$(APP) -config $(CONFIG)

run-local: build
	./$(APP) -config $(CONFIG) -local-model

clean:
	rm -f ./$(APP)
