FROM golang:1.22-alpine

# Installation de git, gcc et musl-dev indispensables pour Whatsmeow et SQLite
RUN apk add --no-cache git gcc musl-dev

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Activation de CGO requis pour SQLite
ENV CGO_ENABLED=1

RUN go build -o main .

EXPOSE 8080

CMD ["./main"]
