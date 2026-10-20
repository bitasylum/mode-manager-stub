# Image to build and push.
IMG ?= mode-manager-stub:latest
# VERSION is stamped into the binary and logged at startup.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

CONTAINER_TOOL ?= docker

# Setting SHELL to bash allows bash commands to be executed by recipes.
SHELL = /usr/bin/env bash -o pipefail
.SHELLFLAGS = -ec

# The client library, github.com/bitasylum/mode-operator/client, comes from
# the version go.mod requires. To build against a local checkout instead,
# make workspace writes a go.work file (ignored by git) that uses it.
LIBRARY_MODULE = github.com/bitasylum/mode-operator/client
LIBRARY_PATH ?= ../mode-operator/client
# The library checkout a go.work file points at, if there is one.
LIBRARY_DIR = $(shell go list -m -f '{{if .Main}}{{.Dir}}{{end}}' $(LIBRARY_MODULE) 2>/dev/null)

.PHONY: all
all: build

##@ General

.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

##@ Development

.PHONY: workspace
workspace: ## Build against the local client library checkout in LIBRARY_PATH.
	@[ -f "$(LIBRARY_PATH)/go.mod" ] || { echo "No client library at $(LIBRARY_PATH); set LIBRARY_PATH"; exit 1; }
	rm -f go.work go.work.sum
	go work init . "$(LIBRARY_PATH)"
	go work edit -go=$$(go mod edit -json | sed -n 's/.*"Go": "\(.*\)".*/\1/p')

.PHONY: fmt
fmt: ## Run go fmt.
	go fmt ./...

.PHONY: vet
vet: ## Run go vet.
	go vet ./...
	go vet -tags e2e ./test/...

.PHONY: test
test: fmt vet ## Run the unit tests.
	go test ./...

# The end-to-end tests run on a kind cluster of their own, with mode-operator
# built and installed from MODE_OPERATOR_DIR: the sibling checkout when there
# is one, or else a clone of the commit go.mod's library version names.
MODE_OPERATOR_DIR ?= $(if $(wildcard ../mode-operator/dist/chart),../mode-operator,bin/mode-operator)
E2E_STUB_IMG ?= mode-manager-stub:e2e
E2E_OPERATOR_IMG ?= mode-operator:e2e
E2E_CLUSTER ?= mode-manager-stub-test-e2e

# The clone is checked out again on every run, so it follows go.mod. Only
# commits on mode-operator's branches and tags can be checked out.
.PHONY: mode-operator-src
mode-operator-src: ## Clone mode-operator at the commit go.mod's library version names, unless a checkout is given.
	@if [ "$(MODE_OPERATOR_DIR)" = bin/mode-operator ]; then \
		ref=$$(hack/mode-operator-ref.sh); \
		if [ ! -d bin/mode-operator/.git ]; then \
			rm -rf bin/mode-operator.tmp; \
			git clone --quiet --no-checkout https://github.com/bitasylum/mode-operator.git bin/mode-operator.tmp; \
			mv bin/mode-operator.tmp bin/mode-operator; \
		fi; \
		git -C bin/mode-operator fetch --quiet --tags origin; \
		commit=$$(git -C bin/mode-operator rev-parse --verify --quiet "$$ref^{commit}") || \
			{ echo "mode-operator has no commit $$ref"; exit 1; }; \
		echo "Using mode-operator at $$commit"; \
		git -C bin/mode-operator checkout --quiet --force --detach "$$commit"; \
	fi

.PHONY: test-e2e
test-e2e: mode-operator-src helm kind ## Run the end-to-end tests on a new kind cluster.
	$(MAKE) docker-build IMG=$(E2E_STUB_IMG)
	$(MAKE) -C "$(MODE_OPERATOR_DIR)" docker-build IMG=$(E2E_OPERATOR_IMG)
	E2E_CLUSTER=$(E2E_CLUSTER) STUB_IMG=$(E2E_STUB_IMG) OPERATOR_IMG=$(E2E_OPERATOR_IMG) \
		MODE_OPERATOR_DIR="$(MODE_OPERATOR_DIR)" HELM="$(HELM)" KIND="$(KIND)" ./hack/test-e2e.sh

.PHONY: lint
lint: golangci-lint ## Run golangci-lint.
	"$(GOLANGCI_LINT_CUSTOM)" run

.PHONY: lint-fix
lint-fix: golangci-lint ## Run golangci-lint and fix what it can.
	"$(GOLANGCI_LINT_CUSTOM)" run --fix

.PHONY: lint-config
lint-config: golangci-lint ## Check the golangci-lint configuration.
	"$(GOLANGCI_LINT_CUSTOM)" config verify

.PHONY: shellcheck
shellcheck: shellcheck-install ## Lint the shell scripts with shellcheck.
	"$(SHELLCHECK)" hack/*.sh

.PHONY: helm-lint
helm-lint: helm ## Lint the chart.
	"$(HELM)" lint chart --strict

# The standard library govulncheck checks is the running Go's, so run it under
# the Go version that the Dockerfile builds the image with.
BUILD_GO_VERSION ?= $(shell hack/build-go-version.sh)
.PHONY: vulncheck
vulncheck: govulncheck ## Check for known vulnerabilities that the code calls, under the image's Go version.
	@[ -n "$(BUILD_GO_VERSION)" ] || { echo "Could not read the Go version from the Dockerfile's golang image"; exit 1; }
	GOTOOLCHAIN=go$(BUILD_GO_VERSION) "$(GOVULNCHECK)" ./...

.PHONY: verify-published
verify-published: ## Build and test without go.work, as CI does, against the library version go.mod requires.
	GOWORK=off go build ./...
	GOWORK=off go vet ./...
	GOWORK=off go vet -tags e2e ./test/...
	GOWORK=off go test ./...

.PHONY: check-no-replace
check-no-replace: ## Fail if go.mod has a replace directive.
	@! grep -q '^replace\|^\s*replace ' go.mod || { echo "go.mod must not have replace directives; use make workspace"; exit 1; }

##@ Build

.PHONY: build
build: fmt vet ## Build the binary.
	go build -ldflags "-X main.version=$(VERSION)" -o bin/mode-manager-stub ./cmd/mode-manager-stub

# With a go.work file pointing at a local library checkout, the image is
# built against that checkout. A label records which library the image has.
LIBRARY_LABEL = org.bitasylum.mode-manager-stub.library
.PHONY: docker-build
docker-build: ## Build the image.
	$(CONTAINER_TOOL) build $(if $(LIBRARY_DIR),--build-context library="$(LIBRARY_DIR)") \
		--label $(LIBRARY_LABEL)=$(if $(LIBRARY_DIR),local,go.mod) --build-arg VERSION=$(VERSION) -t ${IMG} .

# An image built against a local library checkout isn't pushed: nothing but
# the label shows which library it has.
.PHONY: docker-push
docker-push: ## Push the image.
	@library=$$($(CONTAINER_TOOL) inspect -f '{{index .Config.Labels "$(LIBRARY_LABEL)"}}' ${IMG}); \
	[ "$$library" = go.mod ] || { echo "${IMG} wasn't built against the library go.mod requires; remove go.work and rebuild"; exit 1; }
	$(CONTAINER_TOOL) push ${IMG}

##@ Dependencies

LOCALBIN ?= $(shell pwd)/bin
$(LOCALBIN):
	mkdir -p "$(LOCALBIN)"

GOLANGCI_LINT = $(LOCALBIN)/golangci-lint
# golangci-lint built with the plugins in .custom-gcl.yml, which also names
# the golangci-lint version. A change to that file builds a new one.
GOLANGCI_LINT_CUSTOM = $(LOCALBIN)/golangci-lint-custom-$(shell sha256sum .custom-gcl.yml | cut -c1-12)
GOVULNCHECK ?= $(LOCALBIN)/govulncheck
HELM ?= $(LOCALBIN)/helm
KIND ?= $(LOCALBIN)/kind
# shellcheck isn't a Go tool, so shellcheck-install downloads a pinned release.
# Set SHELLCHECK to use another copy.
SHELLCHECK ?= $(LOCALBIN)/shellcheck

# The version .custom-gcl.yml builds with the plugins.
GOLANGCI_LINT_VERSION ?= $(shell sed -n 's/^version: //p' .custom-gcl.yml)
GOVULNCHECK_VERSION ?= v1.9.0
HELM_VERSION ?= v4.3.1
KIND_VERSION ?= v0.33.0
# The sha256 of each release tarball, as GitHub reports it for the asset.
SHELLCHECK_VERSION ?= v0.11.0
SHELLCHECK_SHA256_x86_64 ?= b7af85e41cc99489dcc21d66c6d5f3685138f06d34651e6d34b42ec6d54fe6f6
SHELLCHECK_SHA256_aarch64 ?= 68a8133197a50beb8803f8d42f9908d1af1c5540d4bb05fdfca8c1fa47decefc

.PHONY: govulncheck
govulncheck: $(GOVULNCHECK) ## Download govulncheck locally if necessary.
$(GOVULNCHECK): $(LOCALBIN)
	$(call go-install-tool,$(GOVULNCHECK),golang.org/x/vuln/cmd/govulncheck,$(GOVULNCHECK_VERSION))

.PHONY: helm
helm: $(HELM) ## Download helm locally if necessary.
$(HELM): $(LOCALBIN)
	$(call go-install-tool,$(HELM),helm.sh/helm/v4/cmd/helm,$(HELM_VERSION))

.PHONY: kind
kind: $(KIND) ## Download kind locally if necessary.
$(KIND): $(LOCALBIN)
	$(call go-install-tool,$(KIND),sigs.k8s.io/kind,$(KIND_VERSION))

.PHONY: shellcheck-install
shellcheck-install: $(LOCALBIN) ## Download shellcheck locally if necessary, checking its sha256.
	@target="$(LOCALBIN)/shellcheck"; versioned="$$target-$(SHELLCHECK_VERSION)"; \
	[ "$(SHELLCHECK)" = "$$target" ] || exit 0; \
	[ -f "$$versioned" ] && [ "$$(readlink -- "$$target" 2>/dev/null)" = "$$versioned" ] && exit 0; \
	set -e; \
	case "$$(uname -sm)" in \
	  "Linux x86_64") arch=x86_64; sum=$(SHELLCHECK_SHA256_x86_64) ;; \
	  "Linux aarch64" | "Linux arm64") arch=aarch64; sum=$(SHELLCHECK_SHA256_aarch64) ;; \
	  *) echo "No pinned shellcheck for $$(uname -sm); install one and set SHELLCHECK"; exit 1 ;; \
	esac; \
	tarball="shellcheck-$(SHELLCHECK_VERSION).linux.$$arch.tar.gz"; \
	tmp="$$(mktemp -d)"; \
	echo "Downloading $$tarball"; \
	curl -fsSL -o "$$tmp/$$tarball" "https://github.com/koalaman/shellcheck/releases/download/$(SHELLCHECK_VERSION)/$$tarball"; \
	echo "$$sum  $$tmp/$$tarball" | sha256sum -c --quiet -; \
	tar -xzf "$$tmp/$$tarball" -C "$$tmp"; \
	mv "$$tmp/shellcheck-$(SHELLCHECK_VERSION)/shellcheck" "$$versioned"; \
	rm -rf "$$tmp"; \
	ln -sf "$$versioned" "$$target"

.PHONY: golangci-lint
golangci-lint: $(GOLANGCI_LINT_CUSTOM) ## Download golangci-lint and build it with its plugins, if necessary.
$(GOLANGCI_LINT_CUSTOM): | $(LOCALBIN)
	$(call go-install-tool,$(GOLANGCI_LINT),github.com/golangci/golangci-lint/v2/cmd/golangci-lint,$(GOLANGCI_LINT_VERSION))
	@echo "Building golangci-lint with the plugins in .custom-gcl.yml"
	rm -f "$(LOCALBIN)"/golangci-lint-custom-*
	"$(GOLANGCI_LINT)" custom --destination "$(LOCALBIN)" --name "$(notdir $@)"

# go-install-tool installs a Go tool at a pinned version.
# $1 - target path with name of binary
# $2 - package url which can be installed
# $3 - specific version of package
define go-install-tool
@[ -f "$(1)-$(3)" ] && [ "$$(readlink -- "$(1)" 2>/dev/null)" = "$(1)-$(3)" ] || { \
set -e; \
package=$(2)@$(3) ;\
echo "Downloading $${package}" ;\
rm -f "$(1)" ;\
GOBIN="$(LOCALBIN)" GOWORK=off go install $${package} ;\
mv "$(LOCALBIN)/$$(basename "$(1)")" "$(1)-$(3)" ;\
} ;\
ln -sf "$$(realpath "$(1)-$(3)")" "$(1)"
endef
