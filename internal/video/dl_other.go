//go:build !(darwin || freebsd || linux || netbsd || windows)

package video

import "errors"

func openLibrary(string) (uintptr, error) {
	return 0, errors.New("video: loading FFmpeg is not supported on this platform")
}

func lookupSymbol(uintptr, string) (uintptr, error) {
	return 0, errors.New("video: not supported on this platform")
}
