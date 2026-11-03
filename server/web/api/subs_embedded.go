package api

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"server/mkvsubs"
	"server/subs"
	"server/torr"
)

// Embedded subtitles are read straight from the Matroska file, so they do not depend on
// the GStreamer pipeline and work for direct streaming (/stream) players too.
//
// A window of N seconds costs the video data of those N seconds (subtitle blocks are
// interleaved with video in the clusters), so players should ask for windows ahead of the
// playback position; the whole track (no "to") reads the whole file.
const (
	embeddedWindowTimeout = 120 * time.Second
	embeddedFullTimeout   = 30 * time.Minute
	embeddedMaxWindow     = 15 * time.Minute
)

type embeddedTrackJS struct {
	mkvsubs.Track
	Embedded bool   `json:"embedded"`
	URL      string `json:"url"` // relative link to the track in its natural form: ASS for ASS/SSA, WebVTT for text; empty for unsupported formats
	VTT      string `json:"vtt"` // relative link to the track as WebVTT (styles of ASS are lost), empty for unsupported formats
}

// embeddedSubs serves ?embedded for /subs/:hash/:id:
//
//	?embedded              (or ?embedded=list) - JSON list of the embedded subtitle tracks;
//	?embedded=N            - track N (0-based, "index" of the list): ASS/SSA as ASS with the original styles,
//	                         text (SRT) tracks as WebVTT;
//	  &format=ass|vtt      - the form of the answer; by default the natural one for the track (ASS or WebVTT).
//	                         ASS can be asked for a text track too (one default style), vtt for an ASS track (styles are lost);
//	  &from=SEC&to=SEC     - only the lines shown within this window; without "to" the whole track.
func embeddedSubs(c *gin.Context, tor *torr.Torrent, hash string, videoID int, video subs.FileInfo) {
	if !isMatroska(video.Path) {
		c.AbortWithError(http.StatusBadRequest, errors.New("embedded subtitles are supported for Matroska files only"))
		return
	}

	trackIdx := -1
	if mode := c.Query("embedded"); mode != "" && mode != "list" {
		n, err := strconv.Atoi(mode)
		if err != nil || n < 0 {
			c.AbortWithError(http.StatusBadRequest, errors.New("\"embedded\" must be empty, \"list\" or a track index"))
			return
		}
		trackIdx = n
	}

	// Options are validated before the torrent is touched.
	format := strings.ToLower(c.Query("format"))
	if format != "" && format != "ass" && format != "vtt" {
		c.AbortWithError(http.StatusBadRequest, errors.New("\"format\" must be \"ass\" or \"vtt\""))
		return
	}
	from, to, err := embeddedWindow(c)
	if err != nil {
		c.AbortWithError(http.StatusBadRequest, err)
		return
	}

	timeout := embeddedWindowTimeout
	if trackIdx >= 0 && to <= 0 {
		timeout = embeddedFullTimeout
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), timeout)
	defer cancel()

	started := time.Now()
	req := mkvRequest{
		c:       c,
		ctx:     ctx,
		what:    fmt.Sprintf("embedded file %d (%s) [%s]", videoID, video.Path, c.Request.URL.RawQuery),
		started: started,
	}
	f, release, err := openMatroska(ctx, tor, video)
	if err != nil {
		req.fail(err)
		return
	}
	defer release()
	opened := time.Since(started)

	// Mode 1: list the tracks.
	if trackIdx < 0 {
		base := "/subs/" + hash + "/" + strconv.Itoa(videoID)
		tracks := f.Tracks()
		out := make([]embeddedTrackJS, 0, len(tracks))
		for _, t := range tracks {
			item := embeddedTrackJS{Track: t, Embedded: true}
			if t.Supported() {
				link := base + "?embedded=" + strconv.Itoa(t.Index) + "&format="
				item.VTT = link + "vtt"
				if t.IsASS() {
					item.URL = link + "ass"
				} else {
					item.URL = item.VTT
				}
			}
			out = append(out, item)
		}
		c.JSON(http.StatusOK, out)
		return
	}

	// Mode 2: one track.
	track, err := f.Track(trackIdx)
	if err != nil {
		req.fail(err)
		return
	}
	events, err := f.Events(ctx, trackIdx, from, to)
	if err != nil {
		req.fail(err)
		return
	}
	if format == "" { // the natural form of the track
		format = "ass"
		if track.IsText() {
			format = "vtt"
		}
	}
	var text string
	contentType := "text/plain"
	switch {
	case format == "ass":
		text = track.ASS(events)
	case track.IsText():
		// the same converter as for the SRT files of the torrent
		text, err = subs.ToVTT([]byte(track.SRT(events)), "srt")
		contentType = "text/vtt"
	default:
		text, err = subs.ToVTT([]byte(track.ASS(events)), "ass")
		contentType = "text/vtt"
	}
	if err != nil {
		req.reply(http.StatusUnprocessableEntity, fmt.Errorf("convert to WebVTT failed: %w", err))
		return
	}
	log.Printf("subs: embedded file %d track %d: %d events (%s..%s), opened in %s, total %s", videoID, trackIdx, len(events), from, to,
		opened.Round(time.Millisecond), time.Since(started).Round(time.Millisecond))

	// CORS headers are not set here on purpose: the global CORS middleware already adds them.
	c.Data(http.StatusOK, contentType+"; charset=utf-8", []byte(text))
}

// embeddedWindow parses "from" and "to" (seconds, may be fractional). to == 0 means "to the end".
func embeddedWindow(c *gin.Context) (from, to time.Duration, err error) {
	parse := func(name string) (time.Duration, error) {
		s := c.Query(name)
		if s == "" {
			return 0, nil
		}
		v, err := strconv.ParseFloat(s, 64)
		if err != nil || v < 0 || v > 24*3600*30 {
			return 0, errors.New("\"" + name + "\" must be a number of seconds")
		}
		return time.Duration(v * float64(time.Second)), nil
	}
	if from, err = parse("from"); err != nil {
		return 0, 0, err
	}
	if to, err = parse("to"); err != nil {
		return 0, 0, err
	}
	if to > 0 && to <= from {
		return 0, 0, errors.New("\"to\" must be greater than \"from\"")
	}
	if to > 0 && to-from > embeddedMaxWindow {
		return 0, 0, errors.New("the window is too long, ask for up to " + embeddedMaxWindow.String())
	}
	return from, to, nil
}
