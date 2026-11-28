//go:build !windows

package seamless

import "os"

func readPlatformSystemRoots() []byte {
	candidates := []string{
		"/etc/ssl/cert.pem",                  // macOS, Alpine, OpenBSD
		"/etc/ssl/certs/ca-certificates.crt", // Debian, Ubuntu, Gentoo, Arch
		"/etc/pki/tls/certs/ca-bundle.crt",   // RHEL, Fedora, CentOS
		"/etc/ssl/ca-bundle.pem",             // openSUSE
		"/etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem",
	}
	for _, p := range candidates {
		if data, err := os.ReadFile(p); err == nil && len(data) > 0 {
			return data
		}
	}
	return nil
}
