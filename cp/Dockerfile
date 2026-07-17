# sigmahub-cp — the SigmaHub control plane, self-hostable as one container.
# Build from the repository root:  docker build -f cp/Dockerfile -t sigmahub-cp cp
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/sigmahub-cp ./cmd/sigmahub-cp

# Distroless-style runtime: static binary, non-root, CA certs for ACME/webhook
# egress. The KMS key file and any state live on the mounted /data volume.
FROM alpine:3.21
RUN apk add --no-cache ca-certificates && adduser -D -u 10001 sigmahub
USER sigmahub
WORKDIR /data
ENV CP_ADDR=:8080 \
    CP_KMS_KEY_FILE=/data/cp-kms.key
COPY --from=build /out/sigmahub-cp /usr/local/bin/sigmahub-cp
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/sigmahub-cp"]
