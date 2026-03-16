FROM golang:1.26 AS build

WORKDIR /src

COPY go.* ./
RUN \
  # --mount=type=cache,dst=/go \
  go mod download

COPY main.go main.go
COPY cmd/ cmd/

RUN \
  # --mount=type=cache,dst=/go \
  # CGO_ENABLED=0 GOOS=linux GOARCH=$TARGETARCH \
  go build -trimpath

# FROM scratch
FROM debian:trixie

ENV PATH=${PATH}:/

LABEL maintainer=github.com/meln5674

# COPY --from=build /src/kube-headscale /kube-headscale
COPY kube-headscale /kube-headscale

EXPOSE 8080

ENTRYPOINT [ "/kube-headscale" ]
