FROM golang:1.26-alpine AS builder

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -ldflags="-s -w" -trimpath -o /yarilo ./cmd/yarilo

FROM alpine:3.23
RUN apk add --no-cache ca-certificates tzdata
COPY --from=builder /yarilo /usr/local/bin/yarilo
COPY config/yarilo.yaml /etc/yarilo/yarilo.yaml

EXPOSE 143 993 8080

ENTRYPOINT ["/usr/local/bin/yarilo", "-config", "/etc/yarilo/yarilo.yaml"]
