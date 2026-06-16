# --- Étape de build -----------------------------------------------------------
FROM golang:1.26-alpine AS build

WORKDIR /src
COPY go.mod ./
COPY main.go ./

# Binaire statique (pas de CGO) pour pouvoir tourner sur une image minimale.
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /yt-transcript .

# --- Image finale -------------------------------------------------------------
FROM alpine:3.20

# Certificats racine indispensables pour les appels HTTPS vers YouTube.
RUN apk add --no-cache ca-certificates && adduser -D -u 10001 app

COPY --from=build /yt-transcript /usr/local/bin/yt-transcript

USER app
ENV PORT=8080
EXPOSE 8080

ENTRYPOINT ["/usr/local/bin/yt-transcript"]
CMD ["serve"]
