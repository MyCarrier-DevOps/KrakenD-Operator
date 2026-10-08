# VERSION is the version the image reports (docker-build).
VERSION ?= 0.0.1

# Image URL to use all building/pushing image targets
IMG ?= ghcr.io/mycarrier-devops/krakend-operator:latest

# Get the currently used golang install path (in GOPATH/bin, unless GOBIN is set)
ifeq (,$(shell go env GOBIN))
GOBIN=$(shell go env GOPATH)/bin
else
GOBIN=$(shell go env GOBIN)
endif

# CONTAINER_TOOL defines the container tool to be used for building images.
# Be aware that the target commands are only tested with Docker which is
# scaffolded by default. However, you might want to replace it to use other
# tools. (i.e. podman)
CONTAINER_TOOL ?= docker

# Setting SHELL to bash allows bash commands to be executed by recipes.
# Options are set to exit when a recipe line exits non-zero or a piped command fails.
SHELL = /usr/bin/env bash -o pipefail
.SHELLFLAGS = -ec

.PHONY: all
all: build

##@ General

# The help target prints out all targets with their descriptions organized
# beneath their categories. The categories are represented by '##@' and the
# target descriptions by '##'. The awk command is responsible for reading the
# entire set of makefiles included in this invocation, looking for lines of the
# file as xyz: ## something, and then pretty-format the target and help. Then,
# if there's a line with ##@ something, that gets pretty-printed as a category.
# More info on the usage of ANSI control characters for terminal formatting:
# https://en.wikipedia.org/wiki/ANSI_escape_code#SGR_parameters
# More info on the awk command:
# http://linuxcommand.org/lc3_adv_awk.php

.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

##@ Development

.PHONY: manifests
manifests: controller-gen ## Generate WebhookConfiguration, ClusterRole and CustomResourceDefinition objects.
	$(CONTROLLER_GEN) rbac:roleName=manager-role crd webhook paths="./..." output:crd:artifacts:config=config/crd/bases
	# Keep the Helm chart's CRD copy in sync with the generated source (review
	# id 3812030497, round-5 #1): release.yml's own "Copy CRDs into chart"
	# step runs AFTER the release tag is already cut and never commits the
	# result back, so the committed charts/ copy — which both documented
	# CRD-upgrade paths (raw.githubusercontent.com at a tag, and a charts/
	# checkout) actually read — would otherwise only be refreshed by hand.
	# Doing the copy here, gated by verify-manifests below, is what makes a
	# future `make manifests` run that changes a CRD fail the gate instead of
	# silently leaving the chart copy stale.
	# ../charts/krakend-operator/crds/ is therefore fully generated output —
	# see the README.md in that directory (review id 3812913995, round-6
	# #2). The *.yaml glob below only ever touches CRD files, so it leaves
	# that README.md alone; hand edits to the CRD *.yaml files themselves
	# are still discarded on the next `make manifests` run.
	cp config/crd/bases/*.yaml ../charts/krakend-operator/crds/
	# The chart's manager ClusterRole renders its rules from this copy
	# (templates/clusterrole.yaml), so it cannot drift from the markers;
	# verify-manifests below gates the copy like the CRDs.
	mkdir -p ../charts/krakend-operator/files
	cp config/rbac/role.yaml ../charts/krakend-operator/files/manager-role.yaml

.PHONY: generate
generate: controller-gen ## Generate code containing DeepCopy, DeepCopyInto, and DeepCopyObject method implementations.
	$(CONTROLLER_GEN) object:headerFile="hack/boilerplate.go.txt" paths="./..."

.PHONY: verify-manifests
# Fail if any generated manifest output drifts from source. Covers every
# generated-output location, not just config/crd/bases:
#   - config/crd/bases, config/rbac/role.yaml, config/webhook/manifests.yaml
#     (controller-gen output, via the `manifests` target)
#   - ../charts/krakend-operator/crds and ../charts/krakend-operator/files,
#     the chart's copies of the CRDs and of the manager ClusterRole, which the
#     `manifests` target copies from the controller-gen output. The chart's
#     templates/clusterrole.yaml renders the role copy, so the chart cannot
#     drift from the +kubebuilder:rbac markers.
# It also catches NEW untracked files under those paths as a whole, not just
# modifications to already-tracked files: `git diff --exit-code` alone is blind
# to a newly-generated file that was never committed.
#
# The untracked-file check trips on ANY local, non-gitignored scratch file
# dropped under one of the gated paths, not only on generated drift. That is
# intentional: narrowing it to controller-gen output would reopen the gap this
# gate exists to close. CI is never affected, since CI checkouts carry no local
# scratch files. If this trips locally,
# `git clean -n -- $(VERIFY_MANIFESTS_PATHS)` shows what would be removed.
VERIFY_MANIFESTS_PATHS = config/crd/bases config/rbac/role.yaml config/webhook/manifests.yaml ../charts/krakend-operator/crds ../charts/krakend-operator/files
verify-manifests: manifests generate ## Verify generated manifests (CRDs/RBAC/webhook/chart copies) match committed source. May fail on local untracked scratch files under gated paths (not a bug, CI-safe; see the Makefile comment above this target).
	@untracked=$$(git status --porcelain -- $(VERIFY_MANIFESTS_PATHS) | grep '^??' || true); \
	if [ -n "$$untracked" ]; then \
	  echo "generated manifests drifted: new untracked file(s):"; \
	  echo "$$untracked"; \
	  echo "run 'make manifests' and commit the result"; \
	  exit 1; \
	fi
	git diff --exit-code -- $(VERIFY_MANIFESTS_PATHS) || \
	  (echo "generated manifests are out of date — run 'make manifests' and commit the result" \
	    && exit 1)

.PHONY: fmt
fmt: ## Run go fmt against code.
	go fmt ./...

.PHONY: vet
vet: ## Run go vet against code.
	go vet ./...

.PHONY: test
test: manifests generate fmt vet ## Run unit tests.
	go test ./... -coverprofile cover.out

# Integration tests use testcontainers-go with an ephemeral K3s cluster
# and run controllers in-process. Requires a container runtime (docker/podman).
.PHONY: test-integration
test-integration: manifests generate fmt vet ## Run integration tests in an ephemeral K3s cluster via testcontainers.
	go test -tags=integration -v -timeout 15m ./test/integration/

# The admission audit's fixtures run offline; they need bash, jq and diff.
.PHONY: test-audit
test-audit: ## Run the admission audit against its fixtures.
	bash hack/test-audit-admission-rules.sh

# E2E tests use testcontainers-go with an ephemeral K3s cluster.
# No external cluster setup is required.
.PHONY: test-e2e
test-e2e: manifests generate fmt vet ## Run the e2e tests in an ephemeral K3s cluster via testcontainers.
	go test -tags=e2e -v -timeout 30m ./test/e2e/ -ginkgo.v

.PHONY: lint
lint: golangci-lint ## Run golangci-lint linter
	$(GOLANGCI_LINT) run --config ../.github/.golangci.yml

.PHONY: lint-fix
lint-fix: golangci-lint ## Run golangci-lint linter and perform fixes
	$(GOLANGCI_LINT) run --fix --config ../.github/.golangci.yml

.PHONY: lint-config
lint-config: golangci-lint ## Verify golangci-lint linter configuration
	$(GOLANGCI_LINT) config verify --config ../.github/.golangci.yml

.PHONY: third-party-notices
third-party-notices: go-licenses ## Regenerate THIRD-PARTY-NOTICES from the manager binary's dependency graph.
	GO_LICENSES=$(GO_LICENSES) hack/gen-third-party-notices.sh

##@ Build

.PHONY: build
build: manifests generate fmt vet ## Build manager binary.
	go build -o bin/manager ./cmd

.PHONY: run
run: manifests generate fmt vet ## Run a controller from your host.
	go run ./cmd

# If you wish to build the manager image targeting other platforms you can use the --platform flag.
# (i.e. docker build --platform linux/arm64). However, you must enable docker buildKit for it.
# More info: https://docs.docker.com/develop/develop-images/build_enhancements/
.PHONY: docker-build
docker-build: ## Build docker image with the manager.
	$(CONTAINER_TOOL) build --build-arg VERSION=$(VERSION) -t ${IMG} .

.PHONY: docker-push
docker-push: ## Push docker image with the manager.
	$(CONTAINER_TOOL) push ${IMG}

# PLATFORMS defines the target platforms for the manager image be built to provide support to multiple
# architectures. (i.e. make docker-buildx IMG=myregistry/mypoperator:0.0.1). To use this option you need to:
# - be able to use docker buildx. More info: https://docs.docker.com/build/buildx/
# - have enabled BuildKit. More info: https://docs.docker.com/develop/develop-images/build_enhancements/
# - be able to push the image to your registry (i.e. if you do not set a valid value via IMG=<myregistry/image:<tag>> then the export will fail)
# To adequately provide solutions that are compatible with multiple platforms, you should consider using this option.
PLATFORMS ?= linux/arm64,linux/amd64,linux/s390x,linux/ppc64le
.PHONY: docker-buildx
docker-buildx: ## Build and push docker image for the manager for cross-platform support
	# copy existing Dockerfile and insert --platform=${BUILDPLATFORM} into Dockerfile.cross, and preserve the original Dockerfile
	sed -e '1 s/\(^FROM\)/FROM --platform=\$$\{BUILDPLATFORM\}/; t' -e ' 1,// s//FROM --platform=\$$\{BUILDPLATFORM\}/' Dockerfile > Dockerfile.cross
	- $(CONTAINER_TOOL) buildx create --name krakend-operator-builder
	$(CONTAINER_TOOL) buildx use krakend-operator-builder
	- $(CONTAINER_TOOL) buildx build --push --platform=$(PLATFORMS) --build-arg VERSION=$(VERSION) --tag ${IMG} -f Dockerfile.cross .
	- $(CONTAINER_TOOL) buildx rm krakend-operator-builder
	rm Dockerfile.cross

##@ Deployment

ifndef ignore-not-found
  ignore-not-found = false
endif

.PHONY: install
install: manifests ## Install the CRDs into the cluster of the current kubeconfig.
	$(KUBECTL) apply --server-side -f config/crd/bases

.PHONY: uninstall
uninstall: manifests ## Uninstall the CRDs (and every KrakenD object). Call with ignore-not-found=true to ignore resource not found errors during deletion.
	$(KUBECTL) delete --ignore-not-found=$(ignore-not-found) -f config/crd/bases

##@ Dependencies

## Location to install dependencies to
LOCALBIN ?= $(shell pwd)/bin
$(LOCALBIN):
	mkdir -p $(LOCALBIN)

## Tool Binaries
KUBECTL ?= kubectl
CONTROLLER_GEN ?= $(LOCALBIN)/controller-gen
GOLANGCI_LINT = $(LOCALBIN)/golangci-lint
GO_LICENSES ?= $(LOCALBIN)/go-licenses

## Tool Versions
CONTROLLER_TOOLS_VERSION ?= v0.18.0
GOLANGCI_LINT_VERSION ?= v2.11.4
GO_LICENSES_VERSION ?= v1.6.0

.PHONY: controller-gen
controller-gen: $(CONTROLLER_GEN) ## Download controller-gen locally if necessary.
$(CONTROLLER_GEN): $(LOCALBIN)
	$(call go-install-tool,$(CONTROLLER_GEN),sigs.k8s.io/controller-tools/cmd/controller-gen,$(CONTROLLER_TOOLS_VERSION))

.PHONY: golangci-lint
golangci-lint: $(GOLANGCI_LINT) ## Download golangci-lint locally if necessary.
$(GOLANGCI_LINT): $(LOCALBIN)
	$(call go-install-tool,$(GOLANGCI_LINT),github.com/golangci/golangci-lint/v2/cmd/golangci-lint,$(GOLANGCI_LINT_VERSION))

.PHONY: go-licenses
go-licenses: $(GO_LICENSES) ## Download go-licenses locally if necessary.
$(GO_LICENSES): $(LOCALBIN)
	$(call go-install-tool,$(GO_LICENSES),github.com/google/go-licenses,$(GO_LICENSES_VERSION))

# go-install-tool will 'go install' any package with custom target and name of binary, if it doesn't exist
# $1 - target path with name of binary
# $2 - package url which can be installed
# $3 - specific version of package
define go-install-tool
@[ -f "$(1)-$(3)" ] || { \
set -e; \
package=$(2)@$(3) ;\
echo "Downloading $${package}" ;\
rm -f $(1) || true ;\
GOBIN=$(LOCALBIN) go install $${package} ;\
mv $(1) $(1)-$(3) ;\
} ;\
ln -sf $(1)-$(3) $(1)
endef
