FROM golang:1.26.7-alpine3.23 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /roamvm ./cmd/roamvm

FROM alpine:3.23
RUN apk add --no-cache ca-certificates curl iproute2 iptables dnsmasq qemu-img qemu-system-x86_64 cdrkit
COPY --from=build /roamvm /usr/local/bin/roamvm
ENTRYPOINT ["/usr/local/bin/roamvm"]
