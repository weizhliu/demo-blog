# Build a single static binary, then ship only that.
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /demo-blog .

# distroless/static: CA certificates (to reach Lift over HTTPS) and nothing else.
FROM gcr.io/distroless/static-debian12
COPY --from=build /demo-blog /demo-blog
EXPOSE 8080
ENTRYPOINT ["/demo-blog"]
