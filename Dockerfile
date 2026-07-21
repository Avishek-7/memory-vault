FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY main.go ./
RUN CGO_ENABLED=0 go build -o /memory-vault .

FROM scratch
COPY --from=build /memory-vault /memory-vault
EXPOSE 8080
ENTRYPOINT ["/memory-vault"]
