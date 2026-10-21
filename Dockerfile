FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY *.go index.html ./
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/monitor . \
 && mkdir -p /out/data

# Final image: just the static binary (~5 MB), no shell, no OS.
FROM scratch
COPY --from=build /out/monitor /monitor
COPY --from=build --chown=65534:65534 /out/data /data

ENV PORT=8080 \
    DATA_DIR=/data \
    TZ=Asia/Makassar \
    DOWN_AFTER=180

USER 65534:65534
VOLUME ["/data"]
EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=4s --start-period=10s --retries=3 \
  CMD ["/monitor", "-healthcheck"]

ENTRYPOINT ["/monitor"]
