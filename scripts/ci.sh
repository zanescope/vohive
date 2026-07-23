#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

export GOWORK="${GOWORK:-off}"

find_go() {
	if [[ -n "${GO_BIN:-}" ]]; then
		printf '%s\n' "$GO_BIN"
		return
	fi
	if command -v go >/dev/null 2>&1; then
		command -v go
		return
	fi
	if [[ -x /usr/local/go/bin/go ]]; then
		printf '%s\n' /usr/local/go/bin/go
		return
	fi
	printf 'go not found; set GO_BIN=/path/to/go\n' >&2
	return 127
}

run() {
	printf '\n==> %s\n' "$*"
	"$@"
}

workflow_lint() {
	local actionlint_bin tmpbin

	if [[ -n "${ACTIONLINT_BIN:-}" ]]; then
		actionlint_bin="$ACTIONLINT_BIN"
	elif command -v actionlint >/dev/null 2>&1; then
		actionlint_bin="$(command -v actionlint)"
	else
		tmpbin="$(mktemp -d)"
		ACTIONLINT_VERSION="${ACTIONLINT_VERSION:-v1.7.12}"
		run env GOBIN="$tmpbin" "$GO_BIN" install "github.com/rhysd/actionlint/cmd/actionlint@${ACTIONLINT_VERSION}"
		actionlint_bin="$tmpbin/actionlint"
	fi

	run "$actionlint_bin" .github/workflows/*.yml
}

dependency_hygiene() {
	local forbidden_refs forbidden_modules main_module module resolved

	if [[ -f go.work ]]; then
		printf 'go.work must not be committed or required for CI builds\n' >&2
		return 1
	fi
	main_module="$(env GOWORK=off "$GO_BIN" list -m -f '{{.Path}}')"
	if [[ "$main_module" != "github.com/zanescope/vohive" ]]; then
		printf 'module path mismatch: got %s, want github.com/zanescope/vohive\n' "$main_module" >&2
		return 1
	fi

	forbidden_refs="$(
		{
			git grep -nE 'github[.]com/(iniwex5|boa-z)|iniwex[/]vohive|DOCKERHUB[_]|secrets[.]DOCKERHUB|vohive[-]release|GO[.]?PRIVATE|GO[.]?NOSUMDB|GH[_]PAT' -- \
				go.mod go.sum .github Dockerfile Dockerfile.github Dockerfile.runtime docker-compose.yml CONTAINER.md Makefile scripts internal cmd pkg web/src \
				':!internal/web/dist/**' ':!web/dist/**' || true
			git grep -nE 'replace[[:space:]].*=>[[:space:]]*(\.{1,2}/|/|~)' -- \
				go.mod go.sum .github Dockerfile Dockerfile.github Dockerfile.runtime docker-compose.yml CONTAINER.md Makefile scripts internal cmd pkg web/src \
				':!internal/web/dist/**' ':!web/dist/**' || true
		} | sed '/^$/d'
	)"
	if [[ -n "$forbidden_refs" ]]; then
		printf 'forbidden dependency or local-path references found:\n%s\n' "$forbidden_refs" >&2
		return 1
	fi

	forbidden_modules="$(env GOWORK=off "$GO_BIN" list -m all | grep -E 'github[.]com/(iniwex5|boa-z)' || true)"
	if [[ -n "$forbidden_modules" ]]; then
		printf 'forbidden modules resolved by go list -m all:\n%s\n' "$forbidden_modules" >&2
		return 1
	fi

	if grep -Eq '^[[:space:]]*replace([[:space:]]|$)' go.mod; then
		printf 'go.mod must consume zanescope modules directly without replace directives\n' >&2
		return 1
	fi

	for module in \
		github.com/zanescope/quectel-qmi-go \
		github.com/zanescope/vowifi-go; do
		resolved="$(env GOWORK=off "$GO_BIN" list -m -f '{{.Path}}@{{.Version}}{{with .Replace}} replace={{.Path}}@{{.Version}}{{end}}' "$module")"
		if [[ "$resolved" != "${module}@v"* || "$resolved" == *" replace="* ]]; then
			printf 'module %s must resolve directly at a version; got %s\n' \
				"$module" "${resolved:-<none>}" >&2
			return 1
		fi
		printf '\n==> verified direct dependency: %s\n' "$resolved"
	done

	printf '\n==> dependency hygiene ok\n'
}

release_hygiene() {
	local file needle workflow policy manifest_tool
	workflow=".github/workflows/binary-release.yml"
	policy="packaging/release-policy.json"
	manifest_tool="scripts/release-manifest/main.go"
	for file in "$workflow" "$policy" "$manifest_tool"; do
		if [[ ! -f "$file" ]]; then
			printf 'release file not found: %s\n' "$file" >&2
			return 1
		fi
	done
	if git grep -nE 'repository:[[:space:]]*[^[:space:]]+|GH[_]PAT|vohive[-]release|github[.]com/iniwex5' -- "$workflow"; then
		printf 'release workflow must not contain cross-repository publishing wiring\n' >&2
		return 1
	fi
	for needle in \
		'EXPECTED_REPOSITORY: zanescope/vohive' \
		'name: Validate release gates' \
		'needs: validate' \
		'go run ./scripts/release-manifest generate' \
		'go run ./scripts/release-manifest verify' \
		'go run ./scripts/release-manifest alias-gate' \
		'needs.bundle.outputs.advance_alias' \
		'needs.bundle.outputs.source_revision' \
		'packaging/install.sh' \
		'packaging/uninstall.sh' \
		'./cmd/vohivectl' \
		'./cmd/vohive-verify' \
		'VOHIVE_MINISIGN_PUBLIC_KEYS' \
		'@VOHIVE_VERIFY_SHA256@' \
		'@VOHIVE_BOOTSTRAP_VERSION@' \
		'sha256sum vohive-* vohive_* release-manifest.json' \
		'MINISIGN_PASSWORD}" | minisign -Sm' \
		'minisign -Vm' \
		'name: Refuse an existing GitHub Release' \
		'releases?per_page=100' \
		'release-manifest.json.minisig' \
		'SHA256SUMS.minisig' \
		"if: github.ref_type == 'tag'" \
		'environment: release' \
		'trap cleanup_key EXIT' \
		'gh release create' \
		'repos/${EXPECTED_REPOSITORY}/commits/${RELEASE_VERSION}' \
		'bundle=${SOURCE_REVISION}, remote=${remote_revision}' \
		'--verify-tag' \
		'--json tagName,isDraft,isImmutable' \
		'gh release delete' \
		'for placeholder in "${keys_placeholder}"'; do
		if ! grep -Fq -- "$needle" "$workflow"; then
			printf 'release workflow is missing required constraint: %s\n' "$needle" >&2
			return 1
		fi
	done
	if grep -Fq 'overwrite_files: true' "$workflow"; then
		printf 'release workflow must not overwrite published assets\n' >&2
		return 1
	fi
	if grep -Fq 'softprops/action-gh-release' "$workflow"; then
		printf 'release workflow must use the GitHub CLI draft-upload-publish path\n' >&2
		return 1
	fi
	if ! grep -Fq '"repository": "zanescope/vohive"' "$policy" || \
		! grep -Fq '"container_image": "ghcr.io/zanescope/vohive"' "$policy"; then
		printf 'release policy must pin the repository and container publishing identities\n' >&2
		return 1
	fi
	printf '\n==> release hygiene ok\n'
}

container_hygiene() {
	local file needle publish build compose env_example
	publish=".github/workflows/docker-publish.yml"
	build=".github/workflows/docker-build.yml"
	compose="docker-compose.yml"
	env_example=".env.example"

	for file in "$publish" "$build" "$compose" "$env_example" CONTAINER.md Dockerfile Dockerfile.github Dockerfile.runtime; do
		if [[ ! -f "$file" ]]; then
			printf 'container build file not found: %s\n' "$file" >&2
			return 1
		fi
	done
	if [[ "$(find . -maxdepth 1 -type f -name 'docker-compose*.yml' | wc -l | tr -d ' ')" -ne 1 ]]; then
		printf 'exactly one official Compose file is allowed\n' >&2
		return 1
	fi
	if grep -En 'DOCKERHUB|dockerhub|secrets[.]DOCKERHUB|iniwex[/]vohive|ghcr[.]io/[$][{][{][[:space:]]*github[.]repository' "$publish" "$build" "$compose"; then
		printf 'container build configuration must not reference DockerHub or legacy images\n' >&2
		return 1
	fi
	if grep -En 'alpine:latest|-alpine([[:space:]]|$)' Dockerfile Dockerfile.github Dockerfile.runtime; then
		printf 'Docker base images must pin an explicit Alpine minor\n' >&2
		return 1
	fi
	for file in Dockerfile Dockerfile.github Dockerfile.runtime; do
		for needle in 'ARG ALPINE_VERSION=3.23' 'org.opencontainers.image.source' 'org.opencontainers.image.revision' 'org.opencontainers.image.version' '/healthz'; do
			if ! grep -Fq -- "$needle" "$file"; then
				printf '%s is missing required image constraint: %s\n' "$file" "$needle" >&2
				return 1
			fi
		done
	done
	if grep -En 'go mod tidy([[:space:]]|&|$)' Dockerfile Dockerfile.github; then
		printf 'Docker builds must not run go mod tidy; CI tidy gates should catch dependency drift before image build\n' >&2
		return 1
	fi
	if [[ "$(grep -hF 'registry: ghcr.io' "$publish" "$build" | wc -l | tr -d ' ')" -lt 2 ]]; then
		printf 'docker workflows must authenticate against ghcr.io when pushing\n' >&2
		return 1
	fi
	for needle in \
		'CONTAINER_IMAGE: ghcr.io/zanescope/vohive' \
		'go run ./scripts/release-manifest alias-gate' \
		'needs.validate.outputs.advance_alias' \
		'platforms: linux/amd64,linux/arm64' \
		'org.opencontainers.image.source' \
		'org.opencontainers.image.revision' \
		'org.opencontainers.image.version' \
		'VOHIVE_MINISIGN_PUBLIC_KEYS' \
		'release-manifest.json.minisig' \
		'go run ./cmd/vohive-verify' \
		'go run ./scripts/release-manifest verify' \
		"jq -r '.immutable'" \
		'grep -Fxq "${exact_tag}"' \
		'environment: release' \
		'needs: validate'; do
		if ! grep -Fq -- "$needle" "$publish"; then
			printf 'docker publish workflow is missing constraint: %s\n' "$needle" >&2
			return 1
		fi
	done
	for needle in '${CONTAINER_IMAGE}:latest' '${CONTAINER_IMAGE}:beta' '${CONTAINER_IMAGE}:edge-${revision:0:12}'; do
		if ! grep -Fq -- "$needle" "$publish"; then
			printf 'docker publish workflow is missing channel tag: %s\n' "$needle" >&2
			return 1
		fi
	done
	if grep -Fq ':latest' "$build"; then
		printf 'manual container workflow must never publish latest\n' >&2
		return 1
	fi
	for needle in 'CONTAINER_IMAGE: ghcr.io/zanescope/vohive' ':edge-${revision:0:12}' 'platforms: linux/amd64,linux/arm64'; do
		if ! grep -Fq -- "$needle" "$build"; then
			printf 'manual container workflow is missing constraint: %s\n' "$needle" >&2
			return 1
		fi
	done
	for needle in 'VOHIVE_IMAGE:?' 'ghcr.io/zanescope/vohive@sha256:<digest>' 'network_mode: host' '/healthz'; do
		if ! grep -Fq -- "$needle" "$compose"; then
			printf 'Compose file is missing constraint: %s\n' "$needle" >&2
			return 1
		fi
	done
	if grep -Eq '^[[:space:]]*ports:|HTTPS?_PROXY=http' "$compose"; then
		printf 'host-network Compose must not publish ports or hard-code a proxy\n' >&2
		return 1
	fi
	if ! grep -Fxq 'VOHIVE_IMAGE=' "$env_example"; then
		printf '.env.example must leave VOHIVE_IMAGE empty so Compose emits the required-digest guidance\n' >&2
		return 1
	fi
	if grep -Eq '^VOHIVE_IMAGE=.*(latest|REPLACE)' "$env_example"; then
		printf '.env.example must not provide a mutable tag or fake digest placeholder\n' >&2
		return 1
	fi
	printf '\n==> container hygiene ok\n'
}

web_build() {
	run npm ci --prefix web
	run npm run build --prefix web
	rm -rf internal/web/dist
	mkdir -p internal/web
	cp -R web/dist internal/web/dist
}

tidy_check() {
	run "$GO_BIN" mod tidy -diff
}

go_tests() {
	read -r -a packages <<< "${CI_GO_TEST_PACKAGES:-./internal/device ./internal/mbim ./internal/qmi ./internal/netprobe ./pkg/mbim ./internal/backend ./internal/esim ./internal/cscall ./internal/proxy/traffic ./internal/notify ./internal/qqbot/... ./internal/api ./internal/config ./internal/db ./internal/updater/... ./cmd/vohive-verify ./cmd/vohivectl ./scripts/release-manifest}"
	if [[ ${#packages[@]} -eq 0 ]]; then
		printf '\n==> no Go test packages configured\n'
		return
	fi
	run "$GO_BIN" test "${packages[@]}"
}

go_build() {
	(
		export CGO_ENABLED="${CGO_ENABLED:-0}"
		export GOOS="${GOOS:-linux}"
		run "$GO_BIN" build -trimpath -buildvcs=false -tags "${GO_TAGS:-with_utls nomsgpack}" -o "${CI_BUILD_OUTPUT:-/tmp/vohive}" ./cmd/vohive
	)
}

usage() {
	cat <<'USAGE'
Usage: scripts/ci.sh [all|workflow-lint|hygiene|release-hygiene|container-hygiene|web|tidy|test|build ...]

Default all runs workflow-lint, hygiene, release-hygiene, container-hygiene, web, tidy, test, and build.

Environment:
  GO_BIN               path to go binary
  GOWORK               Go workspace mode, default: off
  ACTIONLINT_BIN       path to an existing actionlint binary
  ACTIONLINT_VERSION   actionlint version to install when needed
  CI_GO_TEST_PACKAGES  package list for Go tests
  CI_BUILD_OUTPUT      output path for the CI build binary
  GO_TAGS              build tags, default: with_utls nomsgpack
USAGE
}

GO_BIN="$(find_go)"

if [[ $# -eq 0 || "${1:-}" == "all" ]]; then
	tasks=(workflow-lint hygiene release-hygiene container-hygiene web tidy test build)
else
	tasks=("$@")
fi

printf 'Using Go: %s\n' "$("$GO_BIN" version)"
printf 'Using GOWORK: %s\n' "$GOWORK"

for task in "${tasks[@]}"; do
	case "$task" in
		workflow-lint | actionlint) workflow_lint ;;
		hygiene | dependency-hygiene) dependency_hygiene ;;
		release-hygiene | release) release_hygiene ;;
		container-hygiene | container | docker-hygiene) container_hygiene ;;
		web | frontend) web_build ;;
		tidy | tidy-check) tidy_check ;;
		test | go-test) go_tests ;;
		build | go-build) go_build ;;
		-h | --help | help)
			usage
			exit 0
			;;
		*)
			printf 'unknown CI task: %s\n' "$task" >&2
			usage >&2
			exit 2
			;;
	esac
done
