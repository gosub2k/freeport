FROM golang:1.25-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
RUN CGO_ENABLED=0 go build -o /csi-driver .

FROM alpine:3.21
# mount/umount needed for NodePublishVolume/NodeUnpublishVolume
RUN apk add --no-cache util-linux
COPY --from=builder /csi-driver /csi-driver
ENTRYPOINT ["/csi-driver"]
