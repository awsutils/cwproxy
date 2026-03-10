# syntax=docker/dockerfile:1.7

FROM alpine:3.21

ARG TARGETARCH

RUN apk add --no-cache ca-certificates \
    && addgroup -S cwproxy \
    && adduser -S -D -H -G cwproxy cwproxy

COPY --chmod=0555 dist/linux/${TARGETARCH}/cwproxy /cwproxy

USER cwproxy:cwproxy

EXPOSE 8081

ENTRYPOINT ["/cwproxy"]
