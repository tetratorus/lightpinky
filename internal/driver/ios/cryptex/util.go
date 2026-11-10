// Copied from github.com/danielpaulus/go-ios v1.3.3-0.20261103131045-6910f3b585ad: getFileSize from
// ios/imagemounter/personalized_image_mounter.go and logModule from
// ios/imagemounter/imagemounter.go, under go-ios's MIT License (LICENSE in
// this folder).

package cryptex

import (
	"fmt"
	"os"
)

const logModule = "go-ios/imagemounter"

func getFileSize(p string) (uint64, error) {
	info, err := os.Stat(p)
	if err != nil {
		return 0, fmt.Errorf("getFileSize: could not get file stats for '%s': %w", p, err)
	}
	if info.IsDir() {
		return 0, fmt.Errorf("getFileSize: expected a file, but got a directory: '%s'", p)
	}
	return uint64(info.Size()), nil
}
