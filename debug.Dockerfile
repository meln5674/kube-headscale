FROM golang:1.26 AS build

WORKDIR /src

COPY go.* ./
RUN \
  go mod download

COPY main.go main.go
COPY cmd/ cmd/

RUN go build -trimpath

FROM debian:trixie

LABEL maintainer=github.com/meln5674

COPY bin/kube-headscale /usr/bin/kube-headscale

EXPOSE 8080

ENTRYPOINT [ "/usr/binkube-headscale" ]
