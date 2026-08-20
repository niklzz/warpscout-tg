#!/bin/sh
# Build a darwin/amd64 binary that runs on macOS 10.13 High Sierra.
#
# Stock Go 1.26 binaries abort at launch there: crypto/x509 binds
# SecTrustEvaluateWithError (10.14+) and SecTrustCopyCertificateChain (12.0+)
# from Security.framework eagerly, so dyld kills the process before main.
# Fix: patch GOROOT inside the build container so x509 verifies in pure Go
# against a compiled-in Mozilla root bundle, then relabel the Mach-O minos.
#
# Needs Docker (OrbStack) and, for the relabel step, macOS with vtool.
set -e

VERSION="${1:-dev}"
OUT="warpscout-darwin-amd64-highsierra"
DIR="$(cd "$(dirname "$0")" && pwd)"

cat > "$DIR/.hs-root_darwin.go" <<'EOF'
package x509

import (
	_ "embed"
	"os"
)

//go:embed root_darwin_bundle.pem
var embeddedRoots []byte

func (c *Certificate) systemVerify(opts *VerifyOptions) (chains [][]*Certificate, err error) {
	return nil, nil
}

func loadSystemRoots() (*CertPool, error) {
	roots := NewCertPool()
	if f := os.Getenv("SSL_CERT_FILE"); f != "" {
		if data, err := os.ReadFile(f); err == nil && roots.AppendCertsFromPEM(data) {
			return roots, nil
		}
	}
	roots.AppendCertsFromPEM(embeddedRoots)
	return roots, nil
}
EOF

docker run --rm \
	-v "$DIR":/src -w /src \
	-v "$HOME/.cache/warpscout-go":/root/.cache/go-build \
	-v "$HOME/go/pkg/mod":/go/pkg/mod \
	-e GOOS=darwin -e GOARCH=amd64 -e CGO_ENABLED=0 \
	golang:1.26-alpine sh -c "
		set -e
		apk add --no-cache ca-certificates >/dev/null
		X=/usr/local/go/src/crypto/x509
		cp /src/.hs-root_darwin.go \$X/root_darwin.go
		cp /etc/ssl/certs/ca-certificates.crt \$X/root_darwin_bundle.pem
		go build -trimpath -ldflags='-s -w -X main.version=$VERSION' -o /src/$OUT .
	"
rm -f "$DIR/.hs-root_darwin.go"

# Go stamps minos 12.0; High Sierra's dyld should not see a newer floor.
if command -v vtool >/dev/null 2>&1; then
	vtool -set-build-version macos 10.13 10.13 -output "$DIR/$OUT.tmp" "$DIR/$OUT"
	mv "$DIR/$OUT.tmp" "$DIR/$OUT"
fi

echo "built $OUT ($VERSION)"
