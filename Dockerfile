# ---- build stage ----
FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY *.go index.html ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/baton .

# ---- runtime stage ----
FROM scratch
COPY --from=build /out/baton /baton
EXPOSE 8080
ENTRYPOINT ["/baton"]