FROM golang:1.26-alpine AS builder

ARG BUILD_VERSION=dev

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
ENV CGO_ENABLED=0 GOOS=linux GOARCH=amd64
RUN set -eu; \
    for cmd in \
        yarilo \
        yarilo-auth \
        yarilo-anvil \
        yarilo-admin \
        yarilo-backend-api \
        yarilo-director \
        yarilo-imap \
        yarilo-imap-login \
        yarilo-pop3 \
        yarilo-pop3-login \
        yarilo-lmtp \
        yarilo-submission \
        yarilo-submission-login \
        yarilo-locks \
        yarilo-migrate \
        yarilo-quota-status \
    ; do \
        go build -ldflags="-s -w -X github.com/0kaba0hub/yarilo/pkg/build.Version=${BUILD_VERSION}" \
            -trimpath -o "/out/$cmd" "./app/$cmd"; \
    done

FROM alpine:3.24
# hadolint ignore=DL3018,DL3019
RUN apk upgrade --no-cache && apk add --no-cache ca-certificates tzdata

COPY --from=builder /out/ /usr/local/bin/
COPY docker/entrypoint.sh /entrypoint.sh
COPY config/yarilo.yaml /etc/yarilo/yarilo.yaml

RUN chmod +x /entrypoint.sh

EXPOSE 143 993 465 587 8080

ENTRYPOINT ["/entrypoint.sh"]
