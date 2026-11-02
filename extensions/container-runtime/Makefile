.PHONY: test vet build generate-crds test-ui build-ui

BIN_DIR ?= $(CURDIR)/bin

test:
	cd core && GOWORK=off go test ./...
	GOWORK=off go test ./...

vet:
	cd core && GOWORK=off go vet ./...
	GOWORK=off go vet ./...

build:
	mkdir -p $(BIN_DIR)
	GOWORK=off go build -o $(BIN_DIR)/container-runtime-controller ./cmd/container-runtime-controller
	GOWORK=off go build -o $(BIN_DIR)/container-runtime-api ./cmd/container-runtime-api
	GOWORK=off go build -o $(BIN_DIR)/raw-supervisor ./cmd/raw-supervisor
	GOWORK=off go build -o $(BIN_DIR)/container-runtime-agent ./cmd/container-runtime-agent
	GOWORK=off go build -o $(BIN_DIR)/container-runtime-admin ./cmd/container-runtime-admin
	GOWORK=off go build -o $(BIN_DIR)/container-runtime-gateway ./cmd/container-runtime-gateway

generate-crds:
	cd core && GOWORK=off $(CURDIR)/../../api/bin/controller-gen-v0.18.0 object paths=./api/...
	cd core && GOWORK=off $(CURDIR)/../../api/bin/controller-gen-v0.18.0 crd:allowDangerousTypes=true,generateEmbeddedObjectMeta=true paths=./api/... output:crd:artifacts:config=$(CURDIR)/config/crd/bases

test-ui:
	npm --prefix ../../apps/rlark-ui test

build-ui:
	npm --prefix ../../apps/rlark-ui run build
