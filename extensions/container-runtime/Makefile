.PHONY: test vet build

test:
	GOWORK=off go test ./...

vet:
	GOWORK=off go vet ./...

build:
	GOWORK=off go build ./cmd/...

generate-crds:
	GOWORK=off $(CURDIR)/../../api/bin/controller-gen-v0.18.0 object paths=./api/...
	GOWORK=off $(CURDIR)/../../api/bin/controller-gen-v0.18.0 crd:allowDangerousTypes=true,generateEmbeddedObjectMeta=true paths=./api/... output:crd:artifacts:config=./config/crd/bases
