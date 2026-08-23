FROM golang:1.26-alpine

WORKDIR /app

RUN apk add --no-cache \
    gcc \
    musl-dev \
    git

ENV CGO_ENABLED=1

COPY go.mod ./

# Génère les dépendances ET le go.sum
RUN go mod tidy

COPY . .

# Vérification finale des dépendances
RUN go mod tidy

# Compilation
RUN go build -o app .

EXPOSE 8080

CMD ["./app"]
