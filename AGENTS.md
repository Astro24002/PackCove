# Repository Guidelines

## Project Structure

PackCove is a Go 1.27 service. The executable starts in `cmd/packcove`; configuration, SQLite-backed blob storage, protocol rewriting, HTTP routes, metrics, and proxying live in `internal/packcove`. Unit and local-upstream integration tests are beside those packages. `packcove.yaml` is the local configuration, `docker/` contains the Compose service and its config, `helm/packcove/` is the Helm chart, and `deploy/kubernetes/packcove.yaml` is the standalone single-replica manifest. The root Dockerfile builds the static image.

## Build, Test, and Run

- `go test ./...` runs hermetic Go tests using local mock upstreams.
- `go run ./cmd/packcove -config packcove.yaml` starts the service.
- `make build` writes `bin/packcove`; `docker build -t packcove:0.1.0 .` builds the container.
- `make test-clients` runs real npm, pip, and Maven client installs. It requires npm, Docker, network access, and may download client images.
- `docker compose -f docker/compose.yaml config` validates the Compose configuration without starting the service.
- `helm lint --strict helm/packcove` checks the chart; `helm template packcove helm/packcove` renders its Kubernetes resources.
- `go vet ./...` checks common Go mistakes.

## Style and Tests

Run `gofmt` on changed Go files. Keep packages lowercase, exported identifiers descriptive, and tests named `TestBehavior`. Put protocol fixtures in Go tests and keep ordinary tests offline and deterministic. Client compatibility tests are explicitly opt-in through `PACKCOVE_RUN_CLIENT_TESTS=1`.

## Changes and Reviews

Use short imperative Conventional Commit subjects such as `feat: add npm tarball mapping`, `fix: reject incomplete upstream blobs`, or `ci: validate Helm and Compose`. Pull requests should describe behavior and configuration impact, list commands run, and call out protocol limits or deployment changes. Include logs or sample requests when changing HTTP behavior.

## Security and Operations

Never log query strings or expose raw upstream credentials. Every repository must restrict upstream and download hosts through configuration. Preserve repository isolation, atomic blob publication, and the single-writer data-directory rule. Do not add publishing, user authentication, UI, or automatic cleanup without changing the v0.1 product scope.
