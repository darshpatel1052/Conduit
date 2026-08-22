# Conduit gateway — multi-stage build.
#
# The result is a distroless image containing one static binary and nothing
# else: no shell, no package manager, no libc. That is a smaller attack surface
# than a slim base, and it also means the image cannot be exec'd into during an
# incident — a trade this project takes deliberately, because a gateway that
# handles credentials should not ship a shell to production. Debug with the
# admin endpoints, the metrics and the logs; those are the interfaces built for
# it.

# --- build ------------------------------------------------------------------
FROM golang:1.26-alpine AS build

WORKDIR /src

# Dependencies first, in their own layer. Source edits are frequent and
# go.mod/go.sum edits are not, so this keeps the module download cached across
# nearly every rebuild.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .

ARG VERSION=dev
ARG TARGETOS=linux
ARG TARGETARCH=amd64

# CGO off produces a genuinely static binary, which is what makes the distroless
# static base viable — with cgo the image would need a libc to run against.
#
#   -s -w        strip the symbol table and DWARF; roughly a third off the size
#   -trimpath    keeps build-host paths out of the binary and panics
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -buildvcs=false -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
      -o /out/conduit ./cmd/gateway

RUN --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -buildvcs=false -trimpath -ldflags "-s -w" \
      -o /out/mockprovider ./scripts/load/mockprovider

FROM gcr.io/distroless/static-debian12:nonroot AS mock-provider
COPY --from=build /out/mockprovider /mockprovider
USER 65532:65532
EXPOSE 19100
ENTRYPOINT ["/mockprovider", "-addr", ":19100"]

# --- runtime ----------------------------------------------------------------
FROM gcr.io/distroless/static-debian12:nonroot AS gateway

# Sane defaults so the image runs without a mounted config, against a gateway
# operator's own routes file.
WORKDIR /app
COPY --from=build /out/conduit /app/conduit
COPY configs/ /app/configs/

# nonroot (uid 65532) comes from the base image. Declared explicitly because a
# Kubernetes securityContext with runAsNonRoot refuses to start a container
# whose user it cannot resolve to a numeric id.
USER 65532:65532

EXPOSE 8080

# No HEALTHCHECK instruction: there is no shell or curl in the image to run one,
# and in Kubernetes the kubelet's httpGet probes replace it entirely. See
# The Compose file defines the probes that run in the supported local stack.

ENTRYPOINT ["/app/conduit"]
CMD ["-config", "/app/configs/conduit.yaml"]
