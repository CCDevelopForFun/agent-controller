# syntax=docker/dockerfile:1.7

# agent-runtime-base — container image for running agent-controller ADL specs.
#
# Bundles:
#   - agentctl binary (built from cli/ in this repo at image-build time;
#     bit-reproducible vs `go install ...@vX.Y.Z`)
#   - Node 22.19+ with @agent-controller/runtime (Pi adapter) installed globally
#   - @agent-controller/runtime-opencode (opencode adapter) installed globally
#   - opencode CLI (required at runtime by the opencode adapter)
#
# Versioning: this image versions independently of the umbrella agent-controller
# tag (see ROADMAP.md). All bundled npm package versions are pinned via ARGs
# below so the image is bit-reproducible; bump them deliberately when refreshing.
# The agentctl binary version is whatever `cli/` looks like at the build commit
# — the workflow tags the image at `runtime-image/vX.Y.Z`, the labels record the
# Go-side state via `org.opencontainers.image.revision` (the commit SHA).
#
# Build CONTEXT is the repo root (not runtime-image/) — needed so the COPY of
# cli/ works. See runtime-image/.dockerignore for what's excluded.
#
# Build (multi-arch via buildx):
#   docker buildx build --platform linux/amd64,linux/arm64 \
#     -f runtime-image/Dockerfile \
#     -t ghcr.io/ccdevelopforfun/agent-runtime-base:0.1.0 \
#     --push .
#
# Use:
#   docker run --rm \
#     -e ANTHROPIC_API_KEY \
#     -v "$(pwd)/spec.yaml":/workspace/spec.yaml \
#     ghcr.io/ccdevelopforfun/agent-runtime-base:0.1.0 \
#     run /workspace/spec.yaml

ARG RUNTIME_VERSION=0.6.0
ARG RUNTIME_OPENCODE_VERSION=0.6.0
# Pin opencode-ai so re-running the same image tag produces a bit-identical
# image. Bumping here is deliberate — verify the new version against our
# adapter's @opencode-ai/sdk peer-dep range before changing. Codex pass 1
# of slice 4.2 caught the original `latest` defeating reproducibility.
ARG OPENCODE_VERSION=1.16.2

############################
# Stage 1 — build agentctl from source
############################
# The cli/ tree is the source of truth; building here matches what
# `go install github.com/CCDevelopForFun/agent-controller/cli/cmd/agentctl@vX.Y.Z`
# would do, bit-for-bit, without requiring a public release URL.
# Go 1.25 needed for OTel SDK v1.44+ (slice 5.1 bump). Keep this image
# tag in sync with cli/go.mod's `go` directive so the in-image builder
# never has to fall back to toolchain auto-download. Codex pass 2 of
# slice 5.1 caught the drift.
FROM --platform=$BUILDPLATFORM golang:1.25-bookworm AS agentctl-build
WORKDIR /src
COPY cli/go.mod cli/go.sum ./
RUN go mod download
COPY cli/ ./
ARG TARGETOS
ARG TARGETARCH
# Build version label uses the image's runtime-image tag (passed in by the
# workflow). Locally falls back to "dev". The Go binary embeds this as
# `main.version` via -ldflags -X — matches the release workflow's pattern.
ARG AGENTCTL_VERSION=dev
RUN --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
      go build -trimpath \
        -ldflags="-s -w -X 'main.version=${AGENTCTL_VERSION}'" \
        -o /out/agentctl ./cmd/agentctl

############################
# Stage 2 — runtime image
############################
FROM node:22.19-bookworm-slim AS runtime

ARG RUNTIME_VERSION
ARG RUNTIME_OPENCODE_VERSION
ARG OPENCODE_VERSION
ARG AGENTCTL_VERSION=dev

LABEL org.opencontainers.image.title="agent-runtime-base"
LABEL org.opencontainers.image.description="Container runtime for agent-controller ADL specs (Pi + opencode adapters)"
LABEL org.opencontainers.image.source="https://github.com/CCDevelopForFun/agent-controller"
LABEL org.opencontainers.image.licenses="MIT"
LABEL org.agent-controller.agentctl-version="${AGENTCTL_VERSION}"
LABEL org.agent-controller.runtime-version="${RUNTIME_VERSION}"
LABEL org.agent-controller.runtime-opencode-version="${RUNTIME_OPENCODE_VERSION}"
LABEL org.agent-controller.opencode-version="${OPENCODE_VERSION}"

# tini: PID-1 signal forwarding so SIGTERM/SIGINT from K8s reach agentctl
# ca-certificates: TLS handshakes to api.anthropic.com, the npm registry,
#                  and any MCP servers / extension sources fetched at runtime
RUN apt-get update \
 && apt-get install -y --no-install-recommends tini ca-certificates \
 && rm -rf /var/lib/apt/lists/*

# Copy the architecture-matching agentctl binary from the build stage
COPY --from=agentctl-build /out/agentctl /usr/local/bin/agentctl

# Install the npm adapters + opencode CLI at the pinned versions.
# --ignore-scripts skips Pi's nested ~/.pi setup hook (which would write
# into the runner user's home at install time — not what we want inside
# an immutable image; the adapter handles its own state at session start
# under /workspace/.pi/).
#
# opencode-ai's `postinstall.mjs` downloads the platform-specific opencode
# binary (without it, /usr/local/bin/opencode is a stub that exits with
# "postinstall script was not run"). --ignore-scripts blocks that too, so
# we run it explicitly here as root before switching to USER 10001 — it
# writes the real binary into the package's bin/ dir which is root-owned.
# Image v0.1.1 of slice 4.2.1 caught the original missing postinstall.
RUN npm install -g --ignore-scripts --no-audit --no-fund \
      "@agent-controller/runtime@${RUNTIME_VERSION}" \
      "@agent-controller/runtime-opencode@${RUNTIME_OPENCODE_VERSION}" \
      "opencode-ai@${OPENCODE_VERSION}" \
 && (cd /usr/local/lib/node_modules/opencode-ai && node postinstall.mjs)

# Default to the Pi adapter. Specs that declare runtime.type: local-opencode
# need the env overridden at run time:
#   AGENT_CONTROLLER_RUNTIME=/usr/local/lib/node_modules/@agent-controller/runtime-opencode/dist/index.js
ENV AGENT_CONTROLLER_RUNTIME=/usr/local/lib/node_modules/@agent-controller/runtime/dist/index.js

# Expose the bundled `pi` binary to `agentctl install` (Pi is a transitive
# dep of @agent-controller/runtime; its .bin shim isn't on PATH inside the
# image). `resolvePiBin` in cli/cmd/agentctl/install.go checks PI_BIN as
# the second precedence after the --pi-bin flag. Codex pass 7 of slice 4.2.
ENV PI_BIN=/usr/local/lib/node_modules/@agent-controller/runtime/node_modules/.bin/pi

# Standard workdir. Spec files / ConfigMap mounts land here.
WORKDIR /workspace

# Non-root user. K8s `securityContext.runAsNonRoot: true` (which the v0.4
# matcher will enforce when requirements.sandbox is true) requires a non-
# root image default. UID 10001 is conventional and avoids common host
# UID collisions.
RUN useradd --create-home --home-dir /home/runner --uid 10001 --shell /bin/bash runner \
 && chown -R runner:runner /workspace
# Numeric UID (not the `runner` username) so K8s `runAsNonRoot: true` can
# verify the image without the Pod also having to set `runAsUser`. Kubelet
# rejects images whose USER is a name because it cannot prove non-rootness
# without resolving /etc/passwd inside the container. Codex pass 5 of slice
# 4.2 caught this.
USER 10001

# tini as PID 1 handles signal forwarding; agentctl is the actual command.
# `docker run <image>` → shows help. `docker run <image> run /spec.yaml` → runs the spec.
ENTRYPOINT ["/usr/bin/tini", "--", "agentctl"]
CMD ["--help"]
