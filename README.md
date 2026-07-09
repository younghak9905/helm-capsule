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

## Korean quick start

`helm-capsule`은 폐쇄망 또는 프라이빗 클러스터에서 Helm chart 설치 전에
필요한 이미지를 내부 registry로 옮기고, 최종 manifest에서 `image` 필드만
내부 registry digest로 바뀌었음을 증명하는 도구입니다.

설치 흐름은 다음 순서를 권장합니다.

```text
plan -> build -> mirror -> verify -> helm upgrade --install
```

### 1. Ubuntu 설치

Go가 없다면 먼저 설치합니다.

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

필수 도구를 설치합니다.

```bash
sudo apt-get update
sudo apt-get install -y curl git skopeo tar zstd bash-completion

curl -fsSL https://raw.githubusercontent.com/helm/helm/main/scripts/get-helm-3 | bash
helm version
```

빌드하고 설치합니다.

```bash
git clone https://github.com/younghak9905/helm-capsule.git
cd helm-capsule

go test ./...
go build -o helm-capsule ./cmd/helm-capsule
sudo install -m 0755 helm-capsule /usr/local/bin/helm-capsule
```

### 2. 도움말과 자동완성

전체 도움말:

```bash
helm-capsule help
```

명령별 도움말:

```bash
helm-capsule help plan
helm-capsule help build
helm-capsule build --help
```

Ubuntu bash 자동완성:

```bash
helm-capsule completion bash | sudo tee /etc/bash_completion.d/helm-capsule >/dev/null
source /etc/bash_completion.d/helm-capsule
```

이후 `helm-capsule ` 뒤에서 `Tab`을 누르면 사용 가능한 명령이 표시됩니다.

### 3. OpenSearch 예시

Helm repo를 추가합니다.

```bash
helm repo add opensearch https://opensearch-project.github.io/helm-charts/
helm repo update
```

설치 전 입력값을 먼저 확인합니다.

```bash
helm-capsule plan opensearch/opensearch \
  --release opensearch \
  --namespace opensearch \
  -f opensearch-values.yaml \
  --pull-secret registry-cloud-kt \
  --kube-version 1.34.3 \
  --out plan-opensearch
```

`NEEDS_INPUT`이 나오면 `plan-opensearch/plan.yaml`을 보고 StorageClass,
pull secret, Secret 참조, Service port 같은 값을 먼저 values나 namespace
bootstrap에 반영합니다.

이미지 capsule을 생성합니다.

```bash
helm-capsule build opensearch/opensearch \
  --release opensearch \
  --namespace opensearch \
  -f opensearch-values.yaml \
  --target-registry registry.cloud.kt.com/2e2koiqr \
  --platform linux/amd64 \
  --kube-version 1.34.3 \
  --out capsule-opensearch
```

이미지를 내부 registry로 복사합니다.

```bash
cd capsule-opensearch
skopeo login registry.cloud.kt.com
helm-capsule mirror images.lock.yaml --tool skopeo
```

증명 결과를 확인합니다.

```bash
helm-capsule verify .
```

`PROVEN`이 나오면 Helm post-renderer로 설치합니다.

```bash
helm upgrade --install opensearch opensearch/opensearch \
  -n opensearch \
  --create-namespace \
  -f ../opensearch-values.yaml \
  --post-renderer ./post-renderer
```

### 4. 주의사항

- `helm-capsule`은 Secret manager가 아닙니다. Namespace별 pull secret 또는
  ServiceAccount 연결은 별도로 준비해야 합니다.
- StorageClass는 자동 선택하지 않습니다. `plan`으로 필요한 입력을 확인하고
  values에 명시합니다.
- `verify`가 `PROVEN`이 되기 전에는 검증 완료 설치로 보지 않습니다.
- Istio Gateway의 `image: auto`처럼 실제 OCI 이미지가 아닌 placeholder는
  `UNPROVEN`으로 처리합니다.

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

Enable bash completion on Ubuntu:

```bash
sudo apt-get install -y bash-completion
helm-capsule completion bash | sudo tee /etc/bash_completion.d/helm-capsule >/dev/null
source /etc/bash_completion.d/helm-capsule
```

After this, pressing `Tab` after `helm-capsule ` shows available commands.
Command-specific help is also available:

```bash
helm-capsule help
helm-capsule help build
helm-capsule build --help
```

## Windows build

```powershell
$env:PATH = "C:\Users\user\.local\go\bin;$env:PATH"
go test ./...
go build -o helm-capsule.exe ./cmd/helm-capsule
```

## Basic flow

Plan chart-specific install inputs before building the image capsule:

```bash
helm-capsule plan bitnami/redis \
  --release redis \
  --namespace apps \
  -f values.yaml \
  --pull-secret registry-cloud-kt \
  --kube-version 1.34.3 \
  --out plan/
```

`plan` renders the chart and reports non-image inputs that may be required
before install, such as:

- PVCs with empty `storageClassName`
- PodSpecs that do not reference the expected `imagePullSecret`
- referenced Kubernetes Secrets
- Service ports to use when wiring Gateway or Ingress routes

It writes `plan.json`, `plan.yaml`, and `rendered.plan.yaml`. A `NEEDS_INPUT`
result is not a proof failure; it means values or namespace bootstrap work
should be completed before `build`.

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

## Install planning

`helm-capsule plan` intentionally does not choose a StorageClass or create
Secrets. It identifies chart inputs that are cluster-specific so operators can
set them explicitly in values before the proof path starts.

Example for OpenSearch:

```bash
helm-capsule plan opensearch/opensearch \
  --release opensearch \
  --namespace opensearch \
  -f opensearch-values.yaml \
  --pull-secret registry-cloud-kt \
  --kube-version 1.34.3 \
  --out plan-opensearch
```

If the plan reports `storageClass`, set the chart value that controls
`persistence.storageClass` or `storageClassName`, then rerun `plan` and
`build`.
