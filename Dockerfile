# The intagent team server.
#
#   docker build -t intagent .
#   docker run --rm -v intagent:/data intagent token add alice --config /data/team.json
#   docker run -d --name intagent -p 7400:7400 -v intagent:/data intagent
#
# Everything the server keeps (team.json, the board snapshot, the dashboard
# session key) lives in the /data volume. Add --tls-cert and --tls-key to the
# serve command, or put a TLS-terminating proxy in front.
# The build stage runs on the build machine and cross-compiles, so a
# multi-platform image needs no emulation.
FROM --platform=$BUILDPLATFORM golang:1.24-alpine AS build
WORKDIR /src
COPY . .
ARG VERSION=dev TARGETOS TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/intagent ./cmd/intagent \
 && mkdir -p /out/data

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/intagent /usr/local/bin/intagent
COPY --from=build --chown=nonroot:nonroot /out/data /data
VOLUME /data
EXPOSE 7400
ENTRYPOINT ["/usr/local/bin/intagent"]
CMD ["serve", "--config", "/data/team.json", "--data", "/data", "--addr", ":7400"]
