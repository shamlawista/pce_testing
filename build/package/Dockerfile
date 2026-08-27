# syntax=docker/dockerfile:1
# ---- builder stage ----
FROM golang:1.26.5@sha256:ae5a2316d12f3e78fd99177dad452e6ad4f240af2d71d57b480c3477f250fec6 AS builder

WORKDIR /build

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .

RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go install ./cmd/...

# ---- runtime stage ----
FROM debian:bookworm-slim@sha256:88200866dfff7ea7f5cbcb6ec7c8a701889efe6fe859fe64d6990e4b07ea4171

LABEL org.opencontainers.image.authors="Motoki TAKENAKA <m.takenaka@ntt.com>, Wataru MISHIMA <w.mishima@ntt.com>"

RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates \
 && rm -rf /var/lib/apt/lists/*

COPY --from=builder /go/bin/pola /usr/local/bin/
COPY --from=builder /go/bin/polad /usr/local/bin/

# polad reads polad.yaml from the working directory (mounted at runtime).
WORKDIR /pola

ENTRYPOINT ["polad"]
CMD ["-f", "polad.yaml"]
