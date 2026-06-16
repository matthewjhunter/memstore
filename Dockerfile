# Float the 1.26.x patch so the image always satisfies go.mod's `go` directive
# (Alpine sets GOTOOLCHAIN=local, so a pinned patch behind go.mod fails the
# build). Dependabot does not manage Docker here, so a pinned tag would go
# stale on every go-directive bump.
FROM golang:1.26-alpine AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /memstored ./cmd/memstored

FROM alpine:3.21
RUN apk add --no-cache ca-certificates
COPY --from=build /memstored /usr/local/bin/memstored
EXPOSE 8230
ENTRYPOINT ["memstored"]
