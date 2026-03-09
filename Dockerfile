FROM alpine:3

ENV HOME=/app
USER 1000:1000

WORKDIR /app

ARG TARGETPLATFORM=linux/amd64
COPY ${TARGETPLATFORM}/publicip-exporter /bin/publicip-exporter

ENTRYPOINT ["/bin/publicip-exporter"]
