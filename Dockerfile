FROM --platform=$BUILDPLATFORM golang:1.24 AS builder

WORKDIR /src

COPY . .

ARG TARGETOS TARGETARCH
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -o /go/bin/netpol-finalizer .

# STEP 2: Build small image
FROM gcr.io/distroless/static-debian12
COPY --from=builder --chown=root:root /go/bin/netpol-finalizer /bin/netpol-finalizer

CMD ["/bin/netpol-finalizer"]
