# PackCove v0.1

PackCove is a single-instance HTTP cache for Maven, npm, and PyPI. Clients use normal package-manager commands against PackCove; it serves complete cached responses or fetches them from the configured upstream. npm tarball URLs and PyPI file links are rewritten to durable, repository-scoped download mappings so those files also pass through the cache.

## Build and Run

Go 1.27 or newer is required. The Go module path is `github.com/Astro24002/PackCove`.

```sh
go test ./...
go run ./cmd/packcove -config packcove.yaml
```

The service listens on `0.0.0.0:8080` by default. `packcove.yaml` uses `./data`; SQLite is stored at `data/index.sqlite`, content-addressed blobs at `data/blobs/`, and incomplete downloads at `data/tmp/`. Keep this directory on persistent storage. Do not delete the database or blobs while PackCove is running.

Build and run the container with a persistent named volume:

```sh
docker build -t packcove:0.1.0 .
docker run --rm -p 8080:8080 -v packcove-data:/var/lib/packcove/data packcove:0.1.0
```

### Docker Compose

The Compose example uses the GHCR image and a named volume for SQLite and cached blobs. If the package is private, log in to GHCR first with an account or token that has package read access.

```sh
docker compose -f docker/compose.yaml up -d
docker compose -f docker/compose.yaml down
```

Edit `docker/packcove.yaml` before starting to set the public URL clients will use. PackCove embeds SQLite, so Compose does not start a separate database service.

### Helm

Install the chart from `helm/packcove`. Set `config.server.public_base_url` and the ingress host in a values file; keep `replicaCount` at `1` and persistent storage enabled.

```sh
helm upgrade --install packcove ./helm/packcove --namespace packcove --create-namespace -f my-values.yaml
```

For a private GHCR package, set `imagePullSecrets` in the values file to a Kubernetes secret with `ghcr.io` read access.

Set `server.public_base_url` to the stable HTTPS URL reachable by package clients before deploying behind an ingress or reverse proxy. Do not rely on the request `Host` header for rewritten download URLs. The sample URL is only for local use. TLS termination is expected at the deployment edge.

## Configuration

`packcove.yaml` has one upstream per named repository. Names must be unique lowercase letters, numbers, or hyphens. Supported `type` values are `maven`, `npm`, and `pypi`. `allowed_download_hosts` must include the upstream host and any file hosts or redirect hosts used by that source. Redirects are checked at every hop. Configuration is validated at startup and changes take effect after restart.

Defaults are five minutes for metadata and mutable Maven SNAPSHOT files, and no expiry for fixed artifacts. Set a finite `immutable_artifact_ttl` for upstreams that overwrite fixed-version artifacts. `stale_if_error` is disabled by default and only applies to expired metadata; enabling it can delay visibility of new or withdrawn versions. PackCove does not automatically remove cached content.

## Client Setup

Use a URL clients can reach and whose TLS certificate they trust:

```sh
npm config set registry https://packcove.example.internal/repository/npm/
npm install

python -m pip config set global.index-url https://packcove.example.internal/repository/pypi/simple/
python -m pip install requests
```

For Maven, mirror only Central in `~/.m2/settings.xml`:

```xml
<mirror>
  <id>packcove-central</id>
  <mirrorOf>central</mirrorOf>
  <url>https://packcove.example.internal/repository/maven-central/</url>
</mirror>
```

Other Maven repositories remain separate. npm lockfiles that name another registry, Git dependencies, direct URLs, and Python direct references may bypass PackCove. PackCove does not publish packages or proxy arbitrary user-supplied URLs.

## Operations

Read-only endpoints are `GET /_packcove/health`, `GET /_packcove/repositories`, `GET /_packcove/stats`, and `GET /metrics`. The first three return JSON; `/metrics` returns Prometheus text. Repository upstream state starts as `unknown` and is updated only by actual requests. Requests and upstream errors are logged as JSON to stdout; query strings and credentials are not logged.

`packcove_artifact_requests_total` counts successful package-file GETs only. Hit ratio is `hit / (hit + miss + revalidated)`; metadata, HEAD, and failed requests are excluded. `packcove_bytes_served_total` counts response-body bytes actually written to clients, and `packcove_cache_bytes` counts unique blobs referenced by the index. In-memory request counters reset on restart. The cache filesystem's available space is reported separately as `packcove_storage_free_bytes`.

The sample single-replica Kubernetes deployment is in `deploy/kubernetes/packcove.yaml`. Configure its host, TLS secret, persistent volume, and public URL for the target cluster. Keep operational endpoints on a trusted network. The data directory supports one active PackCove process; do not scale the Deployment above one replica or attach the same writable directory to another instance.

## Tests and Limits

`go test ./...` runs hermetic config, storage, HTTP, protocol, cache, revalidation, range, concurrency, and failure tests using local upstream fixtures. Real package-manager checks are available with `make test-clients`; npm is run locally, and pip/Maven use Docker images. That target needs Docker and network access to fetch client images and the test Maven Central mirror. It verifies isolated npm and pip clients and two isolated Maven local repositories against a shared PackCove cache.

GitHub Actions checks formatting, runs tests with the race detector, runs `go vet`, validates the Compose configuration, and lints/renders the Helm chart for pushes and pull requests targeting `main`.

After verification succeeds on a push to `main`, CI publishes the image to `ghcr.io/astro24002/packcove` with `latest` and commit-specific `sha-*` tags. The workflow authenticates with the repository `GITHUB_TOKEN`; configure the package visibility in GitHub Packages if it should be publicly pullable.

The v0.1 scope is public read-only repositories, one process, no authentication, no uploads, no web UI, and no automatic cache cleanup. A cold request still depends on its upstream. The cache does not cover compilation time, `node_modules`, virtual environments, OCI images, or downloads that bypass the configured registry. Orphaned blobs from a crash between file publication and index commit are left on disk; automatic reclamation is not implemented.
