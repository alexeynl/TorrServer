package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"server/mkvsubs"
	"server/subs"
	"server/torr"
)

// Common code of the /subs handlers that read a Matroska file of a torrent: the embedded subtitles (?embedded)
// and the fonts attached to the file (?font).

// isMatroska reports whether the file is one that mkvsubs reads.
func isMatroska(name string) bool {
	switch strings.ToLower(path.Ext(name)) {
	case ".mkv", ".mk3d", ".mka", ".webm":
		return true
	}
	return false
}

// openMatroska opens the video file of the torrent as a Matroska file. release must be called when done with the
// file; a blocked read of the torrent is interrupted by the end of ctx.
func openMatroska(ctx context.Context, tor *torr.Torrent, video subs.FileInfo) (*mkvsubs.File, func(), error) {
	rd, err := tor.OpenFileByPath(video.Path)
	if err != nil {
		return nil, nil, err
	}
	// A blocked read of the torrent is interrupted by closing the reader.
	var once sync.Once
	closeReader := func() { once.Do(func() { rd.Close() }) }
	stop := context.AfterFunc(ctx, closeReader)
	release := func() {
		stop()
		closeReader()
	}

	// Sanity check: the reader must be the file we asked for (a wrong id -> file mapping in OpenFile would give
	// "not Matroska" or garbage).
	end, err := rd.Seek(0, io.SeekEnd)
	if err != nil {
		release()
		return nil, nil, err
	}
	if video.Size > 0 && end != video.Size {
		release()
		return nil, nil, fmt.Errorf("OpenFileByPath(%q) returned a file of %d bytes, expected %d: wrong file?", video.Path, end, video.Size)
	}
	if _, err := rd.Seek(0, io.SeekStart); err != nil {
		release()
		return nil, nil, err
	}

	f, err := mkvsubs.Open(rd, video.Size)
	if err != nil {
		release()
		return nil, nil, err
	}
	return f, release, nil
}

// mkvRequest logs and answers the failures of such a handler. The message goes to the body too, so a test page can
// show it.
type mkvRequest struct {
	c       *gin.Context
	ctx     context.Context
	what    string // for the log: which file and query
	started time.Time
}

func (r mkvRequest) reply(code int, err error) {
	log.Printf("subs: %s: %d after %s: %v", r.what, code, time.Since(r.started).Round(time.Millisecond), err)
	r.c.Error(err)
	r.c.String(code, "%s", err.Error())
	r.c.Abort()
}

// fail picks the status by the error: a timeout of the request, a file that is not Matroska, a track that is not
// there, a format that is not supported.
func (r mkvRequest) fail(err error) {
	switch {
	case r.ctx.Err() != nil:
		r.reply(http.StatusGatewayTimeout, r.ctx.Err())
	case errors.Is(err, mkvsubs.ErrNotMatroska), errors.Is(err, mkvsubs.ErrNoTracks):
		r.reply(http.StatusUnprocessableEntity, err)
	case errors.Is(err, mkvsubs.ErrNoTrack):
		r.reply(http.StatusNotFound, err)
	case errors.Is(err, mkvsubs.ErrUnsupportedFormat):
		r.reply(http.StatusUnsupportedMediaType, err)
	default:
		r.reply(http.StatusInternalServerError, err)
	}
}
