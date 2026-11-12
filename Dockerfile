# Build context: the repository root.
FROM golang:1.24-alpine AS build
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/agentirc ./cmd/agentirc

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/agentirc /agentirc
# /data holds log.jsonl and tokens.txt; mount it as a volume.
# Timestamps use TZ; set it to your zone, e.g. -e TZ=Europe/Berlin
ENV TZ=UTC
VOLUME ["/data"]
EXPOSE 8770
ENTRYPOINT ["/agentirc", "serve", "-addr", "0.0.0.0:8770", "-data", "/data", "-tokens", "/data/tokens.txt", "-prefix", "/irc"]
