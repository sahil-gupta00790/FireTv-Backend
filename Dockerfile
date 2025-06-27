FROM golang:1.22-alpine3.19 AS builder

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o fire-tv-rooms cmd/server/main.go

FROM alpine:3.19
RUN apk --no-cache add ca-certificates
WORKDIR /root/
COPY --from=builder /app/fire-tv-rooms .

EXPOSE 8080
CMD ["./fire-tv-rooms"]
