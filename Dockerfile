# Build
FROM golang:1.24-alpine AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/sipphone .

# Run
FROM alpine:3.21
RUN adduser -D -u 10001 sipphone
USER sipphone
COPY --from=build /out/sipphone /usr/local/bin/sipphone
ENTRYPOINT ["sipphone"]
