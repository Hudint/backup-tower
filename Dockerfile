# The runtime image is based on docker:cli for two reasons: it carries the
# compose plugin, which the compose update strategy shells out to, and it is the
# same image backup-tower starts as its own helper container. Reusing one image
# for both roles keeps the archiving code identical on both sides.
FROM golang:1.24-alpine AS build

WORKDIR /src

# Dependencies first, so code changes do not invalidate the module cache.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=dev
RUN CGO_ENABLED=0 go build \
    -trimpath \
    -ldflags "-s -w -X github.com/hudint/backup-tower/internal/version.Version=${VERSION}" \
    -o /out/backup-tower ./cmd/backup-tower

FROM docker:cli

COPY --from=build /out/backup-tower /usr/local/bin/backup-tower

ENV TOWER_BACKUP_DIR=/backups

ENTRYPOINT ["/usr/local/bin/backup-tower"]
CMD ["--help"]
