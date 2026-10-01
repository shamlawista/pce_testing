# syntax=docker/dockerfile:1
# ---- builder stage ----
FROM golang:1.27.0@sha256:4013ae0f9e7994f8535c58c811f8f863fbed38b72e0d51e6592156f758d66146 AS builder

WORKDIR /build

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .

RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go install ./cmd/...

# ---- runtime stage ----
FROM debian:bookworm-slim@sha256:abd67ffcfa541b485a3dff59865ab629aa048a6c613e639d36e7456b0b229241

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
