FROM golang:1.25-alpine

WORKDIR /app

RUN apk add --no-cache git gcc musl-dev

COPY go.mod go.sum ./

RUN go mod download

COPY . .

RUN go build -o app .

CMD ["./app"]
