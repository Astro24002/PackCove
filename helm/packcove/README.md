# PackCove Helm Chart

Install the chart with a values file that sets the client-facing URL and any ingress settings:

```sh
helm upgrade --install packcove ./helm/packcove \
  --namespace packcove --create-namespace \
  -f my-values.yaml
```

The chart uses `ghcr.io/astro24002/packcove:latest`, a single replica, and a 10 GiB persistent volume by default. `replicaCount` must remain `1`; PackCove v0.1 supports one active writer for its SQLite data directory. Set `persistence.existingClaim` to use an existing PVC, or `persistence.enabled: false` only for disposable data.

GHCR packages are private by default. For a private package, create a Kubernetes image pull secret with `ghcr.io` read access and set:

```yaml
imagePullSecrets:
  - name: ghcr-pull-secret
```

The configuration is rendered from `values.yaml` into a ConfigMap. Override `config.server.public_base_url`, repository upstreams, and allowed download hosts in your values file.
