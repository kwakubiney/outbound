FROM golang:1.26-alpine AS builder

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o edge ./cmd/edge/main.go

FROM scratch

COPY --from=builder /app/edge /edge

ENV OUTBOUND_AUTH_SECRET=""

EXPOSE 8080

ENTRYPOINT ["/edge"]
CMD ["--addr", ":8080"]
