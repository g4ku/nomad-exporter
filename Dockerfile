FROM golang:alpine AS builder
RUN mkdir /build
ADD . /build/
WORKDIR /build

RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -ldflags '-extldflags "-static"' .

FROM alpine:3.9

RUN apk --no-cache add ca-certificates

EXPOSE 9441

COPY --from=builder /build/nomad-exporter /
 
ENTRYPOINT [ "/nomad-exporter" ]
