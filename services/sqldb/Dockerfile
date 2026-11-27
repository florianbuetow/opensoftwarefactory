FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /sqldb . && mkdir /state && chown 1000:1000 /state

FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /sqldb /sqldb
COPY --from=build --chown=1000:1000 /state /state
USER 1000:1000
EXPOSE 8080
ENTRYPOINT ["/sqldb"]
