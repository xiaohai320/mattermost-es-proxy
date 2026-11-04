FROM nexus.liangkui.co/docker-group/library/golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod main.go ./
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /mm-es-substring-proxy .

FROM harbor.liangkui.co/3rd_party/docker.io/library/alpine:3.19.8
COPY --from=build /mm-es-substring-proxy /usr/local/bin/mm-es-substring-proxy
EXPOSE 9210
ENTRYPOINT ["mm-es-substring-proxy"]
