FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod main.go ./
RUN CGO_ENABLED=0 go build -o /simple-api-proxy .

FROM alpine:3.20
RUN apk add --no-cache ca-certificates
COPY --from=build /simple-api-proxy /usr/local/bin/simple-api-proxy
EXPOSE 4000
ENTRYPOINT ["simple-api-proxy"]
CMD ["serve", "-port", "4000", "-keys", "/etc/config/keys.json", "-apikey", "/etc/secrets/key.dat"]
