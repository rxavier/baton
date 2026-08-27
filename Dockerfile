# ---- build stage ----
FROM golang:1.27-alpine AS build
WORKDIR /src
RUN apk add --no-cache ca-certificates
COPY go.mod ./
COPY *.go index.html ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/baton .

# ---- runtime stage ----
FROM scratch
# CA bundle so HTTPS Navidrome URLs (e.g. behind a reverse proxy) verify
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/baton /baton
EXPOSE 8080
ENTRYPOINT ["/baton"]