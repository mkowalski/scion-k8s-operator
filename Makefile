IMG_REGISTRY ?= quay.io/mkowalski
VERSION ?= 0.1.0-dev

.PHONY: build test lint images manifests controller-gen envtest

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

CONTROLLER_TOOLS_VERSION ?= v0.21.0
CONTROLLER_GEN = $(CURDIR)/bin/controller-gen-$(CONTROLLER_TOOLS_VERSION)
controller-gen:
	test -x $(CONTROLLER_GEN) || { GOBIN=$(CURDIR)/bin go install sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_TOOLS_VERSION) && mv $(CURDIR)/bin/controller-gen $(CONTROLLER_GEN); }

# envtest installs setup-envtest; run `bin/setup-envtest use -p path <version>`
# and export KUBEBUILDER_ASSETS to run the controller envtest suite.
ENVTEST = $(CURDIR)/bin/setup-envtest
envtest:
	test -x $(ENVTEST) || GOBIN=$(CURDIR)/bin go install sigs.k8s.io/controller-runtime/tools/setup-envtest@latest

images:
	podman build -f build/Dockerfile.agent -t $(IMG_REGISTRY)/scion-node-agent:$(VERSION) .
	podman build -f build/Dockerfile.operator -t $(IMG_REGISTRY)/scion-operator:$(VERSION) .
	podman build -f build/Dockerfile.registrar -t $(IMG_REGISTRY)/scion-registrar:$(VERSION) .
