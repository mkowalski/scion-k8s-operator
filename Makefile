IMG_REGISTRY ?= quay.io/mkowalski
VERSION ?= 0.1.0-dev

BUNDLE_VERSION ?= 0.1.0

.PHONY: build test lint images manifests controller-gen envtest operator-sdk kustomize bundle bundle-check

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

OPERATOR_SDK_VERSION ?= v1.42.3
OPERATOR_SDK = $(CURDIR)/bin/operator-sdk
operator-sdk:
	test -x $(OPERATOR_SDK) && $(OPERATOR_SDK) version | grep -q '"$(OPERATOR_SDK_VERSION)"' || \
		{ curl -sL -o $(OPERATOR_SDK) https://github.com/operator-framework/operator-sdk/releases/download/$(OPERATOR_SDK_VERSION)/operator-sdk_$(shell go env GOOS)_$(shell go env GOARCH) && chmod +x $(OPERATOR_SDK); }

# OLM bundle. config/olm holds the bundle-eligible subset (CRD, RBAC,
# Deployment, sample CR); namespace and monitoring objects ship via the
# plain kustomize path (config/manifests) because OLM bundles reject them.
bundle: manifests operator-sdk kustomize
	$(KUSTOMIZE) build --load-restrictor LoadRestrictionsNone config/olm | \
		$(OPERATOR_SDK) generate bundle --version $(BUNDLE_VERSION) --kustomize-dir config/manifests --package scion-operator
	$(OPERATOR_SDK) bundle validate ./bundle

KUSTOMIZE_VERSION ?= v5.8.1
KUSTOMIZE = $(CURDIR)/bin/kustomize-$(KUSTOMIZE_VERSION)
kustomize:
	test -x $(KUSTOMIZE) || { GOBIN=$(CURDIR)/bin go install sigs.k8s.io/kustomize/kustomize/v5@$(KUSTOMIZE_VERSION) && mv $(CURDIR)/bin/kustomize $(KUSTOMIZE); }

# Fail if the committed bundle/ is out of date with its inputs, ignoring
# the volatile createdAt timestamp operator-sdk stamps into the CSV.
bundle-check: bundle
	git diff -I '^    createdAt:' --exit-code bundle/ || \
		{ echo "bundle/ is out of date; run 'make bundle' and commit the result" >&2; exit 1; }
