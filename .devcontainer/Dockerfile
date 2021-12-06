FROM golang:1.17-alpine
RUN apk add --no-cache git bash zsh g++
RUN go get -v golang.org/x/tools/cmd/goimports
RUN go get -v github.com/stamblerre/gocode
RUN go get -v github.com/uudashr/gopkgs/v2/cmd/gopkgs
RUN go get -v golang.org/x/tools/gopls
RUN go get -v github.com/ramya-rao-a/go-outline
WORKDIR /go/src/github.com/secoya/ingress-mdns
