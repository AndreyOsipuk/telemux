# Сборка telemux (multi-stage).
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -ldflags "-s -w -X main.version=${VERSION}" -o /telemux ./cmd/telemux

FROM alpine:3.20
RUN apk add --no-cache ca-certificates && adduser -D -u 10001 telemux
COPY --from=build /telemux /usr/local/bin/telemux
USER telemux
ENTRYPOINT ["telemux"]
CMD ["serve"]
