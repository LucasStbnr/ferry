# Build Ferry from source. This is the Dockerfile used by `make docker` and by
# CI; releases use Dockerfile.release, which packages a binary GoReleaser has
# already built.

FROM golang:1.27-alpine AS build

WORKDIR /src

# Dependencies first, so a source-only change reuses the module cache.
COPY go.mod go.sum ./
RUN go mod download && go mod verify

COPY . .

ARG VERSION=dev
ARG COMMIT=none
ARG DATE=unknown

# CGO is off because the SQLite driver is pure Go: the result is a static
# binary that runs on distroless with nothing else in the image.
ENV CGO_ENABLED=0
RUN go build -trimpath \
      -ldflags "-s -w \
        -X github.com/LucasStbnr/ferry/internal/cli.version=${VERSION} \
        -X github.com/LucasStbnr/ferry/internal/cli.commit=${COMMIT} \
        -X github.com/LucasStbnr/ferry/internal/cli.date=${DATE}" \
      -o /out/ferry ./cmd/ferry

FROM gcr.io/distroless/static-debian12:nonroot

LABEL org.opencontainers.image.title="ferry" \
      org.opencontainers.image.description="IMAP and SMTP bridge that puts Resend accounts into a mail client" \
      org.opencontainers.image.source="https://github.com/LucasStbnr/ferry" \
      org.opencontainers.image.licenses="MIT"

COPY --from=build /out/ferry /usr/local/bin/ferry

# All state lives here: the database, the message blobs and the certificate.
# Mount a volume, or the mail disappears when the container is replaced.
ENV FERRY_DATA_DIR=/data
VOLUME ["/data"]

# Self-hosting uses the standard ports. They are above 1024 inside the
# container so the process never needs to be root; map them with
# `-p 993:9993 -p 465:9465` if clients expect the usual numbers.
EXPOSE 9993 9465 8443

USER nonroot:nonroot

ENTRYPOINT ["/usr/local/bin/ferry"]
CMD ["serve"]
