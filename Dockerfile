FROM golang:1.26.7-alpine3.23 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /roamvm ./cmd/roamvm

FROM alpine:3.23
RUN apk add --no-cache ca-certificates curl iproute2 iptables dnsmasq qemu-img cdrkit
ARG CH_VERSION=v53.0
ARG CH_SHA256=448af3d4e59b22c2987f7df94c213ad40fb53a10d437e42b5ee6c4fce7c29ecc
RUN test -n "$CH_SHA256" && curl -fLsS "https://github.com/cloud-hypervisor/cloud-hypervisor/releases/download/${CH_VERSION}/cloud-hypervisor-static" -o /usr/local/bin/cloud-hypervisor \
    && echo "$CH_SHA256  /usr/local/bin/cloud-hypervisor" | sha256sum -c - \
    && chmod +x /usr/local/bin/cloud-hypervisor
COPY --from=build /roamvm /usr/local/bin/roamvm
ENTRYPOINT ["/usr/local/bin/roamvm"]
