#!/bin/sh
# Generates a test CA and a vmauth certificate valid only for the vmalert.<site>.test hosts.
set -eu
cd /certs
openssl req -x509 -newkey rsa:2048 -nodes -days 1 -subj "/CN=vmalertsproxy test CA" \
  -keyout ca.key -out ca.crt 2>/dev/null
openssl req -newkey rsa:2048 -nodes -subj "/CN=vmalert.test" -keyout server.key -out server.csr 2>/dev/null
printf 'subjectAltName=DNS:vmalert.msk.test,DNS:vmalert.spb.test\n' >san.ext
openssl x509 -req -days 1 -in server.csr -CA ca.crt -CAkey ca.key -CAcreateserial \
  -extfile san.ext -out server.crt 2>/dev/null
# vmauth and vmalertsproxy may run as non-root users.
chmod 644 ca.crt server.crt server.key
echo "certificates generated"
