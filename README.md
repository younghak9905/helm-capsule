# helm-capsule

`helm-capsule` is a proof-first Helm installation capsule compiler written in Go.

It is not a generic image mirror script, Secret manager, or Helm values rewriter.
Its contract is narrower:

> For a specific chart, values, release, namespace, Kubernetes profile, and
> platform, prove that Helm's final manifest changes only supported `image`
> fields and that those images point to digest-pinned internal registry
> references.

The CLI reports only:

- `PROVEN`: every target image is digest-pinned and only image fields changed.
- `UNPROVEN`: artifacts were generated, but proof is incomplete.
- `FAILED`: rendering, parsing, lock matching, or proof validation failed.

## Ubuntu setup

Install Go from the official tarball and persist the environment variables:

```bash
GO_VERSION=1.26.5
curl -fsSLO "https://go.dev/dl/go${GO_VERSION}.linux-amd64.tar.gz"
sudo rm -rf /usr/local/go
sudo tar -C /usr/local -xzf "go${GO_VERSION}.linux-amd64.tar.gz"

cat <<'EOF' >> ~/.profile
export GOROOT=/usr/local/go
export GOPATH=$HOME/go
export PATH=$GOROOT/bin:$GOPATH/bin:$PATH
EOF

. ~/.profile
go version
```

Install runtime tools used by Helm rendering, mirroring, and air-gap archives:

```bash
sudo apt-get update
sudo apt-get install -y curl git skopeo tar zstd

curl -fsSL https://raw.githubusercontent.com/helm/helm/main/scripts/get-helm-3 | bash
helm version
```

Build the CLI on Ubuntu:

```bash
go test ./...
go build -o helm-capsule ./cmd/helm-capsule
sudo install -m 0755 helm-capsule /usr/local/bin/helm-capsule
helm-capsule
```

## Windows build

```powershell
$env:PATH = "C:\Users\user\.local\go\bin;$env:PATH"
go test ./...
go build -o helm-capsule.exe ./cmd/helm-capsule
```

## Basic flow

```bash
helm-capsule build bitnami/redis \
  --release redis \
  --namespace apps \
  -f values.yaml \
  --target-registry registry.internal/platform \
  --platform linux/amd64 \
  --kube-version 1.34.3 \
  --out capsule/

helm-capsule mirror capsule/images.lock.yaml

helm-capsule verify capsule/

helm upgrade --install redis bitnami/redis \
  -n apps \
  -f values.yaml \
  --post-renderer ./capsule/post-renderer
```

If Helm is not installed locally, pass an already rendered manifest:

```bash
helm-capsule build \
  --release redis \
  --namespace apps \
  --rendered-manifest rendered.yaml \
  --target-registry registry.internal/platform \
  --out capsule/
```

## Air-gap flow

Create an OCI image layout first. This requires `skopeo`.

```bash
helm-capsule mirror capsule/images.lock.yaml \
  --oci-layout capsule/oci-layout

helm-capsule export capsule/ \
  --output redis.capsule.tar.zst
```

Inside the disconnected environment:

```bash
helm-capsule import redis.capsule.tar.zst \
  --target-registry registry.internal/platform \
  --out imported-capsule/

helm-capsule verify imported-capsule/
```

Without an `oci-layout` directory, `export` fails unless `--metadata-only` is
explicitly set.

## Supported proof surface

The MVP proves image fields in PodSpec-bearing resources:

- `Pod`
- `Deployment`
- `ReplicaSet`
- `ReplicationController`
- `StatefulSet`
- `DaemonSet`
- `Job`
- `CronJob`

It checks:

- `containers[].image`
- `initContainers[].image`
- `ephemeralContainers[].image`

Unsupported image-like fields are reported as `UNPROVEN`; CRD internals and
custom resources are not guessed.

## Istio `image: auto`

Some Istio charts render gateway proxy containers with `image: auto`. This is a
chart/runtime placeholder, not a concrete OCI image reference. `helm-capsule`
does not rewrite it to `docker.io/library/auto:latest`; it reports
`UNPROVEN` with `unresolved_image_placeholder` instead.

For Istio Gateway, resolve the actual proxy image through the Istio chart values
or installation profile first, then rebuild the capsule.

## Secret stance

`helm-capsule` does not own namespace-scoped pull credentials. Operators should
choose one pull access profile outside the proof path:

- anonymous internal registry read
- namespace-scoped imagePullSecret
- default ServiceAccount patch
- External Secrets
- kubelet credential provider

The capsule proves image relocation. It does not claim to manage Secret
lifecycle.
