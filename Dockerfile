# syntax=docker/dockerfile:1
FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/fruit-fs ./cmd/fruit-fs \
 && mkdir -p /out/data /out/state

FROM gcr.io/distroless/static:nonroot
COPY --from=build /out/fruit-fs /fruit-fs
# Empty mount points owned by the nonroot user (uid 65532).
COPY --from=build --chown=65532:65532 /out/data /data
COPY --from=build --chown=65532:65532 /out/state /state
ENV FRUITFS_DATA=/data FRUITFS_STATE=/state FRUITFS_LISTEN=:8080
VOLUME ["/data", "/state"]
EXPOSE 8080
USER 65532:65532
HEALTHCHECK --interval=30s --timeout=5s --retries=3 CMD ["/fruit-fs", "healthcheck"]
ENTRYPOINT ["/fruit-fs"]
