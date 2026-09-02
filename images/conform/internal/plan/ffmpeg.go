package plan

import (
	"fmt"
	"sort"
	"strings"
)

// typeChar maps a stream type onto the letter ffmpeg uses in a stream
// specifier such as `-c:a:1`.
var typeChar = map[string]string{"video": "v", "audio": "a", "subtitle": "s"}

// FFmpegArgs renders the plan as arguments for ffmpeg, writing to out.
//
// Every codec option is emitted with a full stream specifier (`-crf:v:0`
// rather than `-crf`). That costs nothing for the common single-video-stream
// file and is what keeps a profile correct on a file with two video tracks,
// where a bare option would be applied to both.
func (p *Plan) FFmpegArgs(in, out string) []string {
	args := []string{"-hide_banner", "-nostdin", "-y"}
	args = append(args, p.InputArgs...)
	args = append(args, "-i", in)

	// Counters give each output stream its index within its own type, which
	// is what a stream specifier counts — not the position in -map order.
	n := map[string]int{}
	var codecArgs []string
	for _, s := range p.Streams {
		args = append(args, "-map", fmt.Sprintf("0:%d", s.Source))

		tc := typeChar[s.Type]
		idx := n[s.Type]
		n[s.Type]++
		spec := fmt.Sprintf("%s:%d", tc, idx)

		codecArgs = append(codecArgs, "-c:"+spec, s.Codec)
		if s.Filter != "" {
			codecArgs = append(codecArgs, "-filter:"+spec, s.Filter)
		}
		for _, k := range sortedKeys(s.Options) {
			codecArgs = append(codecArgs, fmt.Sprintf("-%s:%s", k, spec), s.Options[k])
		}
	}

	args = append(args, codecArgs...)
	// Chapters and global metadata survive the rewrite; without these a remux
	// silently discards them.
	args = append(args, "-map_metadata", "0", "-map_chapters", "0")
	args = append(args, p.OutputArgs...)
	args = append(args, out)
	return args
}

// String renders the plan as a one-line human summary.
func (p *Plan) String() string {
	if len(p.Reasons) == 0 {
		return string(p.Action)
	}
	return string(p.Action) + ": " + strings.Join(p.Reasons, "; ")
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
