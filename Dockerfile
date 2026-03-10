# syntax=docker/dockerfile:1.7

FROM gcr.io/distroless/static-debian12:nonroot

ARG TARGETARCH

COPY dist/linux/${TARGETARCH}/cwproxy /cwproxy

EXPOSE 8081

ENTRYPOINT ["/cwproxy"]
