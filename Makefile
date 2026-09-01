SHELL := /bin/bash
VERSION ?= dev
IMG ?= ghcr.io/growlyx/presage:$(VERSION)
FORECASTER_IMG ?= ghcr.io/growlyx/presage-forecaster:$(VERSION)
CONTROLLER_GEN ?= $(shell go env GOPATH)/bin/controller-gen

.PHONY: help
help: ## Show this help
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-22s\033[0m %s\n", $$1, $$2}'

.PHONY: generate
generate: ## Regenerate deepcopy, CRDs, and RBAC
	$(CONTROLLER_GEN) object paths=./api/...
	$(CONTROLLER_GEN) crd paths=./api/... output:crd:artifacts:config=config/crd
	$(CONTROLLER_GEN) rbac:roleName=presage-manager paths=./internal/... output:rbac:artifacts:config=config/rbac

.PHONY: build
build: ## Build the controller binary
	go build -o bin/manager ./cmd/manager

.PHONY: test
test: ## Run Go tests with the race detector
	go test ./... -race -cover

.PHONY: lint
lint: ## Vet and check formatting
	go vet ./...
	@test -z "$$(gofmt -l ./api ./cmd ./internal)" || \
		{ echo "gofmt needed:"; gofmt -l ./api ./cmd ./internal; exit 1; }

.PHONY: forecaster-test
forecaster-test: ## Run the forecaster tests (no torch required)
	cd forecaster && uv run --extra dev pytest -q

.PHONY: docker-build
docker-build: ## Build the controller image
	docker build --build-arg VERSION=$(VERSION) -t $(IMG) .

.PHONY: forecaster-docker-build
forecaster-docker-build: ## Build the forecaster image
	docker build -t $(FORECASTER_IMG) ./forecaster

.PHONY: install
install: ## Install CRDs into the current cluster
	kubectl apply -f config/crd

.PHONY: uninstall
uninstall: ## Remove CRDs from the current cluster
	kubectl delete -f config/crd --ignore-not-found

.PHONY: deploy
deploy: install ## Deploy the controller into the current cluster
	kubectl apply -k config

.PHONY: verify
verify: lint test forecaster-test validate-examples chart-lint chart-validate ## Everything CI runs

.PHONY: validate-examples
validate-examples: ## Check the examples against the generated CRD schemas
	uv run --with pyyaml --with jsonschema python hack/validate_examples.py

ENVTEST_K8S_VERSION ?= 1.36.2
SETUP_ENVTEST ?= $(shell go env GOPATH)/bin/setup-envtest

.PHONY: envtest-assets
envtest-assets: ## Download the envtest control-plane binaries
	$(SETUP_ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(PWD)/bin/envtest -p path

.PHONY: test-e2e
test-e2e: ## Run the controller e2e suite against a real API server
	KUBEBUILDER_ASSETS="$$($(SETUP_ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(PWD)/bin/envtest -p path)" \
		go test ./test/e2e/... -v -timeout 10m

.PHONY: test-model-e2e
test-model-e2e: ## Run the forecaster against the real TimesFM checkpoint (downloads ~1GB)
	cd forecaster && PRESAGE_E2E=1 uv run --extra torch --extra dev pytest tests/test_real_model.py -v

.PHONY: backtest
backtest: ## Build the backtest CLI
	go build -o bin/presage-backtest ./cmd/backtest

.PHONY: branding
branding: ## Re-render the banner and social images (needs rsvg-convert)
	node hack/branding/render-social.mjs

.PHONY: api-docs
api-docs: ## Regenerate docs/api-reference.md from the CRDs
	uv run --with pyyaml python hack/gen_api_docs.py
 
# --- Helm ---------------------------------------------------------------
CHART_DIR ?= charts/presage

.PHONY: chart-sync-crds
chart-sync-crds: generate ## Copy generated CRDs into the chart
	cp config/crd/*.yaml $(CHART_DIR)/crds/

.PHONY: chart-lint
chart-lint: ## helm lint
	helm lint $(CHART_DIR)

.PHONY: chart-validate
chart-validate: ## Render the chart across value permutations and assert its invariants
	uv run --with pyyaml python hack/validate_chart.py

.PHONY: chart-package
chart-package: ## Package the chart into dist/
	mkdir -p dist && helm package $(CHART_DIR) -d dist
  
.PHONY: check-model-license
check-model-license: ## Re-check the pinned checkpoint's licence and gating (needs network)
	python3 hack/check_model_license.py

.PHONY: backtest-charts
backtest-charts: ## Regenerate the README backtest charts from the demo signal
	@python3 hack/backtest-demo-signal.py 9099 & echo $$! > /tmp/presage-demo.pid; sleep 2; \
	go run ./cmd/backtest -address http://127.0.0.1:9099 -query 'sum(players)' \
	  -window 672h -resolution 5m -lead-time 10m -interval 15m \
	  -per-replica 80 -min-replicas 4 -max-replicas 60 -static 14 \
	  -chart-dir docs/assets -out docs/backtest-report.md $(BACKTEST_FLAGS); \
	status=$$?; kill $$(cat /tmp/presage-demo.pid) 2>/dev/null; exit $$status

.PHONY: backtest-charts-pdf
backtest-charts-pdf: ## Render the backtest charts to PDF as well as SVG
	@for f in docs/assets/backtest-*.svg; do \
	  rsvg-convert -f pdf "$$f" -o "$${f%.svg}.pdf" && echo "wrote $${f%.svg}.pdf"; \
	done
