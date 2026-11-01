FROM golang:1.27.3-alpine3.24 AS build
WORKDIR /src
COPY go.mod main.go ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /gateway .
FROM alpine:3.24.3
RUN apk add --no-cache ca-certificates
WORKDIR /app
COPY --from=build /gateway ./gateway
USER 1000:1000
ENTRYPOINT ["./gateway"]
