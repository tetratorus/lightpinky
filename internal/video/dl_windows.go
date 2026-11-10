//go:build windows

package video

import (
	"path/filepath"

	"golang.org/x/sys/windows"
)

// openLibrary loads a DLL. A DLL given by its full path finds the DLLs it
// imports in its own folder first (LOAD_WITH_ALTERED_SEARCH_PATH), so
// FFmpeg's DLLs work from a folder of their own, such as PINKY_FFMPEG_DIR.
func openLibrary(path string) (uintptr, error) {
	var flags uintptr
	if filepath.IsAbs(path) {
		flags = windows.LOAD_WITH_ALTERED_SEARCH_PATH
	}
	h, err := windows.LoadLibraryEx(path, 0, flags)
	return uintptr(h), err
}

func lookupSymbol(h uintptr, name string) (uintptr, error) {
	return windows.GetProcAddress(windows.Handle(h), name)
}
