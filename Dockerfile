FROM golang:alpine AS builder
RUN apk add --no-cache git gcc libc-dev
WORKDIR /go/src/github.com/andsens/ingress-frontend-zeroconf
ENV GO111MODULE=on
COPY go.mod .
COPY go.sum .
RUN go mod download

COPY ingress-frontend-zeroconf.go .
RUN go install .

FROM golang:alpine
COPY --from=builder /go/bin/ingress-frontend-zeroconf /bin/ingress-frontend-zeroconf
ENTRYPOINT ["/bin/ingress-frontend-zeroconf"]
