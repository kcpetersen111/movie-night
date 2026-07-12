FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /movie-night .

FROM alpine:3.20
RUN adduser -D app
USER app
COPY --from=build /movie-night /usr/local/bin/movie-night
EXPOSE 8080
CMD ["movie-night"]
