FROM golang:alpine
ENV GO111MODULE=on
RUN apk add --no-cache git bash zsh
RUN go get -v golang.org/x/tools/cmd/goimports
RUN go get -v github.com/stamblerre/gocode
RUN go get -v github.com/uudashr/gopkgs/v2/cmd/gopkgs
RUN go get -v golang.org/x/tools/gopls
WORKDIR /go/src/github.com/secoya/ingress-mdns
