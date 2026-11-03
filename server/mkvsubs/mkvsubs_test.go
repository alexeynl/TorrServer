package mkvsubs

import (
	"bytes"
	"compress/zlib"
	"context"
	"encoding/binary"
	"io"
	"strings"
	"testing"
	"time"
)

// ---------- a tiny Matroska writer for the tests ----------

func idBytes(id uint32) []byte {
	switch {
	case id > 0xFFFFFF:
		return []byte{byte(id >> 24), byte(id >> 16), byte(id >> 8), byte(id)}
	case id > 0xFFFF:
		return []byte{byte(id >> 16), byte(id >> 8), byte(id)}
	case id > 0xFF:
		return []byte{byte(id >> 8), byte(id)}
	}
	return []byte{byte(id)}
}

// size is always written as an 8-byte vint, so the sizes of the elements are predictable.
func sizeBytes(n int) []byte {
	return []byte{0x01, 0, 0, 0, byte(n >> 24), byte(n >> 16), byte(n >> 8), byte(n)}
}

func elem(id uint32, parts ...[]byte) []byte {
	body := bytes.Join(parts, nil)
	out := append([]byte{}, idBytes(id)...)
	out = append(out, sizeBytes(len(body))...)
	return append(out, body...)
}

func uintElem(id uint32, v uint64) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, v)
	return elem(id, b)
}

func block(track byte, rel int16, payload []byte) []byte {
	h := []byte{0x80 | track, byte(uint16(rel) >> 8), byte(uint16(rel)), 0}
	return append(h, payload...)
}

const testHeader = "[Script Info]\r\nScriptType: v4.00+\r\nPlayResX: 640\r\nPlayResY: 360\r\n\r\n" +
	"[V4+ Styles]\r\nFormat: Name, Fontname, Fontsize\r\nStyle: Default,Arial,22\r\n\r\n" +
	"[Events]\r\nFormat: Layer, Start, End, Style, Name, MarginL, MarginR, MarginV, Effect, Text\r\n"

type testEvent struct {
	cluster int // cluster index
	rel     int16
	durMs   uint64
	payload string
}

var testEvents = []testEvent{
	{0, 1000, 2000, `0,0,Default,,0,0,0,,Hello, world`},
	{0, 2000, 15000, `1,1,Sign,,0,0,0,,{\pos(1,2)}Long sign`},
	{1, 2000, 1000, `2,0,Default,Bob,5,6,7,Fx,Mid`},
	{2, 5000, 1000, `3,0,Default,,0,0,0,,Late`},
}

var clusterTimes = []uint64{0, 10000, 20000}

// buildMKV creates a file with a video track (1) and an ASS track (2).
// compression: "" (none), "zlib" or "strip" (header stripping of the "0," prefix is not
// used here, the stripped prefix is "X").
func buildMKV(compression string) []byte {
	subParts := [][]byte{
		uintElem(idTrackNumber, 2),
		uintElem(idTrackType, trackTypeSubtitle),
		elem(idCodecID, []byte("S_TEXT/ASS")),
		elem(idCodecPrivate, []byte(testHeader)),
		elem(idLanguage, []byte("rus")),
		elem(idName, []byte("Test ASS")),
	}
	switch compression {
	case "zlib":
		subParts = append(subParts, elem(idContentEncodings, elem(idContentEncoding,
			uintElem(idContentEncodingScop, 1),
			elem(idContentCompression, uintElem(idContentCompAlgo, 0)))))
	case "strip":
		subParts = append(subParts, elem(idContentEncodings, elem(idContentEncoding,
			uintElem(idContentEncodingScop, 1),
			elem(idContentCompression, uintElem(idContentCompAlgo, 3), elem(idContentCompSettings, []byte("X"))))))
	}
	subTrack := bytes.Join(subParts, nil)
	videoTrack := bytes.Join([][]byte{
		uintElem(idTrackNumber, 1),
		uintElem(idTrackType, 1),
		elem(idCodecID, []byte("V_MPEG4/ISO/AVC")),
	}, nil)
	tracks := elem(idTracks, elem(idTrackEntry, videoTrack), elem(idTrackEntry, subTrack))
	info := elem(idInfo, uintElem(idTimestampScale, 1000000))

	var clusters [][]byte
	var clusterOffsets []int
	// Cluster offsets depend on the lengths of the elements before them, so they are
	// computed after the SeekHead (fixed length) and Info/Tracks are known.
	seekLen := len(buildSeekHead(0, 0, 0))
	offset := seekLen + len(info) + len(tracks)
	for ci, ct := range clusterTimes {
		parts := [][]byte{uintElem(idTimestamp, ct)}
		parts = append(parts, elem(idSimpleBlock, block(1, 0, bytes.Repeat([]byte{0xAB}, 300))))
		for _, e := range testEvents {
			if e.cluster != ci {
				continue
			}
			payload := []byte(e.payload)
			switch compression {
			case "zlib":
				var zb bytes.Buffer
				zw := zlib.NewWriter(&zb)
				zw.Write(payload)
				zw.Close()
				payload = zb.Bytes()
			case "strip":
				payload = bytes.TrimPrefix(payload, []byte("X")) // nothing to strip: the prefix is added back
			}
			parts = append(parts, elem(idBlockGroup,
				elem(idBlock, block(2, e.rel, payload)),
				uintElem(idBlockDuration, e.durMs)))
		}
		parts = append(parts, elem(idSimpleBlock, block(1, 40, bytes.Repeat([]byte{0xCD}, 200))))
		c := elem(idCluster, parts...)
		clusterOffsets = append(clusterOffsets, offset)
		offset += len(c)
		clusters = append(clusters, c)
	}

	var cuePoints [][]byte
	for i, ct := range clusterTimes {
		cuePoints = append(cuePoints, elem(idCuePoint,
			uintElem(idCueTime, ct),
			elem(idCueTrackPositions, uintElem(0xF7, 1), uintElem(idCueClusterPosition, uint64(clusterOffsets[i])))))
	}
	cues := elem(idCues, cuePoints...)

	seek := buildSeekHead(seekLen, seekLen+len(info), offset)
	seg := bytes.Join([][]byte{seek, info, tracks}, nil)
	seg = append(seg, bytes.Join(clusters, nil)...)
	seg = append(seg, cues...)

	ebml := elem(idEBML, elem(0x4282, []byte("matroska")))
	return append(ebml, elem(idSegment, seg)...)
}

// buildSeekHead has a fixed length: positions are always 4 bytes.
func buildSeekHead(infoPos, tracksPos, cuesPos int) []byte {
	entry := func(id uint32, pos int) []byte {
		p := make([]byte, 4)
		binary.BigEndian.PutUint32(p, uint32(pos))
		return elem(idSeek, elem(idSeekID, idBytes(id)), elem(idSeekPosition, p))
	}
	return elem(idSeekHead, entry(idInfo, infoPos), entry(idTracks, tracksPos), entry(idCues, cuesPos))
}

func open(t *testing.T, compression string) *File {
	t.Helper()
	data := buildMKV(compression)
	f, err := Open(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return f
}

// ---------- tests ----------

func TestTracks(t *testing.T) {
	f := open(t, "")
	tr := f.Tracks()
	if len(tr) != 1 {
		t.Fatalf("tracks = %d, want 1", len(tr))
	}
	got := tr[0]
	if got.Index != 0 || got.Format != "ass" || got.Lang != "ru" || got.Title != "Test ASS" || got.CodecID != "S_TEXT/ASS" {
		t.Fatalf("track = %+v", got)
	}
	if got.Header() != testHeader {
		t.Fatalf("header differs")
	}
}

func lines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if strings.HasPrefix(l, "Dialogue:") {
			out = append(out, l)
		}
	}
	return out
}

func TestWholeFile(t *testing.T) {
	for _, comp := range []string{"", "zlib"} {
		f := open(t, comp)
		tr, _ := f.Track(0)
		events, err := f.Events(context.Background(), 0, 0, 0)
		if err != nil {
			t.Fatalf("%q: Events: %v", comp, err)
		}
		out := tr.ASS(events)
		if !strings.HasPrefix(out, "[Script Info]\n") || strings.Contains(out, "\r") {
			t.Fatalf("%q: header is not normalized: %q", comp, out[:40])
		}
		want := []string{
			`Dialogue: 0,0:00:01.00,0:00:03.00,Default,,0,0,0,,Hello, world`,
			`Dialogue: 1,0:00:02.00,0:00:17.00,Sign,,0,0,0,,{\pos(1,2)}Long sign`,
			`Dialogue: 0,0:00:12.00,0:00:13.00,Default,Bob,5,6,7,Fx,Mid`,
			`Dialogue: 0,0:00:25.00,0:00:26.00,Default,,0,0,0,,Late`,
		}
		got := lines(out)
		if len(got) != len(want) {
			t.Fatalf("%q: got %d lines:\n%s", comp, len(got), strings.Join(got, "\n"))
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("%q: line %d:\n got  %s\n want %s", comp, i, got[i], want[i])
			}
		}
	}
}

func TestWindow(t *testing.T) {
	f := open(t, "")
	f.Lookback = 2 * time.Second // the cluster at 0 s must be skipped for a window at 21 s
	events, err := f.Events(context.Background(), 0, 21*time.Second, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].ReadOrder != 3 {
		t.Fatalf("events = %+v, want only ReadOrder 3", events)
	}

	// A window in the middle: the long sign (2-17 s) overlaps it, "Mid" (12-13 s) is inside.
	f.Lookback = 30 * time.Second
	events, err = f.Events(context.Background(), 0, 11*time.Second, 14*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0].ReadOrder != 1 || events[1].ReadOrder != 2 {
		t.Fatalf("events = %+v, want ReadOrder 1 and 2", events)
	}

	// Past the last event.
	events, err = f.Events(context.Background(), 0, 40*time.Second, 50*time.Second)
	if err != nil || len(events) != 0 {
		t.Fatalf("events = %+v, err = %v", events, err)
	}
}

func TestHeaderStripping(t *testing.T) {
	// With header stripping the stored payload lacks the settings prefix ("X" here).
	// The test file stores payloads without it, so Events must return "X"+payload.
	f := open(t, "strip")
	events, err := f.Events(context.Background(), 0, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) == 0 {
		t.Fatal("no events")
	}
	// "X0,0,Default,..." splits into ReadOrder "X0" (not a number -> 0).
	if events[0].Layer != "0" {
		t.Fatalf("event = %+v", events[0])
	}
}

func TestCanceledContext(t *testing.T) {
	f := open(t, "")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := f.Events(ctx, 0, 0, 0); err == nil {
		t.Fatal("expected a context error")
	}
}

func TestNotMatroska(t *testing.T) {
	if _, err := Open(bytes.NewReader([]byte("RIFF....AVI LIST")), 16); err == nil {
		t.Fatal("expected an error")
	}
}

func TestAssTime(t *testing.T) {
	cases := map[time.Duration]string{
		0:                                    "0:00:00.00",
		1004 * time.Millisecond:              "0:00:01.00",
		1005 * time.Millisecond:              "0:00:01.01",
		3723*time.Second + 450*time.Millisecond: "1:02:03.45",
	}
	for d, want := range cases {
		if got := assTime(d); got != want {
			t.Errorf("assTime(%v) = %s, want %s", d, got, want)
		}
	}
}

func buildTextMKV() []byte {
	sub := bytes.Join([][]byte{
		uintElem(idTrackNumber, 1),
		uintElem(idTrackType, trackTypeSubtitle),
		elem(idCodecID, []byte("S_TEXT/UTF8")),
		elem(idLanguage, []byte("eng")),
	}, nil)
	tracks := elem(idTracks, elem(idTrackEntry, sub))
	info := elem(idInfo, uintElem(idTimestampScale, 1000000))
	group := func(rel int16, dur uint64, text string) []byte {
		return elem(idBlockGroup, elem(idBlock, block(1, rel, []byte(text))), uintElem(idBlockDuration, dur))
	}
	cluster := elem(idCluster, uintElem(idTimestamp, 0),
		group(1000, 2000, "Hello <i>world</i>\nsecond"),
		group(4000, 1000, "Tom & {Jerry}"),
		group(6000, 1000, `<font color="#ff0000">Red</font> text`))
	ebml := elem(idEBML, elem(0x4282, []byte("matroska")))
	return append(ebml, elem(idSegment, info, tracks, cluster)...)
}

func TestTextTrack(t *testing.T) {
	data := buildTextMKV()
	f, err := Open(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	tr, err := f.Track(0)
	if err != nil {
		t.Fatal(err)
	}
	if tr.Format != "srt" || !tr.IsText() || tr.IsASS() || !tr.Supported() || tr.Lang != "en" {
		t.Fatalf("track = %+v", *tr)
	}
	events, err := f.Events(context.Background(), 0, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	out := tr.ASS(events)
	if !strings.Contains(out, "[V4+ Styles]") || !strings.Contains(out, "Style: Default,") {
		t.Fatalf("no default style in the header:\n%s", out)
	}
	want := []string{
		`Dialogue: 0,0:00:01.00,0:00:03.00,Default,,0,0,0,,Hello {\i1}world{\i0}\Nsecond`,
		`Dialogue: 0,0:00:04.00,0:00:05.00,Default,,0,0,0,,Tom & \{Jerry\}`,
		`Dialogue: 0,0:00:06.00,0:00:07.00,Default,,0,0,0,,{\c&H0000ff&}Red{\c} text`,
	}
	got := lines(out)
	if len(got) != len(want) {
		t.Fatalf("got %d lines:\n%s", len(got), strings.Join(got, "\n"))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d:\n got  %s\n want %s", i, got[i], want[i])
		}
	}
}

func TestTextTrackSRT(t *testing.T) {
	data := buildTextMKV()
	f, err := Open(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	tr, _ := f.Track(0)
	// a window that covers only the second line
	events, err := f.Events(context.Background(), 0, 3500*time.Millisecond, 5500*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	want := "1\n00:00:04,000 --> 00:00:05,000\nTom & {Jerry}\n\n"
	if got := tr.SRT(events); got != want {
		t.Fatalf("SRT = %q, want %q", got, want)
	}

	events, err = f.Events(context.Background(), 0, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	got := tr.SRT(events)
	if !strings.HasPrefix(got, "1\n00:00:01,000 --> 00:00:03,000\nHello <i>world</i>\nsecond\n\n2\n") {
		t.Fatalf("SRT = %q", got)
	}
}

func TestSrtToASS(t *testing.T) {
	cases := map[string]string{
		"{\\an8}Top <b>bold</b>":                    `{\an8}Top {\b1}bold{\b0}`,
		"a &lt;b&gt; &amp;lt;":                      `a <b> &lt;`,
		"one\r\ntwo":                                `one\Ntwo`,
		"  \n":                                      ``,
		`<font face="X" size="3">x</font>`:          `x{\c}`,
	}
	for in, want := range cases {
		if got := srtToASS(in); got != want {
			t.Errorf("srtToASS(%q) = %q, want %q", in, got, want)
		}
	}
}

func buildAttachmentMKV(font []byte) []byte {
	sub := bytes.Join([][]byte{
		uintElem(idTrackNumber, 1),
		uintElem(idTrackType, trackTypeSubtitle),
		elem(idCodecID, []byte("S_TEXT/UTF8")),
	}, nil)
	tracks := elem(idTracks, elem(idTrackEntry, sub))
	info := elem(idInfo, uintElem(idTimestampScale, 1000000))
	file := func(name, mediaType string, data []byte) []byte {
		return elem(idAttachedFile,
			elem(0xBF, []byte{1, 2, 3, 4}), // a CRC-32 element, as ffmpeg writes it
			elem(idFileName, []byte(name)),
			elem(idFileMediaType, []byte(mediaType)),
			uintElem(0x46AE, 7), // FileUID
			elem(idFileData, data))
	}
	attachments := elem(idAttachments,
		elem(0xBF, []byte{0, 0, 0, 0}),
		file("Tahoma.ttf", "application/x-truetype-font", font),
		file("cover.png", "image/png", bytes.Repeat([]byte{1}, 100)),
		file("Georgia.ttc", "", []byte("ttc-data")),
		file("noext", "application/vnd.ms-opentype", []byte("otf-data")))
	cluster := elem(idCluster, uintElem(idTimestamp, 0))
	ebml := elem(idEBML, elem(0x4282, []byte("matroska")))
	return append(ebml, elem(idSegment, info, tracks, attachments, cluster)...)
}

func TestAttachments(t *testing.T) {
	// more than a megabyte: such data is skipped with Seek, not read through
	font := bytes.Repeat([]byte{0xAB}, 1<<20+5)
	data := buildAttachmentMKV(font)
	f, err := Open(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	list, err := f.Attachments()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 4 {
		t.Fatalf("attachments = %+v", list)
	}
	wantNames := []string{"Tahoma.ttf", "cover.png", "Georgia.ttc", "noext"}
	wantFont := []bool{true, false, true, true}
	for i, a := range list {
		if a.Index != i || a.Name != wantNames[i] || a.IsFont() != wantFont[i] {
			t.Errorf("attachment %d = %+v (font %v)", i, a, a.IsFont())
		}
	}
	if list[0].Size != int64(len(font)) || list[1].Size != 100 {
		t.Errorf("sizes = %d, %d", list[0].Size, list[1].Size)
	}
	if list[2].ContentType() != "font/collection" || list[0].ContentType() != "font/ttf" {
		t.Errorf("content types: %s, %s", list[0].ContentType(), list[2].ContentType())
	}

	got, err := f.ReadAttachment(0)
	if err != nil || !bytes.Equal(got, font) {
		t.Fatalf("font data differs (err %v, %d bytes)", err, len(got))
	}
	if got, err = f.ReadAttachment(3); err != nil || string(got) != "otf-data" {
		t.Fatalf("attachment 3 = %q, %v", got, err)
	}
	if _, err = f.ReadAttachment(9); err != ErrNoAttachment {
		t.Fatalf("err = %v, want ErrNoAttachment", err)
	}
}

func TestNoAttachments(t *testing.T) {
	f := open(t, "")
	list, err := f.Attachments()
	if err != nil || len(list) != 0 {
		t.Fatalf("attachments = %+v, %v", list, err)
	}
}

func TestOpenAttachment(t *testing.T) {
	font := make([]byte, 3000)
	for i := range font {
		font[i] = byte(i % 251)
	}
	data := buildAttachmentMKV(font)
	f, err := Open(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	rd, a, err := f.OpenAttachment(0)
	if err != nil || a.Name != "Tahoma.ttf" {
		t.Fatalf("OpenAttachment: %+v, %v", a, err)
	}
	// a Range-like read: seek into the middle, read a piece, seek from the end
	if _, err := rd.Seek(1000, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 500)
	if _, err := io.ReadFull(rd, buf); err != nil || !bytes.Equal(buf, font[1000:1500]) {
		t.Fatalf("piece differs: %v", err)
	}
	if _, err := rd.Seek(-100, io.SeekEnd); err != nil {
		t.Fatal(err)
	}
	tail, err := io.ReadAll(rd)
	if err != nil || !bytes.Equal(tail, font[len(font)-100:]) {
		t.Fatalf("tail differs: %v (%d bytes)", err, len(tail))
	}
	// the whole attachment, and nothing after its end
	if _, err := rd.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	all, err := io.ReadAll(rd)
	if err != nil || !bytes.Equal(all, font) {
		t.Fatalf("data differs: %v (%d bytes)", err, len(all))
	}
	if _, _, err := f.OpenAttachment(9); err != ErrNoAttachment {
		t.Fatalf("err = %v, want ErrNoAttachment", err)
	}
}
