FROM golang:1.22-alpine

# Outils système nécessaires
RUN apk add --no-cache git gcc musl-dev

WORKDIR /app

# Copie juste go.mod
COPY go.mod ./

# Récupération automatique de la dernière version valide directement sur le serveur
RUN go get go.mau.fi/whatsmeow@latest && \
    go get github.com/glebarez/go-sqlite@latest && \
    go get github.com/skip2/go-qrcode@latest && \
    go get google.golang.org/api/option@latest && \
    go get google.golang.org/api/sheets/v4@latest && \
    go mod tidy

COPY . .

ENV CGO_ENABLED=1

RUN go build -o main .

EXPOSE 8080

CMD ["./main"]
