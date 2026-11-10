//go:build darwin || freebsd || linux || netbsd

package video

import "github.com/ebitengine/purego"

func openLibrary(path string) (uintptr, error) {
	return purego.Dlopen(path, purego.RTLD_NOW|purego.RTLD_GLOBAL)
}

func lookupSymbol(h uintptr, name string) (uintptr, error) {
	return purego.Dlsym(h, name)
}
