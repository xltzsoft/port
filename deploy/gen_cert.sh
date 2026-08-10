#!/bin/sh
# 生成自签名 TLS 证书(生产建议使用受信任 CA 或 ACME)。
# 用法: sh deploy/gen_cert.sh [输出目录] [CN]
# 输出: <目录>/server.crt 与 <目录>/server.key
set -e

DIR="${1:-/etc/port}"
CN="${2:-port-server}"

mkdir -p "$DIR"
openssl req -x509 -newkey rsa:2048 -nodes -days 3650 \
  -keyout "$DIR/server.key" -out "$DIR/server.crt" \
  -subj "/CN=$CN" \
  -addext "subjectAltName=DNS:$CN,DNS:localhost,IP:127.0.0.1"

chmod 600 "$DIR/server.key"

echo "生成完成:"
echo "  cert: $DIR/server.crt"
echo "  key:  $DIR/server.key"
echo
echo "客户端指纹(填入 client.yaml 的 tls.server_fingerprint):"
openssl x509 -in "$DIR/server.crt" -noout -fingerprint -sha256
