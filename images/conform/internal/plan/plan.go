// Package plan diffs a file's observed state against a profile and decides
// what, if anything, has to change. It performs no I/O: the same inputs
// always produce the same plan, which is what makes `conform plan` a
// trustworthy preview of `conform apply`.
package plan

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/edjeffreys/conform/internal/config"
	"github.com/edjeffreys/conform/internal/media"
)

type Action string

const (
	// ActionNone means the file already satisfies the profile.
	ActionNone Action = "none"
	// ActionRemux rewrites the container and stream selection without
	// re-encoding — seconds per file, no GPU time.
	ActionRemux Action = "remux"
	// ActionTranscode re-encodes at least one stream.
	ActionTranscode Action = "transcode"
)

// Copy is the sentinel codec meaning "pass this stream through untouched".
const Copy = "copy"

type Plan struct {
	File      *media.File
	Action    Action
	Container string
	// InputArgs are hardware-decode flags that must precede -i. They are set
	// only when a video re-encode is actually planned, so a remux never pays
	// the cost of spinning up a hardware context it will not use.
	InputArgs  []string
	OutputArgs []string
	Streams    []StreamPlan
	Dropped    []Dropped
	Reasons    []string
}

type StreamPlan struct {
	Source int
	Type   string
	// Codec is Copy or an encoder name.
	Codec   string
	Options map[string]string
	Filter  string
	Reason  string
}

type Dropped struct {
	Source int
	Type   string
	Reason string
}

func (p *Plan) transcodes() bool {
	for _, s := range p.Streams {
		if s.Codec != Copy {
			return true
		}
	}
	return false
}

// Build compares f against prof and returns the plan to bring it into line.
func Build(f *media.File, prof config.Profile) *Plan {
	p := &Plan{File: f, Container: prof.Container, OutputArgs: prof.OutputArgs}

	// Cover art is reported as a video stream. It is carried through
	// untouched and never measured against the video rules — judging it as
	// video would mark every file with an embedded poster as needing a
	// re-encode of a single still frame.
	var realVideo []media.Stream
	for _, s := range f.Streams {
		if s.Type == media.Video && s.AttachedPic {
			p.Streams = append(p.Streams, StreamPlan{Source: s.Index, Type: s.Type, Codec: Copy, Reason: "cover art"})
			continue
		}
		if s.Type == media.Video {
			realVideo = append(realVideo, s)
		}
	}

	for _, s := range realVideo {
		p.Streams = append(p.Streams, planVideo(s, prof.Video, p))
	}
	p.planAudio(f.Of(media.Audio), prof.Audio)
	p.planSubtitles(f.Of(media.Subtitle), prof.Subtitles)

	// Sort by source index so output stream order matches input order; the
	// video/audio/subtitle passes above append out of order.
	slices.SortStableFunc(p.Streams, func(a, b StreamPlan) int { return a.Source - b.Source })

	if f.Container != prof.Container {
		p.Reasons = append(p.Reasons, fmt.Sprintf("container %s is not %s", f.Container, prof.Container))
	}

	switch {
	case len(realVideo) == 0:
		// Not a video file, or one this tool cannot reason about. Rewriting
		// it on container grounds alone would be a change with no benefit.
		p.Action = ActionNone
		p.Reasons = []string{"no video stream"}
	case p.transcodes():
		p.Action = ActionTranscode
	case f.Container != prof.Container || len(p.Dropped) > 0:
		p.Action = ActionRemux
	default:
		p.Action = ActionNone
	}
	return p
}

func planVideo(s media.Stream, rules config.VideoRules, p *Plan) StreamPlan {
	sp := StreamPlan{Source: s.Index, Type: s.Type, Codec: Copy}

	var why []string
	if len(rules.Codecs) > 0 && !slices.Contains(rules.Codecs, s.Codec) {
		why = append(why, fmt.Sprintf("codec %s not in %s", s.Codec, strings.Join(rules.Codecs, "/")))
	}
	downscale := rules.MaxHeight > 0 && s.Height > rules.MaxHeight
	if downscale {
		why = append(why, fmt.Sprintf("height %d exceeds %d", s.Height, rules.MaxHeight))
	}
	if len(why) == 0 {
		return sp
	}

	sp.Codec = rules.Encoder.Name
	sp.Options = copyOptions(rules.Encoder.Options)
	sp.Reason = strings.Join(why, ", ")
	p.InputArgs = rules.Encoder.InputArgs
	if downscale {
		sp.Filter = strings.NewReplacer(
			"{height}", strconv.Itoa(rules.MaxHeight),
			"{width}", "-2",
		).Replace(rules.ScaleFilter)
	}
	p.Reasons = append(p.Reasons, "video: "+sp.Reason)
	return sp
}

func (p *Plan) planAudio(streams []media.Stream, rules config.AudioRules) {
	// Decide language keeps first. A language filter that matches nothing
	// would leave a silent file, so in that case the filter is ignored
	// entirely and every stream is kept — a mis-tagged library should not be
	// quietly stripped of its audio.
	keep := make([]bool, len(streams))
	kept := 0
	for i, s := range streams {
		keep[i] = len(rules.Languages) == 0 || slices.Contains(rules.Languages, s.Language)
		if keep[i] {
			kept++
		}
	}
	if kept == 0 && len(streams) > 0 {
		for i := range keep {
			keep[i] = true
		}
		p.Reasons = append(p.Reasons, "audio: language filter matched no stream, keeping all")
	}

	for i, s := range streams {
		if !keep[i] {
			p.Dropped = append(p.Dropped, Dropped{Source: s.Index, Type: s.Type,
				Reason: fmt.Sprintf("language %s not kept", s.Language)})
			continue
		}

		sp := StreamPlan{Source: s.Index, Type: s.Type, Codec: Copy}
		var why []string
		if len(rules.Codecs) > 0 && !slices.Contains(rules.Codecs, s.Codec) {
			why = append(why, fmt.Sprintf("codec %s not in %s", s.Codec, strings.Join(rules.Codecs, "/")))
		}
		downmix := rules.MaxChannels > 0 && s.Channels > rules.MaxChannels
		if downmix {
			why = append(why, fmt.Sprintf("%d channels exceeds %d", s.Channels, rules.MaxChannels))
		}
		if len(why) > 0 {
			sp.Codec = rules.Encoder.Name
			sp.Options = copyOptions(rules.Encoder.Options)
			sp.Reason = strings.Join(why, ", ")
			if downmix {
				sp.Options["ac"] = strconv.Itoa(rules.MaxChannels)
			}
			p.Reasons = append(p.Reasons, fmt.Sprintf("audio:%d: %s", s.Index, sp.Reason))
		}
		p.Streams = append(p.Streams, sp)
	}
}

func (p *Plan) planSubtitles(streams []media.Stream, rules config.SubtitleRules) {
	for _, s := range streams {
		switch {
		case len(rules.Languages) > 0 && !slices.Contains(rules.Languages, s.Language):
			p.Dropped = append(p.Dropped, Dropped{Source: s.Index, Type: s.Type,
				Reason: fmt.Sprintf("language %s not kept", s.Language)})
		case len(rules.Codecs) > 0 && !slices.Contains(rules.Codecs, s.Codec):
			// Dropped, never converted: turning an image-based format into a
			// text one needs OCR, and guessing at it silently corrupts
			// subtitles rather than failing visibly.
			p.Dropped = append(p.Dropped, Dropped{Source: s.Index, Type: s.Type,
				Reason: fmt.Sprintf("codec %s not in %s", s.Codec, strings.Join(rules.Codecs, "/"))})
		default:
			p.Streams = append(p.Streams, StreamPlan{Source: s.Index, Type: s.Type, Codec: Copy})
		}
	}
}

func copyOptions(in map[string]string) map[string]string {
	out := make(map[string]string, len(in)+1)
	for k, v := range in {
		out[k] = v
	}
	return out
}
