package ioutil

import (
	"io"

	"github.com/go-git/go-git/v6/utils/sync"
)

// Copy calls io.CopyBuffer and uses a buffer from sync.GetByteSlice,
// to reduce the complexity when using it while avoiding the allocation
// of a new buffer per call.
func Copy(dst io.Writer, src io.Reader) (written int64, err error) {
	buf := sync.GetByteSlice()
	defer sync.PutByteSlice(buf)

	return io.CopyBuffer(dst, src, *buf)
}
