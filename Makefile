IMG_REGISTRY ?= quay.io/mkowalski
VERSION ?= 0.1.0-dev

.PHONY: build test lint images manifests

build:
	CGO_ENABLED=0 go build -o bin/agent ./cmd/agent
	CGO_ENABLED=0 go build -o bin/operator ./cmd/operator
	CGO_ENABLED=0 go build -o bin/registrar ./cmd/registrar

test:
	go test ./... -count=1

lint:
	go vet ./...

manifests: controller-gen
	$(CONTROLLER_GEN) crd rbac:roleName=scion-operator paths=./api/... paths=./internal/operator/... output:crd:dir=config/crd

CONTROLLER_GEN = $(shell pwd)/bin/controller-gen
controller-gen:
	test -x $(CONTROLLER_GEN) || GOBIN=$(shell pwd)/bin go install sigs.k8s.io/controller-tools/cmd/controller-gen@latest

images:
	podman build -f build/Dockerfile.agent -t $(IMG_REGISTRY)/scion-node-agent:$(VERSION) .
	podman build -f build/Dockerfile.operator -t $(IMG_REGISTRY)/scion-operator:$(VERSION) .
	podman build -f build/Dockerfile.registrar -t $(IMG_REGISTRY)/scion-registrar:$(VERSION) .
