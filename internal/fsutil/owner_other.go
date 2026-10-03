//go:build !unix

package fsutil

import (
	"io/fs"
	"os"
)

// keepOwner does nothing where files have no Unix owner.
func keepOwner(*os.File, fs.FileInfo) {}
