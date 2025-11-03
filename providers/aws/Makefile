# AWS Provider Makefile

# Inherit settings from parent Makefile
TOOLS_DIR := ../../hack/tools
BIN_DIR := bin
TOOLS_BIN_DIR := $(TOOLS_DIR)/$(BIN_DIR)
CONTROLLER_GEN := $(abspath $(TOOLS_BIN_DIR)/controller-gen)

GO_GCFLAGS ?= -gcflags=all='-N -l'
GO := GO111MODULE=on GOWORK=off GOFLAGS=-mod=vendor go
GO_BUILD_RECIPE := CGO_ENABLED=1 $(GO) build $(GO_GCFLAGS)

CRD_OPTIONS ?= "crd"
OUT_DIR ?= ../../bin

.PHONY: api
api: $(CONTROLLER_GEN) ## Generate AWS provider CRDs and deepcopy
	@echo "Generating AWS provider API..."
	GOWORK=off $(CONTROLLER_GEN) object:headerFile="../../hack/boilerplate.go.txt" paths="./api/..."
	GOWORK=off $(CONTROLLER_GEN) $(CRD_OPTIONS) paths="./api/..." output:crd:artifacts:config=manifests/crds
	@echo "AWS provider API generation complete"

.PHONY: install-crds
install-crds: api ## Install AWS provider CRDs
	kubectl apply -f manifests/crds/

.PHONY: build
build: ## Build AWS provider binary
	GOWORK=off $(GO_BUILD_RECIPE) -o $(OUT_DIR)/aws-provider ./cmd

.PHONY: run
run: build ## Run AWS provider controller locally
	@echo "Running AWS provider controller..."
	$(OUT_DIR)/aws-provider

# Ensure controller-gen is available
$(CONTROLLER_GEN):
	@echo "controller-gen not found, building from tools directory..."
	cd $(TOOLS_DIR) && GO111MODULE=on GOFLAGS=-mod=vendor GOWORK=off go build -tags=tools -o $(BIN_DIR)/controller-gen sigs.k8s.io/controller-tools/cmd/controller-gen

.PHONY: help
help: ## Display this help
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)
