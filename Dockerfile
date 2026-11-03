FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod main.go ./
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /mm-es-substring-proxy .

FROM alpine:3.19
COPY --from=build /mm-es-substring-proxy /usr/local/bin/mm-es-substring-proxy
EXPOSE 9210
ENTRYPOINT ["mm-es-substring-proxy"]
