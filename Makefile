# =============================================================================
# Ingress Control Plane Engine -- Makefile
# =============================================================================
# Usage:
#   make build          Build the Go controller binary
#   make test           Run unit tests
#   make docker-build   Build and tag Docker image
#   make docker-push    Push image to registry
#   make deploy         Full GitOps deploy via ArgoCD
#   make helm-install   Direct Helm install (non-GitOps, for local dev)
#   make apply-crds     Apply CRDs to the current kubectl context
#   make opa-test       Run OPA policy tests
#   make lint           Run golangci-lint
#   make clean          Remove build artifacts

# -----------------------------------------------------------------------------
# Configuration -- override on CLI: make docker-build IMAGE_REPO=123.dkr.ecr...
# -----------------------------------------------------------------------------
BINARY_NAME     := control-plane
CMD_DIR         := ./cmd/controller
BUILD_DIR       := ./bin
IMAGE_REPO      ?= ghcr.io/mphasis/ingress-control-plane-engine
IMAGE_TAG       ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
DOCKER_IMAGE    := $(IMAGE_REPO):$(IMAGE_TAG)
HELM_CHART      := ./deploy/helm
HELM_RELEASE    := icpe
HELM_NAMESPACE  := default
ARGOCD_MANIFEST := ./deploy/argocd/root-app.yaml
CRD_DIR         := ./apps/crds
OPA_POLICY_DIR  := ./pkg/opa

GO              ?= go
GOFLAGS         ?=
GOLANGCI_LINT   ?= golangci-lint
OPA             ?= opa

LDFLAGS := -ldflags "-s -w \
  -X main.version=$(IMAGE_TAG) \
  -X main.commit=$(shell git rev-parse --short HEAD 2>/dev/null || echo unknown) \
  -X main.buildDate=$(shell date -u +%Y-%m-%dT%H:%M:%SZ)"

.PHONY: all build build-local test test-coverage lint fmt vet tidy \
        docker-build docker-push \
        apply-crds helm-install helm-upgrade helm-uninstall helm-template helm-lint \
        deploy status logs-controller logs-envoy logs-opa \
        opa-test opa-check clean help

all: lint test build

# -----------------------------------------------------------------------------
# Go targets
# -----------------------------------------------------------------------------

build:
	@echo "==> Building $(BINARY_NAME)..."
	@mkdir -p $(BUILD_DIR)
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
	  $(GO) build $(GOFLAGS) $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY_NAME) $(CMD_DIR)
	@echo "    Binary: $(BUILD_DIR)/$(BINARY_NAME)"

build-local:
	@mkdir -p $(BUILD_DIR)
	$(GO) build $(GOFLAGS) $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY_NAME)-local $(CMD_DIR)

test:
	@echo "==> Running tests..."
	$(GO) test -v -race -cover ./...

test-coverage:
	@mkdir -p $(BUILD_DIR)
	$(GO) test -coverprofile=$(BUILD_DIR)/coverage.out ./...
	$(GO) tool cover -html=$(BUILD_DIR)/coverage.out -o $(BUILD_DIR)/coverage.html
	@echo "    Coverage report: $(BUILD_DIR)/coverage.html"

lint:
	@echo "==> Running linter..."
	$(GOLANGCI_LINT) run ./...

fmt:
	$(GO) fmt ./...

vet:
	$(GO) vet ./...

tidy:
	$(GO) mod tidy
	$(GO) mod verify

# -----------------------------------------------------------------------------
# OPA targets
# -----------------------------------------------------------------------------

opa-test:
	@echo "==> Running OPA policy tests..."
	$(OPA) test $(OPA_POLICY_DIR) -v

opa-check:
	$(OPA) check $(OPA_POLICY_DIR)/policy.rego

# -----------------------------------------------------------------------------
# Docker targets
# -----------------------------------------------------------------------------

docker-build: build
	@echo "==> Building Docker image $(DOCKER_IMAGE)..."
	docker build \
	  --build-arg BINARY=$(BINARY_NAME) \
	  --build-arg VERSION=$(IMAGE_TAG) \
	  -t $(DOCKER_IMAGE) \
	  -t $(IMAGE_REPO):latest \
	  -f Dockerfile .

docker-push: docker-build
	@echo "==> Pushing $(DOCKER_IMAGE)..."
	docker push $(DOCKER_IMAGE)
	docker push $(IMAGE_REPO):latest

# -----------------------------------------------------------------------------
# Kubernetes targets
# -----------------------------------------------------------------------------

apply-crds:
	@echo "==> Applying CRDs..."
	kubectl apply -f $(CRD_DIR)/
	kubectl wait --for condition=established --timeout=60s \
	  crd/ingressroutes.platform.internal

helm-install: apply-crds
	@echo "==> Installing Helm chart $(HELM_RELEASE)..."
	helm install $(HELM_RELEASE) $(HELM_CHART) \
	  --namespace $(HELM_NAMESPACE) \
	  --create-namespace \
	  --wait \
	  --timeout 5m

helm-upgrade: apply-crds
	@echo "==> Upgrading Helm chart $(HELM_RELEASE)..."
	helm upgrade --install $(HELM_RELEASE) $(HELM_CHART) \
	  --namespace $(HELM_NAMESPACE) \
	  --create-namespace \
	  --wait \
	  --timeout 5m

helm-uninstall:
	helm uninstall $(HELM_RELEASE) --namespace $(HELM_NAMESPACE)

helm-template:
	helm template $(HELM_RELEASE) $(HELM_CHART) --namespace $(HELM_NAMESPACE)

helm-lint:
	helm lint $(HELM_CHART)

deploy:
	@echo "==> Deploying via ArgoCD (App-of-Apps)..."
	kubectl apply -f $(ARGOCD_MANIFEST) -n argocd

status:
	kubectl rollout status deployment/icpe-controller -n $(HELM_NAMESPACE)
	kubectl rollout status deployment/icpe-envoy -n $(HELM_NAMESPACE)

logs-controller:
	kubectl logs -f -l app.kubernetes.io/component=controller \
	  -n $(HELM_NAMESPACE) --all-containers

logs-envoy:
	kubectl logs -f -l app.kubernetes.io/component=envoy \
	  -n $(HELM_NAMESPACE) -c envoy

logs-opa:
	kubectl logs -f -l app.kubernetes.io/component=envoy \
	  -n $(HELM_NAMESPACE) -c opa

# -----------------------------------------------------------------------------
# Utility
# -----------------------------------------------------------------------------

clean:
	@echo "==> Cleaning build artifacts..."
	rm -rf $(BUILD_DIR)

help:
	@echo ""
	@echo "Available targets:"
	@echo "  build            Compile controller binary (linux/amd64)"
	@echo "  build-local      Compile for host OS/arch"
	@echo "  test             Run unit tests with race detector"
	@echo "  test-coverage    Generate HTML coverage report"
	@echo "  lint             Run golangci-lint"
	@echo "  fmt              Format Go source"
	@echo "  vet              Run go vet"
	@echo "  tidy             Tidy and verify go modules"
	@echo "  opa-test         Run OPA Rego unit tests"
	@echo "  opa-check        Validate Rego policy syntax"
	@echo "  docker-build     Build Docker image"
	@echo "  docker-push      Push Docker image to registry"
	@echo "  apply-crds       Apply CRDs to cluster"
	@echo "  helm-install     Install Helm chart (first install)"
	@echo "  helm-upgrade     Upgrade or install Helm chart"
	@echo "  helm-uninstall   Remove Helm release"
	@echo "  helm-template    Dry-run render chart templates"
	@echo "  helm-lint        Lint Helm chart"
	@echo "  deploy           Apply ArgoCD App-of-Apps manifest"
	@echo "  status           Check deployment rollout status"
	@echo "  logs-controller  Tail controller pod logs"
	@echo "  logs-envoy       Tail Envoy proxy logs"
	@echo "  logs-opa         Tail OPA sidecar logs"
	@echo "  clean            Remove build artifacts"
	@echo ""
