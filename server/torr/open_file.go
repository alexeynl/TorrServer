package torr

import (
	"errors"
	"io"
	"strings"

	"server/torr/storage/torrstor"
)

// fileReader is a reader of a torrent file that closes itself through the torrent (CloseReader).
type fileReader struct {
	*torrstor.Reader
	t *Torrent
}

func (r *fileReader) Close() error {
	r.t.CloseReader(r.Reader)
	return nil
}

// OpenFileByPath returns a reader of the file with the given path inside the torrent
// (FileStat.Path; "/" and "\" are treated the same). The /play id is NOT an index in Files().
func (t *Torrent) OpenFileByPath(path string) (io.ReadSeekCloser, error) {
	if !t.GotInfo() {
		return nil, errors.New("torrent info is not ready")
	}
	want := strings.ReplaceAll(path, "\\", "/")
	for _, f := range t.Files() {
		if strings.ReplaceAll(f.Path(), "\\", "/") != want {
			continue
		}
		r := t.NewReader(f)
		if r == nil {
			return nil, errors.New("torrent is closed")
		}
		return &fileReader{Reader: r, t: t}, nil
	}
	return nil, errors.New("file not found in the torrent: " + path)
}
