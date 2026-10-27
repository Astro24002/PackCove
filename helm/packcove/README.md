# PackCove Helm Chart

This chart uses Bitnami NGINX-style value groups for image, security, service, ingress, persistence, and pod scheduling. It renders a Deployment, Service, ConfigMap, and persistent volume claim.

## Install

```sh
helm upgrade --install packcove ./helm/packcove --namespace packcove --create-namespace --set config.server.public_base_url=https://packcove.example.internal --set ingress.enabled=true --set ingress.hostname=packcove.example.internal --set ingress.tls=true --set ingress.existingSecret=packcove-tls
```

The image defaults to `ghcr.io/astro24002/packcove:<appVersion>`; CI publishes that version tag, `latest`, and a commit-specific `sha-*` tag. GHCR packages are private by default. Create a Kubernetes registry secret and add its name under `image.pullSecrets` or `global.imagePullSecrets`:

```yaml
image:
  pullSecrets:
    - ghcr-pull-secret
```

## Configuration

Set `config.server.public_base_url` to the client-visible URL and configure `config.repositories` for the upstreams in use. `ingress.hostname` and `config.server.public_base_url` should match. TLS uses an existing Secret; when `ingress.tls` is true and `ingress.existingSecret` is empty, the chart expects `<release-fullname>-tls`.

`persistence.mountPath` controls both the volume mount and generated `config.storage.data_dir`. Set `persistence.existingClaim` to reuse a PVC, or disable persistence only for disposable data. `global.storageClass` overrides `persistence.storageClass`.

PackCove v0.1 uses a single writer for SQLite. `replicaCount` must be `1` and `updateStrategy.type` must be `Recreate`; the values schema rejects other settings. Standard placement, security context, probe, label, annotation, extra volume, and sidecar values are available.

## Values Migration

Values are reorganized from chart version `0.1.0`:

- Split the old full `image.repository` into `image.registry` and `image.repository`. Leave `image.tag` empty to use the chart appVersion, or set an explicit tag.
- Convert old `imagePullSecrets: [{name: secret}]` to `image.pullSecrets: [secret]` (or use `global.imagePullSecrets`).
- Move `service.port` to `service.ports.http` and `persistence.storageClassName` to `persistence.storageClass`.
- Move `ingress.hosts[0].host` to `ingress.hostname`; use `ingress.ingressClassName` and set `ingress.tls: true` with `ingress.existingSecret`.
- Add `enabled: true` to existing pod and container security contexts. Move nested `probes.*` settings to `startupProbe`, `readinessProbe`, or `livenessProbe`.
