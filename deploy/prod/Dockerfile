FROM golang:1.14-alpine AS build
RUN mkdir /ingress-mdns/
WORKDIR /ingress-mdns
COPY . .
RUN go build -o ingress-mdns

FROM golang:1.14-alpine
COPY --from=build /ingress-mdns/ingress-mdns /ingress-mdns
ENTRYPOINT ["/ingress-mdns"]
