FROM golang:1.25-alpine

WORKDIR /app

RUN apk add --no-cache \
    gcc \
    musl-dev \
    git

ENV CGO_ENABLED=1

COPY go.mod ./

RUN go mod tidy
RUN go mod download

COPY . .

RUN go build -o app .

EXPOSE 8080

CMD ["./app"]
