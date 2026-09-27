package main

import (
	"crypto/sha256"
	"flag"
	"fmt"
	"os"
	"strings"
)

// mdRow formats one results row for a model.
func mdRow(model string, n int, agg [3]armAgg) string {
	note := ""
	if unreliable(agg[:]) {
		note = " <!-- unreliable: an arm lost >10% of calls -->"
	}
	return fmt.Sprintf("| %s | %d | %.2f/%.2f | %.2f/%.2f | %.2f/%.2f | %s | %s |%s\n",
		model, n, agg[0].em(), agg[0].f1(), agg[1].em(), agg[1].f1(), agg[2].em(), agg[2].f1(),
		deltaCI(&agg[0], &agg[1]), deltaCI(&agg[0], &agg[2]), note)
}

// mdManifestPrefix opens the provenance comment that starts each run's block
// in a -md results file.
const mdManifestPrefix = "<!-- msc-qa repro:"

// writeMDBlock records one run's results in the -md file. The file is a stream
// of blocks, each opened by its manifest comment. A rerun with the same
// configuration derives the same manifest, so its block is replaced in place
// rather than appended: the file converges to one row per configuration no
// matter how many times the run is repeated. A new configuration appends a new
// block, keeping the history of distinct runs.
func writeMDBlock(path, manifest string, rows []string) error {
	marker := mdManifestPrefix + " " + manifest + " -->"
	var b strings.Builder
	b.WriteString(marker + "\n")
	for _, r := range rows {
		b.WriteString(r)
	}
	block := b.String()

	existing, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	out := replaceMDBlock(string(existing), marker, block)
	return os.WriteFile(path, []byte(out), 0o644)
}

// replaceMDBlock swaps the block opened by marker for block, dropping whatever
// followed that block up to the next manifest comment (or end of file). When
// marker is absent, the block is appended. Returns the full new file contents.
func replaceMDBlock(content, marker, block string) string {
	lines := strings.SplitAfter(content, "\n")
	start := -1
	for i, l := range lines {
		if strings.TrimRight(l, "\n") == marker {
			start = i
			break
		}
	}
	if start < 0 {
		if content != "" && !strings.HasSuffix(content, "\n") {
			content += "\n"
		}
		return content + block
	}
	end := len(lines)
	for i := start + 1; i < len(lines); i++ {
		if strings.HasPrefix(lines[i], mdManifestPrefix) {
			end = i
			break
		}
	}
	// Keep any trailing blank line that separated this block from the next so
	// the file does not gain blank lines on each rerun.
	tail := ""
	if end < len(lines) && strings.TrimSpace(lines[end-1]) == "" {
		tail = lines[end-1]
	}
	var b strings.Builder
	b.WriteString(strings.Join(lines[:start], ""))
	b.WriteString(block)
	b.WriteString(tail)
	b.WriteString(strings.Join(lines[end:], ""))
	return b.String()
}

// ellipsis marks a value trunc shortened to fit its report column.
const ellipsis = "…"

// trunc clips s to at most n characters, replacing the last one with an
// ellipsis. The budget is characters, counted as runes: a byte count would cut
// a multi-byte character in half, leaving a replacement character at the end of
// a non-ASCII model label.
func trunc(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n <= 1 {
		return ellipsis
	}
	return string(r[:n-1]) + ellipsis
}

// fileSHA256 returns the hex SHA-256 of a file's contents.
func fileSHA256(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", sha256.Sum256(data)), nil
}

// reproManifest is a one-line provenance record for a run: every flag's
// effective value (secrets redacted), the dataset digest, the sampled question
// count, and the reader list. It is printed to stderr and written into -md
// output so any results row can be traced to its exact configuration.
func reproManifest(fs *flag.FlagSet, datasetSHA string, nQuestions int, readers []string) string {
	var b strings.Builder
	b.WriteString("msc-qa repro:")
	fs.VisitAll(func(f *flag.Flag) {
		v := f.Value.String()
		switch f.Name {
		case "token", "model-key", "ground-key":
			if v != "" {
				v = "<redacted>"
			}
		}
		fmt.Fprintf(&b, " -%s=%q", f.Name, v)
	})
	fmt.Fprintf(&b, " dataset-sha256=%s questions=%d readers=%q", datasetSHA, nQuestions, strings.Join(readers, ","))
	return b.String()
}
