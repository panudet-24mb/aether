FROM golang:1.26.8-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/api ./cmd/api && \
    CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/admin ./cmd/admin && \
    CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/migrate ./cmd/migrate && \
    CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/mqtt ./cmd/mqtt && \
    CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/simulator ./cmd/simulator
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/mqtt-provisioner ./cmd/mqtt-provisioner
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/mqtt-commander ./cmd/mqtt-commander
FROM alpine:3.23 AS javascript
RUN apk add --no-cache python3 py3-pip python3-dev build-base && python3 -m pip install --break-system-packages --no-cache-dir --target=/python quickjs==1.19.4
FROM alpine:3.23
RUN apk add --no-cache ca-certificates python3 && addgroup -g 10001 aether && adduser -D -u 10001 -G aether aether
WORKDIR /app
COPY --from=build /out/ /app/
COPY migrations /app/migrations
COPY --from=javascript /python /app/python
COPY sandbox /app/sandbox
USER 10001:10001
EXPOSE 8080
ENTRYPOINT ["/app/api"]
