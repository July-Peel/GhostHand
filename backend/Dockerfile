FROM node:22-alpine AS web
WORKDIR /src/web
COPY backend/web/package.json ./
RUN npm install --no-audit --no-fund
COPY backend/web/ ./
RUN npm run build

FROM golang:1.26-bookworm AS server
WORKDIR /src
COPY proto/ ./proto/
COPY backend/go.mod backend/go.sum ./backend/
WORKDIR /src/backend
ENV GOWORK=off CGO_ENABLED=0
RUN go mod download
COPY backend/cmd/ ./cmd/
COPY backend/internal/ ./internal/
COPY --from=web /src/web/dist/ ./cmd/server/web/dist/
RUN go build -mod=mod -trimpath -ldflags="-s -w" -o /out/ghosthand ./cmd/server \
    && install -d /out/rootfs/data \
    && touch /out/rootfs/data/.keep

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=server /out/ghosthand /ghosthand
COPY --from=server --chown=65532:65532 /out/rootfs/ /
ENV GH_DATA_DIR=/data GOMEMLIMIT=32MiB
EXPOSE 8080
ENTRYPOINT ["/ghosthand"]
