.PHONY: test vet build generate-crds

BIN_DIR ?= $(CURDIR)/bin

test:
	GOWORK=off go test ./...

vet:
	GOWORK=off go vet ./...

build:
	mkdir -p $(BIN_DIR)
	GOWORK=off go build -o $(BIN_DIR)/container-runtime-controller ./cmd/container-runtime-controller
	GOWORK=off go build -o $(BIN_DIR)/raw-supervisor ./cmd/raw-supervisor

generate-crds:
	GOWORK=off $(CURDIR)/../../api/bin/controller-gen-v0.18.0 object paths=./api/...
	GOWORK=off $(CURDIR)/../../api/bin/controller-gen-v0.18.0 crd:allowDangerousTypes=true,generateEmbeddedObjectMeta=true paths=./api/... output:crd:artifacts:config=./config/crd/bases
