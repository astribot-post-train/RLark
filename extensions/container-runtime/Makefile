.PHONY: test vet build generate-crds test-ui build-ui

BIN_DIR ?= $(CURDIR)/bin

test:
	GOWORK=off go test ./...

vet:
	GOWORK=off go vet ./...

build:
	mkdir -p $(BIN_DIR)
	GOWORK=off go build -o $(BIN_DIR)/container-runtime-controller ./cmd/container-runtime-controller
	GOWORK=off go build -o $(BIN_DIR)/container-runtime-api ./cmd/container-runtime-api
	GOWORK=off go build -o $(BIN_DIR)/container-runtime-console ./cmd/container-runtime-console
	GOWORK=off go build -o $(BIN_DIR)/raw-supervisor ./cmd/raw-supervisor
	GOWORK=off go build -o $(BIN_DIR)/container-runtime-agent ./cmd/container-runtime-agent
	GOWORK=off go build -o $(BIN_DIR)/container-runtime-admin ./cmd/container-runtime-admin

generate-crds:
	GOWORK=off $(CURDIR)/../../api/bin/controller-gen-v0.18.0 object paths=./api/...
	GOWORK=off $(CURDIR)/../../api/bin/controller-gen-v0.18.0 crd:allowDangerousTypes=true,generateEmbeddedObjectMeta=true paths=./api/... output:crd:artifacts:config=./config/crd/bases

test-ui:
	cd ui && npm ci && npm test

build-ui:
	cd ui && npm ci && npm run build
