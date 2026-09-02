// Package media turns a file on disk into the observed half of the
// reconcile: a normalised description of its container and streams.
package media

import (
	"strings"
	"time"
)

// Stream types, matching ffprobe's codec_type.
const (
	Video    = "video"
	Audio    = "audio"
	Subtitle = "subtitle"
)

// File is the observed state of one media file.
type File struct {
	Path      string    `json:"path"`
	Size      int64     `json:"size"`
	ModTime   time.Time `json:"modTime"`
	Container string    `json:"container"`
	Duration  float64   `json:"duration"`
	Streams   []Stream  `json:"streams"`
}

// Stream is one elementary stream. Fields not applicable to a stream's type
// are left zero rather than omitted, so the planner never has to type-assert.
type Stream struct {
	Index    int    `json:"index"`
	Type     string `json:"type"`
	Codec    string `json:"codec"`
	Profile  string `json:"profile,omitempty"`
	Language string `json:"language"`
	Title    string `json:"title,omitempty"`

	Width    int   `json:"width,omitempty"`
	Height   int   `json:"height,omitempty"`
	Channels int   `json:"channels,omitempty"`
	BitRate  int64 `json:"bitRate,omitempty"`

	Default bool `json:"default,omitempty"`
	Forced  bool `json:"forced,omitempty"`

	// AttachedPic marks cover art, which ffprobe reports as a video stream.
	// Treating it as video would make every file with a poster look like it
	// needs a re-encode, so the planner skips these entirely.
	AttachedPic bool `json:"attachedPic,omitempty"`
}

// Of returns the streams of a given type, in file order.
func (f *File) Of(kind string) []Stream {
	var out []Stream
	for _, s := range f.Streams {
		if s.Type == kind {
			out = append(out, s)
		}
	}
	return out
}

// containerAliases maps ffprobe's comma-joined format_name lists onto the
// single name used in config. ffprobe reports one demuxer for several
// containers ("matroska,webm", "mov,mp4,m4a,3gp,3g2,mj2"), so the raw value
// can never be compared to a config string directly.
var containerAliases = []struct {
	needle string
	name   string
}{
	{"matroska", "mkv"},
	{"mp4", "mp4"},
	{"avi", "avi"},
	{"asf", "wmv"},
	{"mpegts", "ts"},
	{"flv", "flv"},
	{"webm", "webm"},
}

func normaliseContainer(formatName string) string {
	for _, a := range containerAliases {
		for _, part := range strings.Split(formatName, ",") {
			if part == a.needle {
				return a.name
			}
		}
	}
	// Fall back to the first reported demuxer rather than guessing, so an
	// unrecognised container shows up in plan output instead of silently
	// matching or mismatching the profile.
	if i := strings.Index(formatName, ","); i > 0 {
		return formatName[:i]
	}
	return formatName
}

// Ext is the file extension a container should be written with.
func Ext(container string) string {
	return "." + container
}
