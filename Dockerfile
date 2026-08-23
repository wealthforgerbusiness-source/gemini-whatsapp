FROM golang:1.26-alpine

WORKDIR /app

RUN apk add --no-cache \
    gcc \
    musl-dev \
    git

ENV CGO_ENABLED=1

COPY go.mod go.sum ./

RUN go mod download

COPY . .

RUN go build -o app .

EXPOSE 8080

CMD ["./app"]
