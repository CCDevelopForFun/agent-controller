# `runtime-image/` — `agent-runtime-base` container image

Self-contained image for running [agent-controller](https://github.com/CCDevelopForFun/agent-controller) ADL specs anywhere a container can run: locally with Docker / Podman, on K8s (the v0.4 `KubernetesBackend` target), or on any CI runner with a container engine.

Bundles three things:

- `agentctl` (Go CLI) — built from `cli/` in this repo by the image's first stage. Bit-reproducible vs `go install github.com/CCDevelopForFun/agent-controller/cli/cmd/agentctl@<rev>` at the same commit. The build commit's nearest umbrella tag (or its short SHA, when no tag is reachable) is embedded as `main.version` and surfaced in the `org.agent-controller.agentctl-version` label.
- `@agent-controller/runtime` (Pi adapter) — npm-installed globally at a version pinned in the Dockerfile
- `@agent-controller/runtime-opencode` (opencode adapter) + `opencode-ai` CLI — npm-installed globally at versions pinned in the Dockerfile

Published at `ghcr.io/ccdevelopforfun/agent-runtime-base`.

## Versioning

This image versions independently of `agent-controller` itself. The umbrella project tags `vX.Y.Z`; the image tags `runtime-image/vA.B.C`. Bundled component versions are pinned via build-args in the Dockerfile — bump them deliberately when the image rebuilds.

| Image tag | agentctl | `@agent-controller/runtime` | `@agent-controller/runtime-opencode` | `opencode-ai` |
|---|---|---|---|---|
| `0.1.0` | `v0.3.2` | `0.3.2` | `0.3.2` | `1.16.2` (broken — postinstall stub) |
| `0.1.1` | `v0.3.3` | `0.3.3` | `0.3.3` | `1.16.2` (postinstall runs, opencode adapter now works through an Anthropic-compatible gateway via `ANTHROPIC_BASE_URL`) |
| `0.1.2` | `v0.4.0` | `0.4.0` | `0.4.0` | `1.16.2` (adds the `--ndjson-stdout` flag the KubernetesBackend needs; required for `target.type: kubernetes` Pods) |
| `0.1.3` | `v0.5.0` | `0.5.0` | `0.5.0` | `1.16.2` (adds OTel root span + `spec.observability.tracing` ADL field; required for slice-5.1 tracing on K8s) |

The image's labels (`org.agent-controller.*`) carry the bundled versions for runtime inspection: `docker inspect <image> | jq '.[0].Config.Labels'`.

## Usage

### Local Docker

```bash
# Pull
docker pull ghcr.io/ccdevelopforfun/agent-runtime-base:0.1.5

# Run a self-contained spec (no registry-backed tools/extensions)
cat > /tmp/hello.yaml <<'EOF'
apiVersion: agent-controller.dev/v1alpha1
kind: Agent
metadata: { name: hello }
spec:
  model: { provider: anthropic, name: claude-sonnet-4-20250514 }
  persona: { role: Helpful demo, instructions: Answer concisely. }
  task: Say hello.
  tools: []
  runtime: { type: local }
EOF

docker run --rm \
  -e ANTHROPIC_API_KEY=sk-ant-... \
  -v /tmp/hello.yaml:/workspace/hello.yaml:ro \
  ghcr.io/ccdevelopforfun/agent-runtime-base:0.1.5 \
  run /workspace/hello.yaml
```

### opencode adapter

The image bundles both adapters. To use the opencode side, override the env that tells `agentctl` where to find the adapter binary:

```bash
docker run --rm \
  -e ANTHROPIC_API_KEY=sk-ant-... \
  -e AGENT_CONTROLLER_RUNTIME=/usr/local/lib/node_modules/@agent-controller/runtime-opencode/dist/index.js \
  -v /tmp/hello-opencode.yaml:/workspace/spec.yaml:ro \
  ghcr.io/ccdevelopforfun/agent-runtime-base:0.1.5 \
  run /workspace/spec.yaml
```

### Local K8s (`kind`)

```bash
kind create cluster --name agent-controller-dev
kind load docker-image ghcr.io/ccdevelopforfun/agent-runtime-base:0.1.5 --name agent-controller-dev

kubectl create secret generic anthropic --from-literal=ANTHROPIC_API_KEY=sk-ant-...
kubectl create configmap hello-spec --from-file=hello.yaml=/tmp/hello.yaml

kubectl apply -f - <<'EOF'
apiVersion: batch/v1
kind: Job
metadata: { name: hello-agent }
spec:
  template:
    spec:
      restartPolicy: Never
      containers:
        - name: agent
          image: ghcr.io/ccdevelopforfun/agent-runtime-base:0.1.5
          args: ["run", "/workspace/hello.yaml"]
          env:
            - { name: ANTHROPIC_API_KEY, valueFrom: { secretKeyRef: { name: anthropic, key: ANTHROPIC_API_KEY } } }
          volumeMounts:
            # subPath mounts JUST the spec file into /workspace — overlaying
            # only that one path. Without `subPath` the ConfigMap would
            # remount the entire /workspace directory read-only, which
            # breaks the Pi adapter (it writes `.pi/` and `.agent-controller/`
            # state under cwd at session start).
            - { name: spec, mountPath: /workspace/hello.yaml, subPath: hello.yaml, readOnly: true }
      volumes:
        - { name: spec, configMap: { name: hello-spec } }
EOF

kubectl logs -f job/hello-agent
```

The v0.4 `KubernetesBackend` (slice 4.3) automates this — `agentctl run my-spec.yaml --binding k8s.yaml` will construct the Job, stream events back, and exit when the run finishes. Until then, this manifest is the manual equivalent.

## Image properties

- **Base**: `node:22.19-bookworm-slim`
- **User**: `runner` (UID 10001), non-root
- **PID 1**: `tini` (signal forwarding for SIGTERM / SIGINT from the orchestrator)
- **Workdir**: `/workspace`
- **Entrypoint**: `agentctl`
- **Architectures**: `linux/amd64`, `linux/arm64`

## Building locally

The build context is the repo root (the Dockerfile builds `agentctl` from `cli/` in-tree) — run from the repository root, pointing `-f` at the Dockerfile inside `runtime-image/`:

```bash
# Single-arch (your machine's native)
docker build -f runtime-image/Dockerfile -t agent-runtime-base:dev .

# Multi-arch (requires Docker buildx)
docker buildx build \
  --platform linux/amd64,linux/arm64 \
  -f runtime-image/Dockerfile \
  -t agent-runtime-base:dev \
  .
```

To override bundled versions:

```bash
docker build \
  --build-arg AGENTCTL_VERSION=0.3.3 \
  --build-arg RUNTIME_VERSION=0.3.3 \
  --build-arg RUNTIME_OPENCODE_VERSION=0.3.3 \
  --build-arg OPENCODE_VERSION=1.16.2 \
  -f runtime-image/Dockerfile \
  -t agent-runtime-base:dev \
  .
```

`AGENTCTL_VERSION` is just a label/embedded-version string (the binary's actual code comes from the `cli/` tree at the build commit); the npm package args control which adapter versions get installed.

## Publishing

Pushed by [`.github/workflows/runtime-image.yml`](../.github/workflows/runtime-image.yml) on:

- Tags matching `runtime-image/vX.Y.Z` (stable) or `runtime-image/vX.Y.Z-rc1` (prerelease)
- Manual `workflow_dispatch` with an explicit tag input

The workflow builds for `linux/amd64` + `linux/arm64`, pushes to GHCR, and tags stable releases as `:latest`. Pre-release tags push the version-tag only.

## Cross-references

- [agent-controller README](https://github.com/CCDevelopForFun/agent-controller#readme)
- [ROADMAP.md → v0.4 Kubernetes backend](https://github.com/CCDevelopForFun/agent-controller/blob/main/ROADMAP.md)
- [docs/architecture/overview.md](https://github.com/CCDevelopForFun/agent-controller/blob/main/docs/architecture/overview.md)
