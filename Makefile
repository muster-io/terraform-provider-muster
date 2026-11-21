# SPDX-License-Identifier: MIT
# Copyright The Muster Authors

.DEFAULT_GOAL := help

GO ?= go
CURL ?= curl
GIT ?= git
MODULE := github.com/muster-io/terraform-provider-muster
BIN := bin
TOOLS := $(BIN)/tools

# Evaluated only when a tool is built, so that targets without Go, such as spec-check, need no Go.
GO_TOOLCHAIN = $(shell $(GO) env GOVERSION)
GOLANGCI_LINT_PKG := github.com/golangci/golangci-lint/v2/cmd/golangci-lint
# renovate: datasource=go depName=github.com/golangci/golangci-lint/v2
GOLANGCI_LINT_VERSION := v2.14.0
GOLANGCI_LINT := $(TOOLS)/golangci-lint-$(GOLANGCI_LINT_VERSION)
GO_LICENSES_PKG := github.com/google/go-licenses/v2
# renovate: datasource=go depName=github.com/google/go-licenses/v2
GO_LICENSES_VERSION := v2.0.1
GO_LICENSES := $(TOOLS)/go-licenses-$(GO_LICENSES_VERSION)
GOVULNCHECK_PKG := golang.org/x/vuln/cmd/govulncheck
# renovate: datasource=go depName=golang.org/x/vuln
GOVULNCHECK_VERSION := v1.8.0
GOVULNCHECK := $(TOOLS)/govulncheck-$(GOVULNCHECK_VERSION)
OAPI_CODEGEN_PKG := github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen
# renovate: datasource=go depName=github.com/oapi-codegen/oapi-codegen/v2
OAPI_CODEGEN_VERSION := v2.8.2
OAPI_CODEGEN := $(TOOLS)/oapi-codegen-$(OAPI_CODEGEN_VERSION)
TFPLUGINDOCS_PKG := github.com/hashicorp/terraform-plugin-docs/cmd/tfplugindocs
# renovate: datasource=go depName=github.com/hashicorp/terraform-plugin-docs
TFPLUGINDOCS_VERSION := v0.25.0
TFPLUGINDOCS := $(TOOLS)/tfplugindocs-$(TFPLUGINDOCS_VERSION)

# The copy of Muster's specification is pinned by exactly one of two files: api/MUSTER_VERSION, a release X.Y.Z whose
# tag is vX.Y.Z, or api/MUSTER_COMMIT, a commit of Muster's master before that release exists.
MUSTER_REPO := https://github.com/muster-io/muster
SPEC_REF = $(shell if [ -f api/MUSTER_COMMIT ]; then cat api/MUSTER_COMMIT; elif [ -f api/MUSTER_VERSION ]; then \
	echo "v$$(cat api/MUSTER_VERSION)"; fi)
MUSTER_SPEC_URL ?= https://raw.githubusercontent.com/muster-io/muster/$(SPEC_REF)/api/openapi.yaml
MUSTER_MASTER_SPEC_URL := https://raw.githubusercontent.com/muster-io/muster/master/api/openapi.yaml

# The licences the provider's binary may depend on (ADR-0001); HashiCorp's plugin libraries are MPL-2.0, unmodified.
ALLOWED_LICENSES := MIT,MIT-0,BSD-2-Clause,BSD-3-Clause,Apache-2.0,ISC,0BSD,Unlicense,CC0-1.0,MPL-2.0
# Modules whose licence go-licenses cannot classify, checked by hand: oapi-codegen/nullable is Apache-2.0, but its
# LICENSE file holds only the short Apache notice. Re-check an entry when its version changes.
LICENSES_CHECKED_BY_HAND := github.com/oapi-codegen/nullable
COVERAGE_MIN := 80
COVER_PROFILE := $(BIN)/cover.out
GENERATED_CLIENT := internal/client/gen/client.gen.go
# The acceptance tests that make testacc runs: those matching ACC_RUN but not ACC_SKIP, regular expressions of go test,
# each left out when empty. By default every TestAcc test except the _oldCLI ones, which only the job old-cli runs, on
# Terraform 1.10 and OpenTofu 1.10: make testacc ACC_RUN='_oldCLI$$' ACC_SKIP= (a $ is written $$ for make).
# TESTARGS is appended to go test, so its -run or -skip replaces these.
ACC_RUN ?= ^TestAcc
ACC_SKIP ?= _oldCLI$$
TESTARGS ?=
ACC_ARGS = $(if $(ACC_RUN),-run '$(ACC_RUN)') $(if $(ACC_SKIP),-skip '$(ACC_SKIP)')

.PHONY: help fmt lint test testacc generate generate-check spec-sync spec-check docs docs-check licenses vulncheck \
	build ci clean

help: ## List the targets
	@awk 'BEGIN { FS = ":.*## " } /^[a-z][a-z-]*:.*## / { printf "  %-16s %s\n", $$1, $$2 }' $(MAKEFILE_LIST)

fmt: $(GOLANGCI_LINT) ## Format the Go code (gofmt and goimports through golangci-lint)
	$(GOLANGCI_LINT) fmt

lint: $(GOLANGCI_LINT) ## Check the licence headers, go.mod and go.sum, and the code with golangci-lint
	@failed=0; \
	sh -c "$$HEADER_CHECK_SH" || failed=1; \
	$(GO) mod tidy -diff || failed=1; \
	$(GOLANGCI_LINT) run ./... || failed=1; \
	exit $$failed

test: ## Run the unit tests with the race detector and the coverage gate of internal/, the generated client excluded
	@mkdir -p $(BIN)
	$(GO) test -race -covermode=atomic -coverprofile=$(COVER_PROFILE) ./...
	@grep -v -e '^$(MODULE)/internal/client/gen/' -e '^$(MODULE)/main\.go:' $(COVER_PROFILE) > $(BIN)/cover.internal.out
	@$(GO) tool cover -func=$(BIN)/cover.internal.out | awk -v min=$(COVERAGE_MIN) '$$1 == "total:" { \
		sub("%", "", $$3); \
		printf "coverage of internal/, internal/client/gen/ excluded: %s%% (minimum %s%%)\n", $$3, min; \
		if ($$3 + 0 < min) exit 1 }'

testacc: ## Run the acceptance tests against muster dev: the one of MUSTER_ENDPOINT, else one test/muster-dev.sh starts
	@if [ -n "$$MUSTER_ENDPOINT" ]; then \
		TF_ACC=1 $(GO) test ./... $(ACC_ARGS) -count=1 -v -timeout 60m $(TESTARGS); \
	else \
		trap 'test/muster-dev.sh down' EXIT; \
		env=$$(test/muster-dev.sh up) || exit 1; \
		eval "$$env"; \
		TF_ACC=1 $(GO) test ./... $(ACC_ARGS) -count=1 -v -timeout 60m $(TESTARGS); \
	fi

generate: $(OAPI_CODEGEN) ## Generate the client in internal/client/gen/ from api/openapi.yaml
	@mkdir -p $(dir $(GENERATED_CLIENT))
	$(OAPI_CODEGEN) -config api/codegen.yaml api/openapi.yaml

generate-check: ## Fail when the generated client is not what make generate makes of api/openapi.yaml
	@tmp=$$(mktemp); trap 'rm -f "$$tmp"' EXIT; \
	cp $(GENERATED_CLIENT) "$$tmp" 2>/dev/null || : > "$$tmp"; \
	$(MAKE) --no-print-directory generate || exit 1; \
	if cmp -s "$$tmp" $(GENERATED_CLIENT); then \
		echo "generated files are current"; \
	else \
		printf 'stale generated files:\n  %s\nrun make generate and commit the result\n' $(GENERATED_CLIENT) >&2; \
		exit 1; \
	fi

spec-sync: ## Pin the copy of Muster's specification: VERSION=x.y.z (the tag), COMMIT=<sha>, or Muster's master
	@set -e; \
	if [ -n "$(VERSION)" ] && [ -n "$(COMMIT)" ]; then echo "set VERSION or COMMIT, not both" >&2; exit 1; fi; \
	if [ -n "$(VERSION)" ]; then \
		ref="v$(VERSION)"; \
	elif [ -n "$(COMMIT)" ]; then \
		ref="$(COMMIT)"; \
	else \
		ref=$$($(GIT) ls-remote $(MUSTER_REPO) refs/heads/master | cut -f1); \
		if [ -z "$$ref" ]; then echo "cannot resolve the master of $(MUSTER_REPO)" >&2; exit 1; fi; \
		echo "Muster's master is $$ref"; \
	fi; \
	if [ -z "$(VERSION)" ] && ! printf '%s' "$$ref" | grep -Eq '^[0-9a-f]{40}$$'; then \
		echo "COMMIT must be a full commit of 40 hex digits: $$ref" >&2; exit 1; \
	fi; \
	tmp=$$(mktemp); trap 'rm -f "$$tmp"' EXIT; \
	url="https://raw.githubusercontent.com/muster-io/muster/$$ref/api/openapi.yaml"; \
	if ! $(CURL) -fsSL --retry 3 "$$url" -o "$$tmp"; then echo "cannot download $$url" >&2; exit 1; fi; \
	mkdir -p api; \
	cp "$$tmp" api/openapi.yaml; \
	if [ -n "$(VERSION)" ]; then \
		printf '%s\n' "$(VERSION)" > api/MUSTER_VERSION; rm -f api/MUSTER_COMMIT; \
	else \
		printf '%s\n' "$$ref" > api/MUSTER_COMMIT; rm -f api/MUSTER_VERSION; \
	fi; \
	echo "api/openapi.yaml is the specification of $$url"
	@$(MAKE) --no-print-directory generate

spec-check: ## Compare api/openapi.yaml byte for byte with the specification of its pin (MUSTER_SPEC_URL)
	@if [ -f api/MUSTER_COMMIT ] && [ -f api/MUSTER_VERSION ]; then \
		echo "api/MUSTER_COMMIT and api/MUSTER_VERSION both exist: keep one (make spec-sync)" >&2; exit 1; \
	fi; \
	if [ ! -f api/MUSTER_COMMIT ] && [ ! -f api/MUSTER_VERSION ]; then \
		echo "neither api/MUSTER_COMMIT nor api/MUSTER_VERSION exists: run make spec-sync" >&2; exit 1; \
	fi; \
	tmp=$$(mktemp); trap 'rm -f "$$tmp"' EXIT; \
	if ! $(CURL) -fsSL --retry 3 "$(MUSTER_SPEC_URL)" -o "$$tmp"; then \
		echo "cannot download $(MUSTER_SPEC_URL)" >&2; \
		exit 1; \
	fi; \
	if cmp -s "$$tmp" api/openapi.yaml; then \
		echo "api/openapi.yaml is the copy of $(MUSTER_SPEC_URL)"; \
		exit 0; \
	fi; \
	diff -u --label "$(MUSTER_SPEC_URL)" --label api/openapi.yaml "$$tmp" api/openapi.yaml; \
	echo "api/openapi.yaml differs from $(MUSTER_SPEC_URL): restore it, or pin another with make spec-sync" >&2; \
	exit 1

docs: $(TFPLUGINDOCS) ## Generate docs/ with tfplugindocs from the schema, templates/ and examples/ (needs terraform)
	$(TFPLUGINDOCS) generate --provider-name muster --rendered-provider-name Muster

docs-check: ## Fail when docs/ is not what make docs makes
	@tmp=$$(mktemp -d); trap 'rm -rf "$$tmp"' EXIT; \
	if [ -d docs ]; then cp -R docs "$$tmp/docs"; else mkdir "$$tmp/docs"; fi; \
	$(MAKE) --no-print-directory docs || exit 1; \
	if diff -r "$$tmp/docs" docs; then \
		echo "docs/ is current"; \
	else \
		echo "docs/ is stale: run make docs and commit the result" >&2; \
		exit 1; \
	fi

licenses: $(GO_LICENSES) ## Fail on a dependency of the provider whose licence is not in ALLOWED_LICENSES
	$(GO_LICENSES) check ./... --ignore $(MODULE) $(addprefix --ignore ,$(LICENSES_CHECKED_BY_HAND)) \
		--allowed_licenses=$(ALLOWED_LICENSES)

vulncheck: $(GOVULNCHECK) ## Find vulnerable Go code that is called (govulncheck)
	$(GOVULNCHECK) ./...

build: ## Build the provider into bin/terraform-provider-muster
	$(GO) build -trimpath -ldflags "-X main.version=dev" -o $(BIN)/terraform-provider-muster .

ci: lint generate-check spec-check docs-check licenses test ## The checks of a pull request, locally (vulncheck, goreleaser and the acceptance tests run in CI jobs of their own)

clean: ## Remove bin/ and dist/: the binary, the coverage profiles, the installed tools and the snapshot builds
	rm -rf $(BIN) dist

# The drift of the copy from Muster's master, for the daily CI job: prints the difference, never fails.
.PHONY: spec-drift
spec-drift:
	@tmp=$$(mktemp); trap 'rm -f "$$tmp"' EXIT; \
	if ! $(CURL) -fsSL --retry 3 "$(MUSTER_MASTER_SPEC_URL)" -o "$$tmp"; then \
		echo "cannot download $(MUSTER_MASTER_SPEC_URL)"; exit 0; \
	fi; \
	if cmp -s "$$tmp" api/openapi.yaml; then \
		echo "api/openapi.yaml is the specification on Muster's master"; \
	else \
		diff -u --label api/openapi.yaml --label "$(MUSTER_MASTER_SPEC_URL)" api/openapi.yaml "$$tmp" || :; \
	fi

$(GOLANGCI_LINT):
	$(call go-install,$(GOLANGCI_LINT_PKG),$(GOLANGCI_LINT_VERSION),$@)

$(GO_LICENSES):
	$(call go-install,$(GO_LICENSES_PKG),$(GO_LICENSES_VERSION),$@)

$(GOVULNCHECK):
	$(call go-install,$(GOVULNCHECK_PKG),$(GOVULNCHECK_VERSION),$@)

$(OAPI_CODEGEN):
	$(call go-install,$(OAPI_CODEGEN_PKG),$(OAPI_CODEGEN_VERSION),$@)

$(TFPLUGINDOCS):
	$(call go-install,$(TFPLUGINDOCS_PKG),$(TFPLUGINDOCS_VERSION),$@)

# go-install builds the tool $(1) at the version $(2) with the project's Go toolchain into the file $(3), trying three
# times, since a module download can fail for a moment.
define go-install
	@mkdir -p $(TOOLS)
	@tmp=$$(mktemp -d); trap 'rm -rf "$$tmp"' EXIT; \
	for n in 1 2 3; do \
		if GOTOOLCHAIN=$(GO_TOOLCHAIN) GOBIN="$$tmp" $(GO) install $(1)@$(2); then break; fi; \
		if [ "$$n" = 3 ]; then echo "cannot install $(1)@$(2)" >&2; exit 1; fi; \
		sleep $$((n * 10)); \
	done; \
	mv "$$tmp"/* $(3)
endef

# The licence header check of lint: every Go file, workflow, YAML file, Renovate configuration, shell script and the
# Makefile start with the MIT header; generated files and the copy api/openapi.yaml are exempt. The Terraform examples
# are not checked: tfplugindocs copies them into the documentation as they are.
define HEADER_CHECK_SH
missing=$$(find . \( -path ./bin -o -path ./dist -o -path ./.git -o -path ./internal/client/gen \) -prune -o -type f \
	\( -name '*.go' -o -name '*.yml' -o -name '*.yaml' -o -name '*.json5' -o -name '*.sh' -o -name Makefile \) \
	-print | sort |
	while IFS= read -r f; do
		[ "$$f" = ./api/openapi.yaml ] && continue
		head -n 5 "$$f" | grep -q 'Code generated .* DO NOT EDIT\.' && continue
		if ! head -n 5 "$$f" | grep -q 'SPDX-License-Identifier: MIT' ||
			! head -n 5 "$$f" | grep -q 'Copyright The Muster Authors'; then
			echo "$$f"
		fi
	done)
if [ -n "$$missing" ]; then
	echo "files without the licence header (SPDX-License-Identifier: MIT, Copyright The Muster Authors):"
	echo "$$missing" | sed 's/^/  /'
	exit 1
fi
endef
export HEADER_CHECK_SH
