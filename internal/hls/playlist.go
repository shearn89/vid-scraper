// Package hls downloads HTTP Live Streaming playlists natively.
//
// An M3U8 playlist is a line-oriented text format whose segments, fetched in
// order, form a playable byte stream. That is what makes native HLS support
// tractable without a muxer -- and equally, why DASH is not supported: DASH
// ships audio and video as separate representations that must be muxed, which
// concatenation cannot do.
package hls

import (
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Encryption methods that can appear on EXT-X-KEY.
const (
	MethodNone      = "NONE"
	MethodAES128    = "AES-128"
	MethodSampleAES = "SAMPLE-AES"
)

// ErrNotPlaylist reports input that is not an M3U8 document.
var ErrNotPlaylist = errors.New("not an m3u8 playlist")

// ByteRange is an EXT-X-BYTERANGE value.
type ByteRange struct {
	Length int64
	Offset int64
}

// Header renders the range as an HTTP Range header value.
func (b ByteRange) Header() string {
	return fmt.Sprintf("bytes=%d-%d", b.Offset, b.Offset+b.Length-1)
}

// Key is the decryption configuration applying to a run of segments.
type Key struct {
	Method    string
	URI       string
	IV        []byte
	KeyFormat string
}

// IsDRM reports whether the key names a DRM system rather than plain AES-128.
// These are refused rather than attempted: the content keys are not obtainable.
func (k *Key) IsDRM() bool {
	if k == nil {
		return false
	}
	if k.KeyFormat != "" && !strings.EqualFold(k.KeyFormat, "identity") {
		return true
	}
	return strings.EqualFold(k.Method, MethodSampleAES)
}

// Variant is one EXT-X-STREAM-INF entry in a master playlist.
type Variant struct {
	URI        string
	Bandwidth  int
	Width      int
	Height     int
	Codecs     string
	AudioGroup string
}

// Rendition is one EXT-X-MEDIA entry, typically an alternate audio track.
type Rendition struct {
	Type    string
	GroupID string
	Name    string
	URI     string
	Default bool
}

// Segment is one media segment.
type Segment struct {
	URI      string
	Duration float64
	Key      *Key
	Seq      uint64
	Range    *ByteRange
}

// Master is a playlist of variant streams.
type Master struct {
	Variants   []Variant
	Renditions []Rendition
}

// Media is a playlist of segments.
type Media struct {
	Segments       []Segment
	InitURI        string // EXT-X-MAP, present for fMP4 streams
	InitRange      *ByteRange
	TargetDuration float64
	MediaSequence  uint64
	// Live is true when the playlist carries no EXT-X-ENDLIST. Live streams
	// are refused: they have no defined end to download to.
	Live bool
}

// Playlist is either a master or a media playlist.
type Playlist struct {
	IsMaster bool
	Master   *Master
	Media    *Media
}

// Parse reads an M3U8 document.
func Parse(data []byte) (*Playlist, error) {
	lines := splitLines(string(data))
	if len(lines) == 0 || !strings.HasPrefix(strings.TrimSpace(lines[0]), "#EXTM3U") {
		return nil, ErrNotPlaylist
	}

	var (
		master     = &Master{}
		media      = &Media{Live: true}
		pendingVar *Variant
		pendingSeg *Segment
		pendingRng *ByteRange
		currentKey *Key
		seq        uint64
	)

	for _, raw := range lines[1:] {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}

		if !strings.HasPrefix(line, "#") {
			// A bare line is a URI belonging to whatever tag preceded it.
			switch {
			case pendingVar != nil:
				pendingVar.URI = line
				master.Variants = append(master.Variants, *pendingVar)
				pendingVar = nil
			case pendingSeg != nil:
				pendingSeg.URI = line
				pendingSeg.Key = currentKey
				pendingSeg.Seq = seq
				pendingSeg.Range = pendingRng
				media.Segments = append(media.Segments, *pendingSeg)
				pendingSeg, pendingRng = nil, nil
				seq++
			}
			continue
		}

		tag, value, _ := strings.Cut(line, ":")
		switch tag {
		case "#EXT-X-STREAM-INF":
			a := parseAttrs(value)
			v := Variant{
				Bandwidth:  atoiDefault(a["BANDWIDTH"], 0),
				Codecs:     a["CODECS"],
				AudioGroup: a["AUDIO"],
			}
			if avg := atoiDefault(a["AVERAGE-BANDWIDTH"], 0); v.Bandwidth == 0 {
				v.Bandwidth = avg
			}
			v.Width, v.Height = parseResolution(a["RESOLUTION"])
			pendingVar = &v

		case "#EXT-X-MEDIA":
			a := parseAttrs(value)
			master.Renditions = append(master.Renditions, Rendition{
				Type:    a["TYPE"],
				GroupID: a["GROUP-ID"],
				Name:    a["NAME"],
				URI:     a["URI"],
				Default: strings.EqualFold(a["DEFAULT"], "YES"),
			})

		case "#EXTINF":
			durStr, _, _ := strings.Cut(value, ",")
			dur, _ := strconv.ParseFloat(strings.TrimSpace(durStr), 64)
			pendingSeg = &Segment{Duration: dur}

		case "#EXT-X-KEY":
			key, err := parseKey(value)
			if err != nil {
				return nil, err
			}
			currentKey = key

		case "#EXT-X-MAP":
			a := parseAttrs(value)
			media.InitURI = a["URI"]
			if r := a["BYTERANGE"]; r != "" {
				media.InitRange = parseByteRange(r, 0)
			}

		case "#EXT-X-BYTERANGE":
			var prevEnd int64
			if n := len(media.Segments); n > 0 && media.Segments[n-1].Range != nil {
				last := media.Segments[n-1].Range
				prevEnd = last.Offset + last.Length
			}
			pendingRng = parseByteRange(value, prevEnd)

		case "#EXT-X-MEDIA-SEQUENCE":
			if n, err := strconv.ParseUint(strings.TrimSpace(value), 10, 64); err == nil {
				media.MediaSequence, seq = n, n
			}

		case "#EXT-X-TARGETDURATION":
			media.TargetDuration, _ = strconv.ParseFloat(strings.TrimSpace(value), 64)

		case "#EXT-X-ENDLIST":
			media.Live = false
		}
	}
	if len(master.Variants) > 0 {
		return &Playlist{IsMaster: true, Master: master}, nil
	}
	if len(media.Segments) == 0 {
		return nil, fmt.Errorf("%w: no variants or segments found", ErrNotPlaylist)
	}
	return &Playlist{Media: media}, nil
}

// parseKey reads an EXT-X-KEY attribute list.
func parseKey(value string) (*Key, error) {
	a := parseAttrs(value)
	k := &Key{
		Method:    strings.ToUpper(a["METHOD"]),
		URI:       a["URI"],
		KeyFormat: a["KEYFORMAT"],
	}
	if k.Method == MethodNone || k.Method == "" {
		return nil, nil
	}
	if ivStr := a["IV"]; ivStr != "" {
		iv, err := hex.DecodeString(strings.TrimPrefix(strings.TrimPrefix(ivStr, "0x"), "0X"))
		if err != nil {
			return nil, fmt.Errorf("bad EXT-X-KEY IV %q: %w", ivStr, err)
		}
		k.IV = iv
	}
	return k, nil
}

// parseAttrs splits a comma-separated attribute list, honouring quoted values
// so that commas inside quotes (common in CODECS) do not split a field.
func parseAttrs(s string) map[string]string {
	out := make(map[string]string)
	var (
		field    strings.Builder
		inQuotes bool
	)
	flush := func() {
		f := strings.TrimSpace(field.String())
		field.Reset()
		if f == "" {
			return
		}
		k, v, ok := strings.Cut(f, "=")
		if !ok {
			return
		}
		out[strings.ToUpper(strings.TrimSpace(k))] = strings.Trim(strings.TrimSpace(v), `"`)
	}
	for _, r := range s {
		switch {
		case r == '"':
			inQuotes = !inQuotes
			field.WriteRune(r)
		case r == ',' && !inQuotes:
			flush()
		default:
			field.WriteRune(r)
		}
	}
	flush()
	return out
}

// parseByteRange reads an "n[@o]" value, defaulting the offset to the end of
// the previous range as the specification requires.
func parseByteRange(s string, prevEnd int64) *ByteRange {
	s = strings.Trim(strings.TrimSpace(s), `"`)
	lenStr, offStr, hasOff := strings.Cut(s, "@")
	length, err := strconv.ParseInt(strings.TrimSpace(lenStr), 10, 64)
	if err != nil || length <= 0 {
		return nil
	}
	br := &ByteRange{Length: length, Offset: prevEnd}
	if hasOff {
		if off, err := strconv.ParseInt(strings.TrimSpace(offStr), 10, 64); err == nil {
			br.Offset = off
		}
	}
	return br
}

func parseResolution(s string) (int, int) {
	w, h, ok := strings.Cut(strings.TrimSpace(s), "x")
	if !ok {
		return 0, 0
	}
	return atoiDefault(w, 0), atoiDefault(h, 0)
}

func atoiDefault(s string, def int) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return def
	}
	return n
}

// splitLines handles CRLF and a leading byte-order mark, both of which appear
// in playlists served by real CDNs.
func splitLines(s string) []string {
	s = strings.TrimPrefix(s, "\uFEFF")
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.Split(s, "\n")
}
