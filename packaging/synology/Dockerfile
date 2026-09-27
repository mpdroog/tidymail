# tidymail for a Synology NAS (Container Manager), x86-64.
# The binary is built on a Mac with: packaging/synology/build.sh
FROM alpine:3.20
# CA certificates for TLS to the IMAP and SMTP servers; tzdata for local dates.
RUN apk add --no-cache ca-certificates tzdata
ENV TZ=Europe/Amsterdam
COPY tidymail /usr/local/bin/tidymail
WORKDIR /data
