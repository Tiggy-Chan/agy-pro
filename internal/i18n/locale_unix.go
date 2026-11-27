//go:build !windows

package i18n

func osLocaleFallback() string {
	return ""
}
