FROM golang:1.25.8-alpine AS build

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
