FROM golang:1.12-alpine3.9 as builder
RUN apk update && apk add git glide build-base
ENV GOPATH $HOME/go
ADD . ${GOPATH}/src/github.com/aergoio/aergo-indexer
WORKDIR ${GOPATH}/src/github.com/aergoio/aergo-indexer
RUN make all

FROM alpine:3.9
RUN apk add libgcc
COPY --from=builder $HOME/go/src/github.com/aergoio/aergo-indexer/bin/* /usr/local/bin/
CMD ["esindexer"]