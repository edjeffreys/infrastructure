package media

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// Prober runs ffprobe. The binary is a field so a caller can point at a
// specific build; tests substitute a fixture reader instead.
type Prober struct {
	Binary string
}

func NewProber(binary string) *Prober {
	if binary == "" {
		binary = "ffprobe"
	}
	return &Prober{Binary: binary}
}

type rawProbe struct {
	Format struct {
		FormatName string `json:"format_name"`
		Duration   string `json:"duration"`
		Size       string `json:"size"`
	} `json:"format"`
	Streams []rawStream `json:"streams"`
}

type rawStream struct {
	Index       int               `json:"index"`
	CodecName   string            `json:"codec_name"`
	CodecType   string            `json:"codec_type"`
	Profile     string            `json:"profile"`
	Width       int               `json:"width"`
	Height      int               `json:"height"`
	Channels    int               `json:"channels"`
	BitRate     string            `json:"bit_rate"`
	Tags        map[string]string `json:"tags"`
	Disposition map[string]int    `json:"disposition"`
}

// Probe describes a single file. The returned File carries the size and mtime
// observed at probe time, which is what the state cache keys on.
func (p *Prober) Probe(ctx context.Context, path string) (*File, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}

	cmd := exec.CommandContext(ctx, p.Binary,
		"-v", "error",
		"-print_format", "json",
		"-show_format",
		"-show_streams",
		path,
	)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("ffprobe %s: %w: %s", path, err, strings.TrimSpace(stderr.String()))
	}

	var raw rawProbe
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, fmt.Errorf("parse ffprobe output for %s: %w", path, err)
	}

	f := &File{
		Path:      path,
		Size:      info.Size(),
		ModTime:   info.ModTime(),
		Container: normaliseContainer(raw.Format.FormatName),
		Duration:  parseFloat(raw.Format.Duration),
	}
	for _, rs := range raw.Streams {
		if rs.CodecType != Video && rs.CodecType != Audio && rs.CodecType != Subtitle {
			continue // data and attachment streams are carried, never planned
		}
		f.Streams = append(f.Streams, Stream{
			Index:       rs.Index,
			Type:        rs.CodecType,
			Codec:       rs.CodecName,
			Profile:     rs.Profile,
			Language:    language(rs.Tags),
			Title:       rs.Tags["title"],
			Width:       rs.Width,
			Height:      rs.Height,
			Channels:    rs.Channels,
			BitRate:     parseInt(rs.BitRate),
			Default:     rs.Disposition["default"] == 1,
			Forced:      rs.Disposition["forced"] == 1,
			AttachedPic: rs.Disposition["attached_pic"] == 1,
		})
	}
	return f, nil
}

// language normalises the stream language tag. An absent tag becomes "und",
// the ISO 639-2 code for undetermined, so profile rules only ever compare
// against real codes and a missing tag can be matched explicitly.
func language(tags map[string]string) string {
	l := strings.ToLower(strings.TrimSpace(tags["language"]))
	if l == "" {
		return "und"
	}
	return l
}

func parseFloat(s string) float64 {
	v, _ := strconv.ParseFloat(s, 64)
	return v
}

func parseInt(s string) int64 {
	v, _ := strconv.ParseInt(s, 10, 64)
	return v
}
