# 1st stage, build app
FROM golang:1.19 as builder
RUN apt-get update && apt-get -y upgrade && apt-get install -y upx
COPY . /build/app
WORKDIR /build/app

RUN go get ./... && go build -ldflags "-s -w" -trimpath -o bundles cmd/bundles/main.go
RUN upx bundles && upx -t bundles

# 2nd stage, create a user to copy, and install libraries needed if connecting to upstream TLS server
# we don't want the /lib and /lib64 from the go container cause it has more than we need.
FROM debian:10 AS ssl
ENV DEBIAN_FRONTEND noninteractive
RUN apt-get update && apt-get -y upgrade && apt-get install -y ca-certificates && \
    addgroup --gid 9876 --system bundles && adduser -uid 9876 --ingroup bundles --system --home /var/lib/bundles bundles

# 3rd and final stage, copy the minimum parts into a scratch container, is a smaller and more secure build. This pulls
# in SSL libraries and CAs so Go can connect to TLS servers.
FROM scratch
COPY --from=ssl /etc/ca-certificates /etc/ca-certificates
COPY --from=ssl /etc/ssl /etc/ssl
COPY --from=ssl /usr/share/ca-certificates /usr/share/ca-certificates
COPY --from=ssl /usr/lib /usr/lib
COPY --from=ssl /lib /lib
COPY --from=ssl /lib64 /lib64

COPY --from=ssl /etc/passwd /etc/passwd
COPY --from=ssl /etc/group /etc/group
COPY --from=ssl --chown=bundles:bundles /var/lib/bundles /var/lib/bundles

COPY --from=builder /build/app/bundles /bin/bundles

USER bundles
WORKDIR /var/lib/bundles

ENTRYPOINT ["/bin/bundles"]
