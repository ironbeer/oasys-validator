# Build Geth in a stock Go builder container
FROM golang:1.24.10-bookworm as base

# Support setting various labels on the final image
ARG COMMIT=""
ARG VERSION=""
ARG BUILDNUM=""

RUN apt update && apt install -y git ca-certificates

# Get dependencies - will also be cached if we won't change go.mod/go.sum
COPY go.mod /go-ethereum/
COPY go.sum /go-ethereum/
RUN cd /go-ethereum && go mod download

ADD . /go-ethereum

# For blst
ENV CGO_CFLAGS="-O -D__BLST_PORTABLE__"
ENV CGO_CFLAGS_ALLOW="-O -D__BLST_PORTABLE__"

FROM base as geth-builder
RUN cd /go-ethereum && go run build/ci.go install -static ./cmd/geth

FROM base as plugin-builder
RUN cd /go-ethereum && go run build/ci.go plugin

# Binary extraction stages
FROM scratch as binaries
COPY --from=geth-builder /go-ethereum/build/bin/geth /geth
COPY --from=plugin-builder /go-ethereum/build/bin/suspicious_txfilter.so /suspicious_txfilter.so

FROM scratch as plugin-binaries
COPY --from=plugin-builder /go-ethereum/build/bin/suspicious_txfilter.so /suspicious_txfilter.so

# Final stage
FROM gcr.io/distroless/base-debian12
COPY --from=geth-builder /go-ethereum/build/bin/geth /usr/local/bin/geth
COPY --from=geth-builder /etc/ssl /etc/ssl
COPY --from=geth-builder /usr/share/ca-certificates /usr/share/ca-certificates

EXPOSE 8545 8546 30303 30303/udp
ENTRYPOINT ["geth"]

# Add some metadata labels to help programatic image consumption
ARG COMMIT=""
ARG VERSION=""
ARG BUILDNUM=""

LABEL commit="$COMMIT" version="$VERSION" buildnum="$BUILDNUM"
