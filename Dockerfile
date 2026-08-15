# The runtime image is based on docker:cli for two reasons: it carries the
# compose plugin, which the compose update strategy shells out to, and it is the
# same image backup-tower starts as its own helper container. Reusing one image
# for both roles keeps the archiving code identical on both sides.
# The build stage runs on the machine doing the building, not on the platform
# being built for. Go cross-compiles natively, so an arm64 image is produced by
# an amd64 runner at full speed instead of through QEMU emulation.
FROM --platform=$BUILDPLATFORM golang:1.24-alpine AS build

WORKDIR /src

# Dependencies first, so code changes do not invalidate the module cache.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Supplied by buildx for each platform in the target list.
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build \
    -trimpath \
    -ldflags "-s -w -X github.com/Hudint/backup-tower/internal/version.Version=${VERSION}" \
    -o /out/backup-tower ./cmd/backup-tower

FROM docker:cli

COPY --from=build /out/backup-tower /usr/local/bin/backup-tower

ENV TOWER_BACKUP_DIR=/backups

ENTRYPOINT ["/usr/local/bin/backup-tower"]
CMD ["--help"]
