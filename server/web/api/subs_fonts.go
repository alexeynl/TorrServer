package api

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"server/mkvsubs"
	"server/subs"
	"server/torr"
)

// Fonts for the ASS subtitles of a video file, in one list for the player:
//
//	GET /subs/{hash}/{id}?fonts    - JSON [{source, name, type, size, url}]
//	GET /subs/{hash}/{id}?font=N   - the bytes of the font attached to the video file (see below)
//
// source "external": a font file of the torrent, url is a /stream link of that file;
// source "embedded": a font attached to a Matroska file, url is /subs/{hash}/{id}?font=N.
// "name" always has a font extension (.ttf, .otf, .ttc), the player recognizes font files by it.
//
// The player does not care where a font comes from: it reads the name table of every font by Range requests to
// "url" and hands the fonts that the subtitles use to the renderer.
//
// External fonts: only the fonts that belong to the video are listed. These are the font files in the folder of
// the video or below it; if there are none, those in the folder above (with its subfolders), and so on up to the
// root of the torrent.
//
// Embedded fonts (the file is opened with the helpers of subs_mkv.go): the fonts attached to an mkv file (Attachments) cannot be read by a client through /stream: it
// would have to parse the container. The server already does, so it serves them. Range requests work, so a client
// can read only the name table of a font, as it does for the font files of a torrent. Listing costs only the
// headers of the file (the fonts are skipped with Seek), reading a font costs what is read. If the attachments
// cannot be read, the list has the external fonts only.
const (
	fontsTimeout = 60 * time.Second // opening the file and the list of the attachments
	fontTimeout  = 5 * time.Minute  // the bytes of a font from a torrent that is still downloading
)

type fontJS struct {
	Source string `json:"source"`
	Name   string `json:"name"`
	Type   string `json:"type"`
	Size   int64  `json:"size"`
	URL    string `json:"url"`
}

// torrentFile is a file of the torrent as /stream knows it: ID is the "index" of the /stream and /play links.
type torrentFile struct {
	ID   int
	Path string
	Size int64
}

// Same formats as the client reads (WOFF and WOFF2 cannot be read by Range requests).
func isFontFileName(name string) bool {
	switch strings.ToLower(path.Ext(name)) {
	case ".ttf", ".otf", ".ttc":
		return true
	}
	return false
}

func fontContentType(name string) string {
	switch strings.ToLower(path.Ext(name)) {
	case ".otf":
		return "font/otf"
	case ".ttc":
		return "font/collection"
	}
	return "font/ttf"
}

// nearestFonts returns the font files that belong to the video (see above), sorted by path.
func nearestFonts(videoPath string, files []torrentFile) []torrentFile {
	norm := func(p string) string { return strings.ReplaceAll(p, "\\", "/") }

	var fonts []torrentFile
	for _, f := range files {
		if isFontFileName(f.Path) {
			f.Path = norm(f.Path)
			fonts = append(fonts, f)
		}
	}
	sort.Slice(fonts, func(i, j int) bool { return fonts[i].Path < fonts[j].Path })

	dir := path.Dir(norm(videoPath))
	for {
		root := dir == "." || dir == "/"
		var found []torrentFile
		for _, f := range fonts {
			if root || strings.HasPrefix(f.Path, dir+"/") {
				found = append(found, f)
			}
		}
		if len(found) > 0 || root {
			return found
		}
		dir = path.Dir(dir)
	}
}

// externalFontURL is the same link that the web interface builds for the files of a torrent.
func externalFontURL(hash string, f torrentFile) string {
	return "/stream/" + url.PathEscape(path.Base(f.Path)) + "?link=" + hash + "&index=" + strconv.Itoa(f.ID) + "&play"
}

// fontFileName is the name of an attachment with a font extension: an attachment may have none, and the player
// recognizes font files by the extension.
func fontFileName(a mkvsubs.Attachment) string {
	if isFontFileName(a.Name) {
		return a.Name
	}
	return a.Name + ".ttf"
}

// subsFonts answers ?fonts and ?font=N of GET /subs/{hash}/{id}. files is the list of the files of the torrent that
// subsTracks already has (Index is the id of /stream and /play links, Path uses "/"); video is the requested video.
func subsFonts(c *gin.Context, tor *torr.Torrent, hash string, videoID int, video subs.FileInfo, files []subs.FileInfo) {
	if v, ok := c.GetQuery("font"); ok {
		embeddedFont(c, tor, hash, videoID, video, v)
		return
	}

	candidates := make([]torrentFile, 0, len(files))
	for _, f := range files {
		candidates = append(candidates, torrentFile{ID: f.Index, Path: f.Path, Size: f.Size})
	}

	out := make([]fontJS, 0)
	for _, f := range nearestFonts(video.Path, candidates) {
		out = append(out, fontJS{
			Source: "external",
			Name:   path.Base(f.Path),
			Type:   fontContentType(f.Path),
			Size:   f.Size,
			URL:    externalFontURL(hash, f),
		})
	}
	if isMatroska(video.Path) {
		out = append(out, embeddedFontList(c.Request.Context(), tor, hash, videoID, video)...)
	}
	c.JSON(http.StatusOK, out)
}

// embeddedFontList lists the fonts attached to a Matroska file. Problems are logged and give an empty list: the
// external fonts must still be served.
func embeddedFontList(ctx context.Context, tor *torr.Torrent, hash string, videoID int, video subs.FileInfo) []fontJS {
	ctx, cancel := context.WithTimeout(ctx, fontsTimeout)
	defer cancel()

	f, release, err := openMatroska(ctx, tor, video)
	if err != nil {
		log.Printf("subs: attached fonts of file %d (%s): %v", videoID, video.Path, err)
		return nil
	}
	defer release()
	list, err := f.Attachments()
	if err != nil {
		log.Printf("subs: attached fonts of file %d (%s): %v", videoID, video.Path, err)
		return nil
	}

	base := "/subs/" + hash + "/" + strconv.Itoa(videoID) + "?font="
	var out []fontJS
	for _, a := range list {
		if !a.IsFont() {
			continue
		}
		name := fontFileName(a)
		out = append(out, fontJS{
			Source: "embedded",
			Name:   name,
			Type:   fontContentType(name),
			Size:   a.Size,
			URL:    base + strconv.Itoa(a.Index),
		})
	}
	return out
}

// embeddedFont serves the bytes of one attached font (?font=N, N is "index" of the list of the attachments).
func embeddedFont(c *gin.Context, tor *torr.Torrent, hash string, videoID int, video subs.FileInfo, value string) {
	if !isMatroska(video.Path) {
		c.AbortWithError(http.StatusBadRequest, errors.New("fonts are attached to Matroska files only"))
		return
	}
	n, err := strconv.Atoi(value)
	if err != nil || n < 0 {
		c.AbortWithError(http.StatusBadRequest, errors.New("\"font\" must be the index of an attachment"))
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), fontTimeout)
	defer cancel()

	started := time.Now()
	req := mkvRequest{
		c:       c,
		ctx:     ctx,
		what:    fmt.Sprintf("font %d of file %d (%s)", n, videoID, video.Path),
		started: started,
	}
	f, release, err := openMatroska(ctx, tor, video)
	if err != nil {
		req.fail(err)
		return
	}
	defer release()
	list, err := f.Attachments()
	if err != nil {
		req.fail(err)
		return
	}
	if n >= len(list) || !list[n].IsFont() {
		req.fail(mkvsubs.ErrNoAttachment)
		return
	}
	body, a, err := f.OpenAttachment(n)
	if err != nil {
		req.fail(err)
		return
	}

	// The data of a file of a torrent does not change, and the link contains the torrent and the file.
	c.Header("Cache-Control", "public, max-age=31536000, immutable")
	c.Header("Content-Type", a.ContentType())
	c.Header("ETag", fmt.Sprintf(`"%s-%d-%d-%d"`, hash, videoID, a.Index, a.Size))
	// ServeContent answers the Range requests and HEAD. CORS headers are not set here on purpose: the global
	// CORS middleware already adds them.
	http.ServeContent(c.Writer, c.Request, a.Name, time.Time{}, body)
	if c.Request.Header.Get("Range") == "" && ctx.Err() == nil {
		log.Printf("subs: font %d (%s) of file %d sent: %d bytes in %s", a.Index, a.Name, videoID, a.Size, time.Since(started).Round(time.Millisecond))
	}
}
