# The operator's image is one static Go binary. The operator holds
# cluster credentials and API keys, so the image has no shell, no libc,
# and no tools.

FROM golang:1.27.0-bookworm AS build
WORKDIR /src
# The module files come first, so a source change reuses the cached
# download layer.
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
COPY healthchecks ./healthchecks
ARG VERSION=dev
# CGO_ENABLED=0 makes a static binary that runs without a loader.
# -trimpath removes the build machine's paths from it.
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /healthchecks-operator .

FROM scratch
COPY --from=build /healthchecks-operator /healthchecks-operator
# The probes connect to HTTPS endpoints and the Healthchecks API, so the
# image needs the CA certificates that scratch does not have.
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
USER 65532:65532
ENTRYPOINT ["/healthchecks-operator"]
