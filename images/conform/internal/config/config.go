// Package config is the desired half of the reconcile: profiles describing
// what a conformant file looks like, and the libraries they apply to.
package config

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Libraries []Library          `yaml:"libraries"`
	Profiles  map[string]Profile `yaml:"profiles"`
	Execution Execution          `yaml:"execution"`
}

type Library struct {
	Name    string `yaml:"name"`
	Path    string `yaml:"path"`
	Profile string `yaml:"profile"`
	// Extensions to consider. Defaults to DefaultExtensions.
	Extensions []string `yaml:"extensions"`
	// Exclude holds glob patterns matched against the path relative to Path.
	Exclude []string `yaml:"exclude"`
}

// Profile is the desired end state of a file. Every rule is a predicate on
// what is acceptable, never an instruction to act: a file already satisfying
// all of them is left alone, which is what makes repeated runs converge.
type Profile struct {
	Container string `yaml:"container"`
	// OutputArgs are appended verbatim before the output file — the escape
	// hatch for muxer flags no rule models, such as -max_muxing_queue_size.
	OutputArgs []string      `yaml:"outputArgs"`
	Video      VideoRules    `yaml:"video"`
	Audio      AudioRules    `yaml:"audio"`
	Subtitles  SubtitleRules `yaml:"subtitles"`
}

type VideoRules struct {
	// Codecs that are acceptable as-is. Empty accepts any codec.
	Codecs    []string `yaml:"codecs"`
	MaxHeight int      `yaml:"maxHeight"`
	Encoder   Encoder  `yaml:"encoder"`
	// ScaleFilter is templated with {height} and {width} when a downscale is
	// needed. It is configurable because the filter is encoder-specific:
	// software encoders want `scale`, QSV wants `scale_qsv` on frames that
	// never leave the GPU.
	ScaleFilter string `yaml:"scaleFilter"`
}

type AudioRules struct {
	// Languages to keep. Empty keeps every language.
	Languages   []string `yaml:"languages"`
	Codecs      []string `yaml:"codecs"`
	MaxChannels int      `yaml:"maxChannels"`
	Encoder     Encoder  `yaml:"encoder"`
}

type SubtitleRules struct {
	Languages []string `yaml:"languages"`
	// Codecs that are acceptable. A subtitle stream in any other codec is
	// dropped rather than converted: image-based formats cannot be turned
	// into text ones without OCR, and this tool will not silently guess.
	Codecs []string `yaml:"codecs"`
}

type Encoder struct {
	Name string `yaml:"name"`
	// Options become `-key value` after the encoder selection, in sorted key
	// order so a given profile always produces byte-identical arguments.
	Options map[string]string `yaml:"options"`
	// InputArgs are placed before -i, for hardware decode setup such as
	// `-hwaccel qsv`.
	InputArgs []string `yaml:"inputArgs"`
}

type Execution struct {
	TempDir  string `yaml:"tempDir"`
	StateDir string `yaml:"stateDir"`
	Workers  int    `yaml:"workers"`

	// MinDurationRatio rejects an output shorter than this fraction of the
	// source, which is how a truncated encode is caught: ffmpeg exits 0 after
	// writing a partial file more often than it reports an error.
	MinDurationRatio float64 `yaml:"minDurationRatio"`
	// MaxSizeRatio rejects an output larger than this fraction of the source.
	// A rejected file is excused rather than retried — see state.Record.
	MaxSizeRatio float64 `yaml:"maxSizeRatio"`
	// MaxFailures is how many times a file may fail before it is excused.
	MaxFailures int `yaml:"maxFailures"`

	FFmpeg  string `yaml:"ffmpeg"`
	FFprobe string `yaml:"ffprobe"`

	// Owner chowns replaced files. Needed on the shared NFS export, where
	// everything must stay uid/gid 3000 or the arr stack loses write access.
	Owner *Owner `yaml:"owner"`
}

type Owner struct {
	UID int `yaml:"uid"`
	GID int `yaml:"gid"`
}

var DefaultExtensions = []string{".mkv", ".mp4", ".avi", ".m4v", ".mov", ".wmv", ".ts", ".mpg", ".mpeg"}

// Load reads and validates a config file, filling in defaults.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true) // a typo in a rule name must not silently disable it
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	c.applyDefaults()
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) applyDefaults() {
	e := &c.Execution
	if e.Workers <= 0 {
		e.Workers = 1
	}
	if e.MinDurationRatio == 0 {
		e.MinDurationRatio = 0.98
	}
	if e.MaxSizeRatio == 0 {
		e.MaxSizeRatio = 1.0
	}
	if e.MaxFailures == 0 {
		e.MaxFailures = 3
	}
	if e.FFmpeg == "" {
		e.FFmpeg = "ffmpeg"
	}
	if e.FFprobe == "" {
		e.FFprobe = "ffprobe"
	}
	if e.StateDir == "" {
		e.StateDir = "."
	}

	for i := range c.Libraries {
		if len(c.Libraries[i].Extensions) == 0 {
			c.Libraries[i].Extensions = DefaultExtensions
		}
		for j, ext := range c.Libraries[i].Extensions {
			if !strings.HasPrefix(ext, ".") {
				c.Libraries[i].Extensions[j] = "." + ext
			}
			c.Libraries[i].Extensions[j] = strings.ToLower(c.Libraries[i].Extensions[j])
		}
	}

	for name, p := range c.Profiles {
		if p.Video.ScaleFilter == "" {
			p.Video.ScaleFilter = "scale=-2:{height}"
		}
		p.Container = strings.ToLower(p.Container)
		p.Video.Codecs = lowerAll(p.Video.Codecs)
		p.Audio.Codecs = lowerAll(p.Audio.Codecs)
		p.Audio.Languages = lowerAll(p.Audio.Languages)
		p.Subtitles.Codecs = lowerAll(p.Subtitles.Codecs)
		p.Subtitles.Languages = lowerAll(p.Subtitles.Languages)
		c.Profiles[name] = p
	}
}

func (c *Config) Validate() error {
	if len(c.Libraries) == 0 {
		return fmt.Errorf("no libraries defined")
	}
	seen := map[string]bool{}
	for _, l := range c.Libraries {
		switch {
		case l.Name == "":
			return fmt.Errorf("library with path %q has no name", l.Path)
		case seen[l.Name]:
			return fmt.Errorf("duplicate library name %q", l.Name)
		case l.Path == "":
			return fmt.Errorf("library %q has no path", l.Name)
		case l.Profile == "":
			return fmt.Errorf("library %q has no profile", l.Name)
		}
		seen[l.Name] = true

		p, ok := c.Profiles[l.Profile]
		if !ok {
			return fmt.Errorf("library %q references undefined profile %q", l.Name, l.Profile)
		}
		if p.Container == "" {
			return fmt.Errorf("profile %q has no container", l.Profile)
		}
		// A profile that can reject a stream but not re-encode it would plan a
		// transcode it cannot emit, so require the encoder up front rather
		// than failing per-file much later.
		if len(p.Video.Codecs) > 0 || p.Video.MaxHeight > 0 {
			if p.Video.Encoder.Name == "" {
				return fmt.Errorf("profile %q constrains video but sets no video encoder", l.Profile)
			}
		}
		if len(p.Audio.Codecs) > 0 || p.Audio.MaxChannels > 0 {
			if p.Audio.Encoder.Name == "" {
				return fmt.Errorf("profile %q constrains audio but sets no audio encoder", l.Profile)
			}
		}
	}
	return nil
}

// Profile returns the profile for a library. Validate guarantees it exists.
func (c *Config) Profile(l Library) Profile { return c.Profiles[l.Profile] }

func lowerAll(in []string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = strings.ToLower(strings.TrimSpace(s))
	}
	return out
}
