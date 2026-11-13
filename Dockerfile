FROM alpine:3.23 AS qemu-build
RUN apk add --no-cache build-base bash curl xz patch python3 meson ninja pkgconf linux-headers glib-dev pixman-dev zlib-dev zstd-dev bzip2-dev libcap-ng-dev attr-dev dtc-dev pax-utils
WORKDIR /build
RUN curl --fail --location https://download.qemu.org/qemu-10.2.4.tar.xz -o qemu.tar.xz \
    && echo '821b545b92f165e57dddccac5077d76d4d436a226595b8813ad59306bbfd0746  qemu.tar.xz' | sha256sum -c - \
    && tar -xJf qemu.tar.xz
COPY nix/qemu-external-ram.patch /build/external-ram.patch
WORKDIR /build/qemu-10.2.4
RUN set -eu; set -o pipefail; patch -p1 < /build/external-ram.patch \
    && ./configure --prefix=/usr/local --with-pkgversion=roamvm-external-ram-v1 \
       --target-list=x86_64-softmmu --enable-kvm --disable-tcg --disable-docs \
       --disable-tools --disable-guest-agent --disable-modules --enable-zstd --enable-virtfs \
    && make -j"$(getconf _NPROCESSORS_ONLN)" \
    && make install DESTDIR=/out \
    && rm -rf build \
    && scanelf --needed --nobanner --format '%n#p' --recursive /out/usr/local \
       | tr ',' '\n' | sort -u | sed '/^$/d;s/^/so:/' > /out/runtime-deps

FROM golang:1.26.7-alpine3.23 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /roamvm ./cmd/roamvm

FROM alpine:3.23 AS runtime
RUN apk add --no-cache ca-certificates curl iproute2 iptables dnsmasq qemu-img qemu-system-x86_64 cdrkit
COPY --from=build /roamvm /usr/local/bin/roamvm
ENTRYPOINT ["/usr/local/bin/roamvm"]

# Explicit opt-in: docker build --target v2 ...
FROM runtime AS v2
COPY --from=qemu-build /out/usr/local /usr/local
COPY --from=qemu-build /out/runtime-deps /tmp/qemu-runtime-deps
RUN apk add --no-cache $(cat /tmp/qemu-runtime-deps) && rm /tmp/qemu-runtime-deps

# Keep the default legacy image and its packaged QEMU unchanged.
FROM runtime AS default
