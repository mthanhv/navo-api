# syntax=docker/dockerfile:1
# Built in CI (never on the server). Static binaries on a distroless, non-root base.
FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN set -eu; for cmd in api migrate navo-user navo-seed; do \
      CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/$cmd ./cmd/$cmd; \
    done; \
    mv /out/api /out/navo-api; mv /out/migrate /out/navo-migrate

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/ /usr/local/bin/
USER nonroot:nonroot
EXPOSE 8080
HEALTHCHECK --interval=15s --timeout=5s --start-period=10s --retries=4 CMD ["/usr/local/bin/navo-api", "healthcheck"]
ENTRYPOINT ["/usr/local/bin/navo-api"]
