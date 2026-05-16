FROM golang:1.26-alpine AS builder

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -trimpath -o /out/yarilo                    ./app/yarilo                    && \
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -trimpath -o /out/yarilo-auth               ./app/yarilo-auth               && \
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -trimpath -o /out/yarilo-anvil              ./app/yarilo-anvil              && \
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -trimpath -o /out/yarilo-director           ./app/yarilo-director           && \
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -trimpath -o /out/yarilo-imap               ./app/yarilo-imap               && \
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -trimpath -o /out/yarilo-imap-login         ./app/yarilo-imap-login         && \
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -trimpath -o /out/yarilo-pop3               ./app/yarilo-pop3               && \
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -trimpath -o /out/yarilo-pop3-login         ./app/yarilo-pop3-login         && \
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -trimpath -o /out/yarilo-lmtp               ./app/yarilo-lmtp               && \
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -trimpath -o /out/yarilo-submission         ./app/yarilo-submission         && \
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -trimpath -o /out/yarilo-submission-login          ./app/yarilo-submission-login          && \
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -trimpath -o /out/yarilo-imap-login-backend       ./app/yarilo-imap-login-backend       && \
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -trimpath -o /out/yarilo-pop3-login-backend       ./app/yarilo-pop3-login-backend       && \
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -trimpath -o /out/yarilo-submission-login-backend ./app/yarilo-submission-login-backend && \
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -trimpath -o /out/yarilo-migrate                 ./app/yarilo-migrate

FROM alpine:3.23
# hadolint ignore=DL3018
RUN apk add --no-cache ca-certificates tzdata

COPY --from=builder /out/ /usr/local/bin/
COPY docker/entrypoint.sh /entrypoint.sh
COPY config/yarilo.yaml /etc/yarilo/yarilo.yaml

RUN chmod +x /entrypoint.sh

EXPOSE 143 993 465 587 8080

ENTRYPOINT ["/entrypoint.sh"]
