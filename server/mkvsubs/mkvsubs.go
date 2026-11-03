// Package mkvsubs reads embedded subtitle tracks of Matroska (mkv/webm) files and
// rebuilds ASS/SSA subtitles from them, with the original styles and positions.
// It uses the standard library only.
//
// The file is read through an io.ReadSeeker (a torrent file reader works): only the
// metadata at the start of the file (and the Cues index) is read to open the file;
// events of a time window are read sequentially from the cluster that precedes the window.
//
// Limits: laced blocks and clusters of unknown size are not supported (subtitle
// tracks are never laced, unknown-size clusters appear in live recordings only).
package mkvsubs

import (
	"bufio"
	"bytes"
	"compress/zlib"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/bits"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Element IDs (EBML IDs keep their length marker bits).
const (
	idEBML                = 0x1A45DFA3
	idSegment             = 0x18538067
	idSeekHead            = 0x114D9B74
	idSeek                = 0x4DBB
	idSeekID              = 0x53AB
	idSeekPosition        = 0x53AC
	idInfo                = 0x1549A966
	idTimestampScale      = 0x2AD7B1
	idTracks              = 0x1654AE6B
	idTrackEntry          = 0xAE
	idTrackNumber         = 0xD7
	idTrackType           = 0x83
	idCodecID             = 0x86
	idCodecPrivate        = 0x63A2
	idLanguage            = 0x22B59C
	idLanguageIETF        = 0x22B59D
	idName                = 0x536E
	idDefaultDuration     = 0x23E383
	idFlagDefault         = 0x88
	idFlagForced          = 0x55AA
	idContentEncodings    = 0x6D80
	idContentEncoding     = 0x6240
	idContentEncodingScop = 0x5035
	idContentCompression  = 0x5034
	idContentCompAlgo     = 0x4254
	idContentCompSettings = 0x4255
	idCluster             = 0x1F43B675
	idTimestamp           = 0xE7
	idSimpleBlock         = 0xA3
	idBlockGroup          = 0xA0
	idBlock               = 0xA1
	idBlockDuration       = 0x9B
	idCues                = 0x1C53BB6B
	idAttachments         = 0x1941A469
	idAttachedFile        = 0x61A7
	idFileName            = 0x466E
	idFileMediaType       = 0x4660
	idFileData            = 0x465C
	idCuePoint            = 0xBB
	idCueTime             = 0xB3
	idCueTrackPositions   = 0xB7
	idCueClusterPosition  = 0xF1
)

const (
	trackTypeSubtitle = 17

	maxMetaSize       = 16 << 20 // Tracks / SeekHead / Info
	maxCuesSize       = 64 << 20
	maxBlockSize      = 4 << 20  // a subtitle block is a few hundred bytes
	maxAttachmentSize = 32 << 20 // a font; CJK fonts reach tens of megabytes

	// Skipping more than this is done with Seek, less - by reading through.
	skipSeekThreshold = 1 << 20
)

var (
	ErrNotMatroska       = errors.New("mkvsubs: not a Matroska file")
	ErrNoTracks          = errors.New("mkvsubs: no Tracks element")
	ErrNoTrack           = errors.New("mkvsubs: subtitle track not found")
	ErrNoAttachment      = errors.New("mkvsubs: attachment not found")
	ErrUnsupportedFormat = errors.New("mkvsubs: only ASS/SSA and text (SRT) tracks are supported")
	errBadElement        = errors.New("mkvsubs: broken EBML element")
	errUnknownSize       = errors.New("mkvsubs: elements of unknown size are not supported")
)

// Track is an embedded subtitle track.
type Track struct {
	Index   int    `json:"index"`    // number among the subtitle tracks, 0-based (GStreamer subtitle_N)
	CodecID string `json:"codec_id"` // e.g. "S_TEXT/ASS"
	Format  string `json:"format"`   // "ass", "ssa", "srt", "vtt", "pgs", "vobsub" or the lower-cased codec id
	Lang    string `json:"lang"`     // "ru", "en", ... or "und"
	Title   string `json:"title"`
	Default bool   `json:"default"`
	Forced  bool   `json:"forced"`

	number          uint64
	header          []byte // decoded CodecPrivate
	defaultDuration time.Duration
	compAlgo        int // -1: none, 0: zlib, 3: header stripping
	compSettings    []byte
	compScope       uint64 // bit 0: blocks, bit 1: CodecPrivate
}

// Header returns the ASS header of the track ([Script Info], styles, [Events] and its Format line).
func (t *Track) Header() string { return string(t.header) }

// IsASS reports whether the track holds ASS or SSA subtitles.
func (t *Track) IsASS() bool { return t.Format == "ass" || t.Format == "ssa" }

// IsText reports whether the track holds plain text subtitles (SRT: S_TEXT/UTF8, S_TEXT/ASCII).
// They are served as ASS with one default style, so the players that show ASS can show them too.
func (t *Track) IsText() bool { return t.Format == "srt" }

// Supported reports whether Events and ASS work for the track.
func (t *Track) Supported() bool { return t.IsASS() || t.IsText() }

func (t *Track) decodeBlock(data []byte) ([]byte, error) {
	if t.compAlgo < 0 || t.compScope&1 == 0 {
		return data, nil
	}
	return decompress(t.compAlgo, t.compSettings, data)
}

func decompress(algo int, settings, data []byte) ([]byte, error) {
	switch algo {
	case 0:
		zr, err := zlib.NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		defer zr.Close()
		return io.ReadAll(io.LimitReader(zr, maxBlockSize))
	case 3: // header stripping: the removed prefix is stored in the settings
		out := make([]byte, 0, len(settings)+len(data))
		out = append(out, settings...)
		return append(out, data...), nil
	}
	return nil, fmt.Errorf("mkvsubs: unsupported content compression %d", algo)
}

// Event is one subtitle line (a "Dialogue:" line of an ASS file).
type Event struct {
	ReadOrder  int
	Start, End time.Duration
	Layer      string // Layer (ASS) or Marked (SSA)
	Rest       string // "Style,Name,MarginL,MarginR,MarginV,Effect,Text"
	Text       string // text tracks only: the text of the line, "\n" between the rows
}

type cuePoint struct {
	t   time.Duration
	pos int64
}

// File is an opened Matroska file.
type File struct {
	// Events that started earlier than the window may still be on the screen inside it;
	// the scan starts this much before the window. Long signs are rare, 30 s is plenty.
	Lookback time.Duration

	src          io.ReadSeeker
	size         int64
	segData      int64 // offset of the Segment data (positions in Cues/SeekHead are relative to it)
	segEnd       int64
	scale        int64 // nanoseconds per timestamp tick
	tracks       []*Track
	cuesPos      int64 // -1: no Cues
	attPos       int64 // -1: no Attachments
	attachments  []Attachment
	attLoaded    bool
	firstCluster int64 // -1: none
	cues         []cuePoint
	cuesLoaded   bool
}

// Open reads the metadata of a Matroska file. size is the file size (0 if unknown).
func Open(src io.ReadSeeker, size int64) (*File, error) {
	f := &File{
		Lookback:     30 * time.Second,
		src:          src,
		size:         size,
		scale:        1000000,
		cuesPos:      -1,
		attPos:       -1,
		firstCluster: -1,
	}
	st := newStream(src)
	if err := st.seek(0); err != nil {
		return nil, err
	}
	first, _ := st.br.Peek(16)
	id, sz, _, err := st.header()
	if err != nil {
		if errors.Is(err, errBadElement) {
			return nil, fmt.Errorf("%w: the file starts with % x", ErrNotMatroska, first)
		}
		return nil, fmt.Errorf("mkvsubs: reading the start of the file: %w", err)
	}
	if id != idEBML || sz < 0 || sz > 1<<20 {
		return nil, fmt.Errorf("%w: the file starts with % x", ErrNotMatroska, first)
	}
	if err := st.skip(sz); err != nil {
		return nil, fmt.Errorf("mkvsubs: reading the EBML header: %w", err)
	}
	id, sz, _, err = st.header()
	if err != nil {
		return nil, fmt.Errorf("mkvsubs: reading the Segment header: %w", err)
	}
	if id != idSegment {
		return nil, fmt.Errorf("%w: no Segment after the EBML header", ErrNotMatroska)
	}
	f.segData = st.pos
	f.segEnd = size
	if sz >= 0 && (size <= 0 || f.segData+sz < size) {
		f.segEnd = f.segData + sz
	}
	if f.segEnd <= 0 {
		f.segEnd = 1<<62 - 1
	}

	seeks := make(map[uint32]int64)
	var infoBody, tracksBody []byte
	for i := 0; i < 4096 && st.pos < f.segEnd; i++ {
		pos := st.pos
		id, sz, _, err := st.header()
		if err != nil {
			break
		}
		if sz < 0 {
			break
		}
		if id == idCluster {
			f.firstCluster = pos
			break
		}
		switch id {
		case idSeekHead:
			b, err := st.readBody(sz, maxMetaSize)
			if err != nil {
				return nil, err
			}
			parseSeekHead(b, seeks)
		case idInfo:
			if infoBody, err = st.readBody(sz, maxMetaSize); err != nil {
				return nil, err
			}
		case idTracks:
			if tracksBody, err = st.readBody(sz, maxMetaSize); err != nil {
				return nil, err
			}
		case idAttachments: // only the position is kept: the fonts are big, they are read on demand
			f.attPos = pos
			if err := st.skip(sz); err != nil {
				return nil, err
			}
		default:
			if err := st.skip(sz); err != nil {
				return nil, err
			}
		}
	}

	// Tracks/Info may be placed after the clusters (rare); the SeekHead tells where.
	readAt := func(id uint32) []byte {
		rel, ok := seeks[id]
		if !ok {
			return nil
		}
		if st.seek(f.segData+rel) != nil {
			return nil
		}
		gotID, sz, _, err := st.header()
		if err != nil || gotID != id || sz < 0 {
			return nil
		}
		b, err := st.readBody(sz, maxMetaSize)
		if err != nil {
			return nil
		}
		return b
	}
	if tracksBody == nil {
		tracksBody = readAt(idTracks)
	}
	if infoBody == nil {
		infoBody = readAt(idInfo)
	}
	if tracksBody == nil {
		return nil, ErrNoTracks
	}
	if rel, ok := seeks[idCues]; ok {
		f.cuesPos = f.segData + rel
	}
	if rel, ok := seeks[idAttachments]; ok && f.attPos < 0 {
		f.attPos = f.segData + rel
	}
	if infoBody != nil {
		for _, e := range splitElems(infoBody) {
			if e.id == idTimestampScale {
				if v := int64(uintOf(e.data)); v > 0 {
					f.scale = v
				}
			}
		}
	}
	f.tracks = parseTracks(tracksBody)
	if f.firstCluster < 0 {
		return nil, errors.New("mkvsubs: no clusters found")
	}
	return f, nil
}

// Tracks returns the embedded subtitle tracks (all formats, including bitmap ones).
func (f *File) Tracks() []Track {
	out := make([]Track, len(f.tracks))
	for i, t := range f.tracks {
		out[i] = *t
	}
	return out
}

// Track returns the track with the given index.
func (f *File) Track(index int) (*Track, error) {
	if index < 0 || index >= len(f.tracks) {
		return nil, ErrNoTrack
	}
	return f.tracks[index], nil
}

func (f *File) dur(ticks int64) time.Duration { return time.Duration(ticks * f.scale) }

// Events returns the events of an ASS/SSA track that are on the screen at some moment of
// [from, to). to <= 0 means "until the end of the file" (reads the whole file!).
func (f *File) Events(ctx context.Context, index int, from, to time.Duration) ([]Event, error) {
	tr, err := f.Track(index)
	if err != nil {
		return nil, err
	}
	if !tr.Supported() {
		return nil, ErrUnsupportedFormat
	}
	if from < 0 {
		from = 0
	}
	st := newStream(f.src)
	if err := st.seek(f.startPos(st, from)); err != nil {
		return nil, err
	}

	var events []Event
	for st.pos < f.segEnd {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		id, size, _, err := st.header()
		if err != nil {
			if err == io.EOF {
				break
			}
			return nil, err
		}
		if size < 0 {
			return nil, errUnknownSize
		}
		if id != idCluster { // Cues, Tags, Attachments at the end of the file
			if err := st.skip(size); err != nil {
				return nil, err
			}
			continue
		}
		done, err := f.readCluster(ctx, st, tr, st.pos+size, from, to, &events)
		if err != nil {
			return nil, err
		}
		if done {
			break
		}
	}
	return events, nil
}

// startPos returns the offset to start scanning from: the cluster of the last Cues entry
// that is not later than from-Lookback (or the first cluster).
func (f *File) startPos(st *stream, from time.Duration) int64 {
	pos := f.firstCluster
	limit := from - f.Lookback
	if limit <= 0 {
		return pos
	}
	f.loadCues(st)
	for _, c := range f.cues {
		if c.t > limit {
			break
		}
		pos = c.pos
	}
	return pos
}

// loadCues reads the Cues index; any problem just means "no index" (linear scan).
func (f *File) loadCues(st *stream) {
	if f.cuesLoaded {
		return
	}
	f.cuesLoaded = true
	if f.cuesPos < 0 || st.seek(f.cuesPos) != nil {
		return
	}
	id, sz, _, err := st.header()
	if err != nil || id != idCues || sz < 0 {
		return
	}
	b, err := st.readBody(sz, maxCuesSize)
	if err != nil {
		return
	}
	for _, e := range splitElems(b) {
		if e.id != idCuePoint {
			continue
		}
		var (
			ticks   uint64
			pos     uint64
			hasTime bool
			hasPos  bool
		)
		for _, c := range splitElems(e.data) {
			switch c.id {
			case idCueTime:
				ticks, hasTime = uintOf(c.data), true
			case idCueTrackPositions:
				for _, p := range splitElems(c.data) {
					if p.id == idCueClusterPosition && !hasPos {
						pos, hasPos = uintOf(p.data), true
					}
				}
			}
		}
		if hasTime && hasPos {
			f.cues = append(f.cues, cuePoint{t: f.dur(int64(ticks)), pos: f.segData + int64(pos)})
		}
	}
	sort.SliceStable(f.cues, func(i, j int) bool { return f.cues[i].t < f.cues[j].t })
}

// readCluster reads one cluster (the header is already consumed). done=true means the
// window is over and no later cluster is needed.
func (f *File) readCluster(ctx context.Context, st *stream, tr *Track, end int64, from, to time.Duration, out *[]Event) (done bool, err error) {
	var ctime int64
	for st.pos < end {
		id, size, _, err := st.header()
		if err != nil {
			return false, unexpectedEOF(err)
		}
		if size < 0 {
			return false, errUnknownSize
		}
		childEnd := st.pos + size
		switch id {
		case idTimestamp:
			b, err := st.readBody(size, 8)
			if err != nil {
				return false, unexpectedEOF(err)
			}
			ctime = int64(uintOf(b))
			ct := f.dur(ctime)
			if to > 0 && ct > to {
				return true, nil
			}
			if ct < from-f.Lookback { // nothing of this cluster can reach the window
				return false, st.seek(end)
			}
		case idSimpleBlock:
			payload, rel, ok, err := st.readBlock(tr, size)
			if err != nil {
				return false, unexpectedEOF(err)
			}
			if ok {
				f.addEvent(out, tr, ctime, rel, -1, payload, from, to)
			}
		case idBlockGroup:
			var (
				payload []byte
				rel     int16
				have    bool
				dur     int64 = -1
			)
			for st.pos < childEnd {
				gid, gsize, _, err := st.header()
				if err != nil {
					return false, unexpectedEOF(err)
				}
				if gsize < 0 {
					return false, errUnknownSize
				}
				switch gid {
				case idBlock:
					p, r, ok, err := st.readBlock(tr, gsize)
					if err != nil {
						return false, unexpectedEOF(err)
					}
					if ok {
						payload, rel, have = p, r, true
					}
				case idBlockDuration:
					b, err := st.readBody(gsize, 8)
					if err != nil {
						return false, unexpectedEOF(err)
					}
					dur = int64(uintOf(b))
				default:
					if err := st.skip(gsize); err != nil {
						return false, unexpectedEOF(err)
					}
				}
			}
			if have {
				f.addEvent(out, tr, ctime, rel, dur, payload, from, to)
			}
		default:
			if err := st.skip(size); err != nil {
				return false, unexpectedEOF(err)
			}
		}
	}
	return false, ctx.Err()
}

func (f *File) addEvent(out *[]Event, tr *Track, ctime int64, rel int16, dur int64, payload []byte, from, to time.Duration) {
	data, err := tr.decodeBlock(payload)
	if err != nil {
		return
	}
	start := f.dur(ctime + int64(rel))
	var end time.Duration
	switch {
	case dur >= 0:
		end = start + f.dur(dur)
	case tr.defaultDuration > 0:
		end = start + tr.defaultDuration
	default:
		return // no way to know how long the line is shown
	}
	if end <= from || (to > 0 && start >= to) {
		return
	}
	if tr.IsText() {
		text := cleanText(string(data))
		if text == "" {
			return
		}
		// there is no ReadOrder in plain text blocks: the start time keeps the order
		*out = append(*out, Event{
			ReadOrder: int(start / time.Millisecond),
			Start:     start,
			End:       end,
			Text:      text,
		})
		return
	}

	// ASS in Matroska: "ReadOrder,Layer,Style,Name,MarginL,MarginR,MarginV,Effect,Text"
	// (SSA: the second field is "Marked"). The text may contain commas.
	p := strings.SplitN(string(data), ",", 9)
	if len(p) < 9 {
		return
	}
	order, _ := strconv.Atoi(strings.TrimSpace(p[0]))
	*out = append(*out, Event{
		ReadOrder: order,
		Start:     start,
		End:       end,
		Layer:     p[1],
		Rest:      strings.Join(p[2:], ","),
	})
}

// ASS builds an ASS file: the track header and the events, ordered as in the original script.
func (t *Track) ASS(events []Event) string {
	sorted := make([]Event, len(events))
	copy(sorted, events)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].ReadOrder < sorted[j].ReadOrder })

	raw := string(t.header)
	if t.IsText() {
		raw = textASSHeader
	}
	head := strings.TrimPrefix(raw, "\xef\xbb\xbf")
	head = strings.ReplaceAll(head, "\r\n", "\n")
	if !strings.HasSuffix(head, "\n") {
		head += "\n"
	}
	var b strings.Builder
	b.WriteString(head)
	if !strings.Contains(strings.ToLower(head), "[events]") {
		b.WriteString("\n[Events]\nFormat: Layer, Start, End, Style, Name, MarginL, MarginR, MarginV, Effect, Text\n")
	}
	for _, e := range sorted {
		end := e.End
		if end < e.Start+10*time.Millisecond {
			end = e.Start + 10*time.Millisecond
		}
		layer, rest := e.Layer, e.Rest
		if t.IsText() {
			layer, rest = "0", "Default,,0,0,0,,"+srtToASS(e.Text)
		}
		fmt.Fprintf(&b, "Dialogue: %s,%s,%s,%s\n", layer, assTime(e.Start), assTime(end), rest)
	}
	return b.String()
}

// SRT builds an SRT file from the events of a text track (sorted by the start time).
// The server turns it into WebVTT with the same converter as for the SRT files of a torrent.
func (t *Track) SRT(events []Event) string {
	sorted := make([]Event, len(events))
	copy(sorted, events)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Start < sorted[j].Start })

	var b strings.Builder
	for i, e := range sorted {
		end := e.End
		if end < e.Start+10*time.Millisecond {
			end = e.Start + 10*time.Millisecond
		}
		fmt.Fprintf(&b, "%d\n%s --> %s\n%s\n\n", i+1, srtTime(e.Start), srtTime(end), e.Text)
	}
	return b.String()
}

// srtTime formats d as HH:MM:SS,mmm.
func srtTime(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	ms := int64(d / time.Millisecond)
	return fmt.Sprintf("%02d:%02d:%02d,%03d", ms/3600000, ms/60000%60, ms/1000%60, ms%1000)
}

var reBlankLines = regexp.MustCompile(`\n{2,}`)

// cleanText normalizes the text of an SRT block: "\n" line breaks, no empty lines (an empty
// line would end the cue in SRT and WebVTT), no spaces around.
func cleanText(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	s = reBlankLines.ReplaceAllString(strings.TrimSpace(s), "\n")
	return s
}

// textASSHeader is the header for text tracks: one Default style (the usual 384x288 script
// of SRT-to-ASS converters, so the size of the text matches what players show for SRT).
const textASSHeader = "[Script Info]\nScriptType: v4.00+\nPlayResX: 384\nPlayResY: 288\nWrapStyle: 0\nScaledBorderAndShadow: yes\n\n" +
	"[V4+ Styles]\nFormat: Name, Fontname, Fontsize, PrimaryColour, SecondaryColour, OutlineColour, BackColour, Bold, Italic, Underline, StrikeOut, ScaleX, ScaleY, Spacing, Angle, BorderStyle, Outline, Shadow, Alignment, MarginL, MarginR, MarginV, Encoding\n" +
	"Style: Default,Arial,16,&H00FFFFFF,&H000000FF,&H00000000,&H00000000,0,0,0,0,100,100,0,0,1,1,0,2,10,10,10,1\n\n" +
	"[Events]\nFormat: Layer, Start, End, Style, Name, MarginL, MarginR, MarginV, Effect, Text\n"

var (
	reSrtFont    = regexp.MustCompile(`(?i)<\s*font\b[^>]*\bcolor\s*=\s*["']?#?([0-9a-f]{6})["']?[^>]*>`)
	reSrtFontEnd = regexp.MustCompile(`(?i)<\s*/\s*font\s*>`)
	reSrtOpen    = regexp.MustCompile(`(?i)<\s*([ibus])\s*>`)
	reSrtClose   = regexp.MustCompile(`(?i)<\s*/\s*([ibus])\s*>`)
	reSrtTag     = regexp.MustCompile(`</?[A-Za-z][^>]*>`)
	srtEntities  = strings.NewReplacer("&lt;", "<", "&gt;", ">", "&amp;", "&", "&nbsp;", " ")
)

// srtToASS converts the text of an SRT block to the text of an ASS event: line breaks become \N,
// <i> <b> <u> <s> and <font color> become override tags, other tags are dropped, stray braces are
// escaped (an ASS block like {\an8} that is already in the text is kept).
func srtToASS(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	s = escapeBraces(s)
	s = reSrtFont.ReplaceAllStringFunc(s, func(m string) string {
		hex := reSrtFont.FindStringSubmatch(m)[1] // RRGGBB -> &HBBGGRR&
		return `{\c&H` + hex[4:6] + hex[2:4] + hex[0:2] + `&}`
	})
	s = reSrtFontEnd.ReplaceAllString(s, `{\c}`)
	s = reSrtOpen.ReplaceAllStringFunc(s, func(m string) string {
		return `{\` + strings.ToLower(reSrtOpen.FindStringSubmatch(m)[1]) + `1}`
	})
	s = reSrtClose.ReplaceAllStringFunc(s, func(m string) string {
		return `{\` + strings.ToLower(reSrtClose.FindStringSubmatch(m)[1]) + `0}`
	})
	s = reSrtTag.ReplaceAllString(s, "")
	s = srtEntities.Replace(s)
	return strings.ReplaceAll(s, "\n", `\N`)
}

func escapeBraces(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '{' && i+1 < len(s) && s[i+1] == '\\':
			end := strings.IndexByte(s[i:], '}')
			if end < 0 {
				b.WriteString(`\{`)
				continue
			}
			b.WriteString(s[i : i+end+1])
			i += end
		case c == '{':
			b.WriteString(`\{`)
		case c == '}':
			b.WriteString(`\}`)
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// assTime formats d as H:MM:SS.cc (centiseconds, rounded).
func assTime(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	cs := int64((d + 5*time.Millisecond) / (10 * time.Millisecond))
	return fmt.Sprintf("%d:%02d:%02d.%02d", cs/360000, cs/6000%60, cs/100%60, cs%100)
}

// ---------- Attachments ----------

// Attachment is a file attached to the Matroska file: fonts of the subtitles, covers...
type Attachment struct {
	Index     int    `json:"index"` // position among all attachments of the file
	Name      string `json:"name"`
	MediaType string `json:"type"`
	Size      int64  `json:"size"`

	offset int64 // position of the data in the file
}

// IsFont reports whether the attachment is a font file that the subtitle renderers can use:
// TrueType, OpenType or a TrueType collection.
func (a Attachment) IsFont() bool {
	switch strings.ToLower(path.Ext(a.Name)) {
	case ".ttf", ".otf", ".ttc":
		return true
	case ".woff", ".woff2":
		return false
	}
	t := strings.ToLower(a.MediaType)
	return strings.Contains(t, "truetype") || strings.Contains(t, "opentype") ||
		t == "font/ttf" || t == "font/otf" || t == "font/sfnt" || t == "font/collection" || t == "application/font-sfnt"
}

// ContentType is the type to serve a font attachment with.
func (a Attachment) ContentType() string {
	switch strings.ToLower(path.Ext(a.Name)) {
	case ".otf":
		return "font/otf"
	case ".ttc":
		return "font/collection"
	}
	return "font/ttf"
}

// Attachments returns the attachments of the file. The data is not read (big files are skipped with
// Seek), so the list costs only the headers: it does not download the fonts of a torrent.
func (f *File) Attachments() ([]Attachment, error) {
	if f.attLoaded {
		return f.attachments, nil
	}
	if f.attPos < 0 {
		f.attLoaded = true
		return nil, nil
	}
	st := newStream(f.src)
	if err := st.seek(f.attPos); err != nil {
		return nil, err
	}
	id, sz, _, err := st.header()
	if err != nil {
		return nil, unexpectedEOF(err)
	}
	if id != idAttachments || sz < 0 {
		f.attLoaded = true
		return nil, nil
	}
	end := st.pos + sz
	var list []Attachment
	for st.pos < end {
		id, size, _, err := st.header()
		if err != nil {
			return nil, unexpectedEOF(err)
		}
		if size < 0 {
			return nil, errUnknownSize
		}
		if id != idAttachedFile { // CRC-32 and unknown elements
			if err := st.skip(size); err != nil {
				return nil, unexpectedEOF(err)
			}
			continue
		}
		a, ok, err := readAttachedFile(st, st.pos+size)
		if err != nil {
			return nil, unexpectedEOF(err)
		}
		if ok {
			a.Index = len(list)
			list = append(list, a)
		}
	}
	f.attachments, f.attLoaded = list, true
	return list, nil
}

// readAttachedFile reads one AttachedFile element (its header is already consumed).
func readAttachedFile(st *stream, end int64) (a Attachment, ok bool, err error) {
	for st.pos < end {
		id, size, _, err := st.header()
		if err != nil {
			return a, false, err
		}
		if size < 0 {
			return a, false, errUnknownSize
		}
		switch id {
		case idFileName, idFileMediaType:
			b, err := st.readBody(size, 4096)
			if err != nil {
				return a, false, err
			}
			if id == idFileName {
				a.Name = string(b)
			} else {
				a.MediaType = string(b)
			}
		case idFileData:
			a.offset, a.Size, ok = st.pos, size, true
			if err := st.skip(size); err != nil {
				return a, false, err
			}
		default:
			if err := st.skip(size); err != nil {
				return a, false, err
			}
		}
	}
	return a, ok, nil
}

// section is an io.ReadSeeker over a part of the file (the data of an attachment). It seeks the underlying
// reader only when the position differs from where that reader is, so reading a font sequentially costs
// one Seek.
type section struct {
	src       io.ReadSeeker
	off, size int64
	pos       int64 // position inside the section
	cur       int64 // where the underlying reader is, -1 if unknown
}

func (s *section) Read(p []byte) (int, error) {
	if s.pos >= s.size {
		return 0, io.EOF
	}
	if left := s.size - s.pos; int64(len(p)) > left {
		p = p[:left]
	}
	if s.cur != s.off+s.pos {
		if _, err := s.src.Seek(s.off+s.pos, io.SeekStart); err != nil {
			s.cur = -1
			return 0, err
		}
		s.cur = s.off + s.pos
	}
	n, err := s.src.Read(p)
	s.pos += int64(n)
	s.cur += int64(n)
	return n, err
}

func (s *section) Seek(offset int64, whence int) (int64, error) {
	var pos int64
	switch whence {
	case io.SeekStart:
		pos = offset
	case io.SeekCurrent:
		pos = s.pos + offset
	case io.SeekEnd:
		pos = s.size + offset
	default:
		return 0, errors.New("mkvsubs: bad whence")
	}
	if pos < 0 {
		return 0, errors.New("mkvsubs: negative position")
	}
	s.pos = pos
	return pos, nil
}

// OpenAttachment returns a reader of the data of the attachment (it supports Seek, so it can be served with
// http.ServeContent and Range requests). The reader uses the reader of the file: the two must not be used at
// the same time.
func (f *File) OpenAttachment(index int) (io.ReadSeeker, Attachment, error) {
	list, err := f.Attachments()
	if err != nil {
		return nil, Attachment{}, err
	}
	if index < 0 || index >= len(list) {
		return nil, Attachment{}, ErrNoAttachment
	}
	a := list[index]
	return &section{src: f.src, off: a.offset, size: a.Size, cur: -1}, a, nil
}

// ReadAttachment returns the data of the attachment with the given index.
func (f *File) ReadAttachment(index int) ([]byte, error) {
	list, err := f.Attachments()
	if err != nil {
		return nil, err
	}
	if index < 0 || index >= len(list) {
		return nil, ErrNoAttachment
	}
	a := list[index]
	st := newStream(f.src)
	if err := st.seek(a.offset); err != nil {
		return nil, err
	}
	b, err := st.readBody(a.Size, maxAttachmentSize)
	if err != nil {
		return nil, err
	}
	return b, nil
}

// ---------- Tracks ----------

func parseTracks(b []byte) []*Track {
	var out []*Track
	for _, e := range splitElems(b) {
		if e.id != idTrackEntry {
			continue
		}
		t := &Track{compAlgo: -1, compScope: 1, Default: true}
		var typ uint64
		var lang, langIETF string
		for _, c := range splitElems(e.data) {
			switch c.id {
			case idTrackNumber:
				t.number = uintOf(c.data)
			case idTrackType:
				typ = uintOf(c.data)
			case idCodecID:
				t.CodecID = string(c.data)
			case idCodecPrivate:
				t.header = c.data
			case idLanguage:
				lang = string(bytes.TrimRight(c.data, "\x00"))
			case idLanguageIETF:
				langIETF = string(bytes.TrimRight(c.data, "\x00"))
			case idName:
				t.Title = string(bytes.TrimRight(c.data, "\x00"))
			case idDefaultDuration:
				t.defaultDuration = time.Duration(uintOf(c.data))
			case idFlagDefault:
				t.Default = uintOf(c.data) != 0
			case idFlagForced:
				t.Forced = uintOf(c.data) != 0
			case idContentEncodings:
				parseEncodings(c.data, t)
			}
		}
		if typ != trackTypeSubtitle {
			continue
		}
		t.Index = len(out)
		t.Format = formatOf(t.CodecID)
		t.Lang = langOf(lang, langIETF)
		if t.compAlgo >= 0 && t.compScope&2 != 0 {
			if h, err := decompress(t.compAlgo, t.compSettings, t.header); err == nil {
				t.header = h
			}
		}
		out = append(out, t)
	}
	return out
}

func parseEncodings(b []byte, t *Track) {
	for _, e := range splitElems(b) {
		if e.id != idContentEncoding {
			continue
		}
		for _, c := range splitElems(e.data) {
			switch c.id {
			case idContentEncodingScop:
				t.compScope = uintOf(c.data)
			case idContentCompression:
				t.compAlgo = 0 // the default algorithm is zlib
				for _, x := range splitElems(c.data) {
					switch x.id {
					case idContentCompAlgo:
						t.compAlgo = int(uintOf(x.data))
					case idContentCompSettings:
						t.compSettings = x.data
					}
				}
			}
		}
	}
}

func formatOf(codecID string) string {
	switch strings.ToUpper(codecID) {
	case "S_TEXT/ASS", "S_ASS":
		return "ass"
	case "S_TEXT/SSA", "S_SSA":
		return "ssa"
	case "S_TEXT/UTF8", "S_TEXT/ASCII":
		return "srt"
	case "S_TEXT/WEBVTT":
		return "vtt"
	case "S_HDMV/PGS":
		return "pgs"
	case "S_VOBSUB":
		return "vobsub"
	}
	return strings.ToLower(codecID)
}

var iso639 = map[string]string{
	"rus": "ru", "eng": "en", "jpn": "ja", "ukr": "uk", "ger": "de", "deu": "de", "fre": "fr", "fra": "fr",
	"spa": "es", "ita": "it", "por": "pt", "chi": "zh", "zho": "zh", "kor": "ko", "pol": "pl", "tur": "tr",
	"ara": "ar", "cze": "cs", "ces": "cs", "dut": "nl", "nld": "nl", "swe": "sv", "heb": "he", "hin": "hi",
}

func langOf(lang, ietf string) string {
	if ietf != "" {
		primary := strings.ToLower(strings.SplitN(ietf, "-", 2)[0])
		if len(primary) == 2 {
			return primary
		}
		lang = primary
	}
	lang = strings.ToLower(lang)
	if v, ok := iso639[lang]; ok {
		return v
	}
	if lang == "" || lang == "und" {
		return "und"
	}
	return lang
}

func parseSeekHead(b []byte, seeks map[uint32]int64) {
	for _, e := range splitElems(b) {
		if e.id != idSeek {
			continue
		}
		var (
			id  uint64
			pos uint64
		)
		for _, c := range splitElems(e.data) {
			switch c.id {
			case idSeekID:
				id = uintOf(c.data)
			case idSeekPosition:
				pos = uintOf(c.data)
			}
		}
		if id != 0 {
			seeks[uint32(id)] = int64(pos)
		}
	}
}

// ---------- EBML ----------

type node struct {
	id   uint32
	data []byte
}

// splitElems splits the body of a master element into its children; a damaged tail is ignored.
func splitElems(b []byte) []node {
	var out []node
	for len(b) > 0 {
		id, n1, err := parseID(b)
		if err != nil {
			break
		}
		size, n2, err := parseSize(b[n1:])
		if err != nil {
			break
		}
		rest := b[n1+n2:]
		if size < 0 || size > int64(len(rest)) {
			size = int64(len(rest))
		}
		out = append(out, node{id: id, data: rest[:size]})
		b = rest[size:]
	}
	return out
}

func uintOf(b []byte) uint64 {
	var v uint64
	for _, x := range b {
		v = v<<8 | uint64(x)
	}
	return v
}

// parseID parses an element ID (the length marker bits stay in the value).
func parseID(b []byte) (id uint32, n int, err error) {
	if len(b) == 0 {
		return 0, 0, io.ErrUnexpectedEOF
	}
	n = bits.LeadingZeros8(b[0]) + 1
	if n > 4 {
		return 0, 0, errBadElement
	}
	if len(b) < n {
		return 0, 0, io.ErrUnexpectedEOF
	}
	for i := 0; i < n; i++ {
		id = id<<8 | uint32(b[i])
	}
	return id, n, nil
}

// parseSize parses an element size or a track number; size = -1 means "unknown size".
func parseSize(b []byte) (size int64, n int, err error) {
	if len(b) == 0 {
		return 0, 0, io.ErrUnexpectedEOF
	}
	n = bits.LeadingZeros8(b[0]) + 1
	if n > 8 {
		return 0, 0, errBadElement
	}
	if len(b) < n {
		return 0, 0, io.ErrUnexpectedEOF
	}
	mask := uint64(0xFF) >> uint(n)
	v := uint64(b[0]) & mask
	allOnes := v == mask
	for i := 1; i < n; i++ {
		v = v<<8 | uint64(b[i])
		if b[i] != 0xFF {
			allOnes = false
		}
	}
	if allOnes {
		return -1, n, nil
	}
	return int64(v), n, nil
}

func unexpectedEOF(err error) error {
	if err == io.EOF {
		return io.ErrUnexpectedEOF
	}
	return err
}

// stream reads a file sequentially (buffered); small skips are read through, big ones are Seeks.
type stream struct {
	src io.ReadSeeker
	br  *bufio.Reader
	pos int64
}

func newStream(src io.ReadSeeker) *stream {
	return &stream{src: src, br: bufio.NewReaderSize(src, 256<<10)}
}

func (s *stream) seek(pos int64) error {
	if _, err := s.src.Seek(pos, io.SeekStart); err != nil {
		return err
	}
	s.br.Reset(s.src)
	s.pos = pos
	return nil
}

func (s *stream) skip(n int64) error {
	if n <= 0 {
		return nil
	}
	if n >= skipSeekThreshold {
		return s.seek(s.pos + n)
	}
	d, err := s.br.Discard(int(n))
	s.pos += int64(d)
	return err
}

// header reads an element header; io.EOF means a clean end of the file.
func (s *stream) header() (id uint32, size int64, hl int, err error) {
	b, perr := s.br.Peek(12)
	if len(b) == 0 {
		if perr == nil {
			perr = io.EOF
		}
		return 0, 0, 0, perr
	}
	var n1, n2 int
	if id, n1, err = parseID(b); err != nil {
		return 0, 0, 0, err
	}
	if size, n2, err = parseSize(b[n1:]); err != nil {
		return 0, 0, 0, err
	}
	hl = n1 + n2
	s.br.Discard(hl)
	s.pos += int64(hl)
	return id, size, hl, nil
}

func (s *stream) readBody(size int64, limit int64) ([]byte, error) {
	if size < 0 || size > limit {
		return nil, fmt.Errorf("mkvsubs: element of %d bytes is too big", size)
	}
	buf := make([]byte, size)
	n, err := io.ReadFull(s.br, buf)
	s.pos += int64(n)
	if err != nil {
		return nil, unexpectedEOF(err)
	}
	return buf, nil
}

// readBlock reads a (Simple)Block of size bytes. Blocks of other tracks (video, audio) are
// skipped without being kept in memory. ok=false means "not ours / not supported".
func (s *stream) readBlock(tr *Track, size int64) (payload []byte, rel int16, ok bool, err error) {
	b, _ := s.br.Peek(11) // track number (1-8 bytes) + timestamp (2) + flags (1)
	num, n, perr := parseSize(b)
	if perr != nil || len(b) < n+3 || size < int64(n+3) {
		return nil, 0, false, s.skip(size)
	}
	flags := b[n+2]
	if uint64(num) != tr.number || flags&0x06 != 0 { // another track, or laced
		return nil, 0, false, s.skip(size)
	}
	rel = int16(binary.BigEndian.Uint16(b[n:]))
	s.br.Discard(n + 3)
	s.pos += int64(n + 3)
	payload, err = s.readBody(size-int64(n+3), maxBlockSize)
	if err != nil {
		return nil, 0, false, err
	}
	return payload, rel, true, nil
}
