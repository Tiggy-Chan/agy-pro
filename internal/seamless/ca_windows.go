//go:build windows

package seamless

import (
	"bytes"
	"encoding/pem"
	"unsafe"

	"golang.org/x/sys/windows"
)

// readPlatformSystemRoots exports trusted Root CAs from the Windows CryptoAPI "ROOT" store
// into PEM format so the child process's temporary SSL_CERT_FILE bundle trusts both public
// HTTPS endpoints and our ephemeral per-session loopback CA without modifying the OS store.
func readPlatformSystemRoots() []byte {
	storeName, err := windows.UTF16PtrFromString("ROOT")
	if err != nil {
		return nil
	}
	store, err := windows.CertOpenSystemStore(0, storeName)
	if err != nil {
		return nil
	}
	defer windows.CertCloseStore(store, 0)

	var buf bytes.Buffer
	var prev *windows.CertContext
	for {
		cert, err := windows.CertEnumCertificatesInStore(store, prev)
		if err != nil || cert == nil {
			break
		}
		if cert.EncodedCert != nil && cert.Length > 0 {
			der := unsafe.Slice(cert.EncodedCert, int(cert.Length))
			_ = pem.Encode(&buf, &pem.Block{Type: "CERTIFICATE", Bytes: der})
		}
		prev = cert
	}
	return buf.Bytes()
}
