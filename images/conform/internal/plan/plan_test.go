package plan

import (
	"slices"
	"strings"
	"testing"

	"github.com/edjeffreys/conform/internal/config"
	"github.com/edjeffreys/conform/internal/media"
)

func profile() config.Profile {
	return config.Profile{
		Container: "mkv",
		Video: config.VideoRules{
			Codecs: []string{"hevc"}, MaxHeight: 1080,
			ScaleFilter: "scale=-2:{height}",
			Encoder:     config.Encoder{Name: "libx265", Options: map[string]string{"crf": "28"}},
		},
		Audio: config.AudioRules{
			Languages: []string{"eng", "und"}, Codecs: []string{"aac", "eac3"}, MaxChannels: 6,
			Encoder: config.Encoder{Name: "eac3", Options: map[string]string{"b": "640k"}},
		},
		Subtitles: config.SubtitleRules{Languages: []string{"eng"}, Codecs: []string{"subrip"}},
	}
}

func file(container string, streams ...media.Stream) *media.File {
	for i := range streams {
		streams[i].Index = i
	}
	return &media.File{Path: "/x.mkv", Container: container, Duration: 60, Size: 1000, Streams: streams}
}

func vid(codec string, height int) media.Stream {
	return media.Stream{Type: media.Video, Codec: codec, Height: height, Width: height * 16 / 9, Language: "und"}
}
func aud(codec, lang string, ch int) media.Stream {
	return media.Stream{Type: media.Audio, Codec: codec, Language: lang, Channels: ch}
}
func sub(codec, lang string) media.Stream {
	return media.Stream{Type: media.Subtitle, Codec: codec, Language: lang}
}

func TestActions(t *testing.T) {
	tests := []struct {
		name string
		file *media.File
		want Action
	}{
		{"conformant is left alone", file("mkv", vid("hevc", 1080), aud("aac", "eng", 2)), ActionNone},
		{"wrong video codec", file("mkv", vid("h264", 1080), aud("aac", "eng", 2)), ActionTranscode},
		{"too tall", file("mkv", vid("hevc", 2160), aud("aac", "eng", 2)), ActionTranscode},
		{"wrong audio codec", file("mkv", vid("hevc", 1080), aud("truehd", "eng", 8)), ActionTranscode},
		{"container alone is a remux", file("mp4", vid("hevc", 1080), aud("aac", "eng", 2)), ActionRemux},
		{"dropping a stream alone is a remux",
			file("mkv", vid("hevc", 1080), aud("aac", "eng", 2), aud("aac", "fre", 2)), ActionRemux},
		{"a file with no video is never touched", file("mkv", aud("mp3", "eng", 2)), ActionNone},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Build(tc.file, profile()).Action; got != tc.want {
				t.Errorf("got %s, want %s", got, tc.want)
			}
		})
	}
}

// The output of a transcode must itself be conformant, or a scheduled run
// re-encodes the same files forever. This is the property the runner verifies
// at execution time; here it is checked against the planner directly.
func TestConvergence(t *testing.T) {
	nonconformant := file("avi",
		vid("h264", 2160),
		aud("truehd", "eng", 8), aud("dts", "fre", 6),
		sub("subrip", "eng"), sub("hdmv_pgs_subtitle", "eng"),
	)
	p := Build(nonconformant, profile())
	if p.Action != ActionTranscode {
		t.Fatalf("expected a transcode, got %s", p.Action)
	}

	// Model what ffmpeg would emit for this plan.
	after := file("mkv", vid("hevc", 1080), aud("eac3", "eng", 6), sub("subrip", "eng"))
	if got := Build(after, profile()).Action; got != ActionNone {
		t.Fatalf("the result of a plan still needs work (%s) — this would loop", got)
	}
}

// A language filter matching nothing must not leave a silent file.
func TestAudioLanguageFilterNeverEmptiesAFile(t *testing.T) {
	f := file("mkv", vid("hevc", 1080), aud("aac", "jpn", 2))
	p := Build(f, profile())
	if p.Action != ActionNone {
		t.Errorf("got %s, want none — the only audio track must be kept", p.Action)
	}
	if len(p.Dropped) != 0 {
		t.Errorf("dropped %d streams; the file would have no audio", len(p.Dropped))
	}
}

// Cover art is a video stream to ffprobe. Judging it as one would mark every
// file carrying a poster as needing a re-encode of a single still frame.
func TestCoverArtIsCarriedNotJudged(t *testing.T) {
	art := media.Stream{Type: media.Video, Codec: "mjpeg", Height: 1500, AttachedPic: true}
	f := file("mkv", vid("hevc", 1080), art, aud("aac", "eng", 2))
	p := Build(f, profile())
	if p.Action != ActionNone {
		t.Fatalf("got %s, want none", p.Action)
	}
	if !slices.ContainsFunc(p.Streams, func(s StreamPlan) bool { return s.Source == 1 && s.Codec == Copy }) {
		t.Error("cover art was not carried through")
	}
}

func TestDownmixSetsChannelCount(t *testing.T) {
	f := file("mkv", vid("hevc", 1080), aud("aac", "eng", 8))
	p := Build(f, profile())
	for _, s := range p.Streams {
		if s.Type == media.Audio && s.Options["ac"] != "6" {
			t.Errorf("ac = %q, want 6", s.Options["ac"])
		}
	}
}

// Every codec option must carry a full stream specifier, or a file with two
// video tracks has the profile applied to both.
func TestFFmpegArgsAreStreamSpecific(t *testing.T) {
	f := file("mkv", vid("h264", 2160), aud("truehd", "eng", 8), sub("subrip", "eng"))
	args := strings.Join(Build(f, profile()).FFmpegArgs("in.mkv", "out.mkv"), " ")

	for _, want := range []string{
		"-map 0:0", "-map 0:1", "-map 0:2",
		"-c:v:0 libx265", "-crf:v:0 28", "-filter:v:0 scale=-2:1080",
		"-c:a:0 eac3", "-ac:a:0 6", "-b:a:0 640k",
		"-c:s:0 copy", "-map_chapters 0",
	} {
		if !strings.Contains(args, want) {
			t.Errorf("missing %q in:\n%s", want, args)
		}
	}
}

// Options come from a map; without sorting, the command line would differ
// between runs and no two invocations would be reproducible.
func TestFFmpegArgsAreDeterministic(t *testing.T) {
	f := file("mkv", vid("h264", 1080), aud("truehd", "eng", 8))
	first := strings.Join(Build(f, profile()).FFmpegArgs("in", "out"), " ")
	for range 20 {
		if got := strings.Join(Build(f, profile()).FFmpegArgs("in", "out"), " "); got != first {
			t.Fatalf("argument order varies between builds:\n%s\n%s", first, got)
		}
	}
}
