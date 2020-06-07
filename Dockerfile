FROM golang:alpine as builder
RUN mkdir /build
ADD . /build/
WORKDIR /build 
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-w -s" -o main .

FROM gcr.io/distroless/static
COPY --from=builder /build/main /app/
WORKDIR /app
CMD ["./main"]
EXPOSE 9090