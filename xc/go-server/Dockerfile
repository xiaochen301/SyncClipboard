# syntax=docker/dockerfile:1
# xc/go-server — SyncClipboard self-hosted server, Go edition.
# Build: docker build -t syncclipboard-server-xc xc/go-server

# ---- build stage ----
FROM golang:1.25-alpine AS build
ARG GOPROXY=https://proxy.golang.org,direct
ENV GOPROXY=$GOPROXY
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/syncclipboard-server-xc . \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/synccheck ./cmd/synccheck

# ---- runtime stage ----
FROM alpine:3.21
RUN apk add --no-cache ca-certificates tzdata
COPY --from=build /out/syncclipboard-server-xc /usr/local/bin/syncclipboard-server-xc
COPY --from=build /out/synccheck /usr/local/bin/synccheck
WORKDIR /app
EXPOSE 5033
ENTRYPOINT ["syncclipboard-server-xc"]
CMD ["-data", "/app/data/server", "-config", "/app/appsettings.json", "-listen", "0.0.0.0:5033"]
