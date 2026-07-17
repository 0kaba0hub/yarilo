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
        yarilo-lmtp-login \
        yarilo-submission \
        yarilo-submission-login \
        yarilo-locks \
        yarilo-migrate \
        yarilo-quota-status \
        yarilo-sasl-login \
        yarilo-managesieve \
        yarilo-managesieve-login \
    ; do \
        go build -ldflags="-s -w -X github.com/0kaba0hub/yarilo/pkg/build.Version=${BUILD_VERSION}" \
            -trimpath -o "/out/$cmd" "./app/$cmd"; \
    done
RUN go build -ldflags="-s -w -X github.com/0kaba0hub/yarilo/pkg/build.Version=${BUILD_VERSION}" \
    -trimpath -o /out/yarilo-smoketest ./app/smoketest

## --- yarilo-fts: the only cgo binary (libxapian). Built in its own stage so
## every other binary stays CGO_ENABLED=0 static; ships in the single image.
FROM golang:1.26-alpine AS fts-builder
# hadolint ignore=DL3018
RUN apk add --no-cache build-base xapian-core-dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG BUILD_VERSION=dev
ENV CGO_ENABLED=1 GOOS=linux GOARCH=amd64
RUN for cmd in yarilo-fts fts-bench; do \
        go build -tags flatcurve -ldflags="-s -w -X github.com/0kaba0hub/yarilo/pkg/build.Version=${BUILD_VERSION}" \
            -trimpath -o "/out/$cmd" "./app/$cmd"; \
    done

FROM alpine:3.24
# hadolint ignore=DL3018,DL3019
RUN apk upgrade --no-cache && apk add --no-cache ca-certificates tzdata xapian-core libstdc++ && \
    addgroup -g 1000 yarilo && \
    adduser -u 1000 -G yarilo -s /sbin/nologin -D yarilo

COPY --from=builder /out/ /usr/local/bin/
COPY --from=fts-builder /out/yarilo-fts /usr/local/bin/yarilo-fts
COPY --from=fts-builder /out/fts-bench /usr/local/bin/fts-bench
COPY docker/entrypoint.sh /entrypoint.sh
COPY config/yarilo.yaml /etc/yarilo/yarilo.yaml

RUN chmod +x /entrypoint.sh

USER yarilo:yarilo

EXPOSE 143 993 465 587 8080

ENTRYPOINT ["/entrypoint.sh"]
