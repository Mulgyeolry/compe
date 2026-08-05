ARG GOLANG_IMAGE=m.daocloud.io/docker.io/library/golang:1.25-bookworm
ARG RUNTIME_IMAGE=m.daocloud.io/docker.io/library/debian:bookworm-slim

FROM ${GOLANG_IMAGE} AS builder
ARG GOPROXY=https://goproxy.cn,direct
ENV GOPROXY=${GOPROXY}
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/competition-assistant ./cmd/competition-assistant

FROM ${RUNTIME_IMAGE}
ARG APT_MIRROR=
RUN if [ -n "${APT_MIRROR}" ]; then sed -i "s|http://deb.debian.org|${APT_MIRROR}|g" /etc/apt/sources.list.d/debian.sources; fi \
    && apt-get -o Acquire::Retries=3 -o Acquire::http::Timeout=30 update \
    && DEBIAN_FRONTEND=noninteractive apt-get -o Acquire::Retries=3 -o Acquire::http::Timeout=30 install -y --no-install-recommends ca-certificates poppler-utils tzdata \
    && apt-get clean \
    && useradd --system --uid 10001 --create-home appuser \
    && mkdir -p /data /app/config \
    && chown -R appuser:appuser /data /app
COPY --from=builder /out/competition-assistant /usr/local/bin/competition-assistant
USER appuser
WORKDIR /app
ENV CONFIG_PATH=/app/config/sources.yaml
ENTRYPOINT ["competition-assistant"]
CMD ["serve"]
