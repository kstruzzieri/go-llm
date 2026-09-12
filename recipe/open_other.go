//go:build !unix

package recipe

import "os"

func openRecipeFile(path string) (*os.File, error) {
	return os.Open(path)
}
