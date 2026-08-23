FROM golang:1.22-alpine

# Outils système indispensables (Git pour rapatrier le dépôt whatsmeow + GCC pour SQLite)
RUN apk add --no-cache git gcc musl-dev

WORKDIR /app

COPY go.mod go.sum ./

# Télécharge et synchronise automatiquement la bonne révision depuis GitHub
RUN go get go.mau.fi/whatsmeow@latest && go mod tidy

COPY . .

ENV CGO_ENABLED=1

RUN go build -o main .

EXPOSE 8080

CMD ["./main"]
