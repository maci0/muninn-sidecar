// Package scripts holds no Go code; this package exists so the release gate
// in check-release-notes.sh is covered by `go test ./...` like every other
// contract in the tree. The script is the shipped entry point, so the test
// runs it rather than restating its logic in Go.
package scripts

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// A changelog that satisfies every rule: heading first, an empty
// [Unreleased] above the newest released section, sections in descending
// order, a link per version, and one impact group per section in the order a
// reader scans them.
const validChangelog = `# Changelog

## [Unreleased]

## [0.5.0] — 2026-09-27

### Fixed

- **the widget no longer leaks.** one line

## [0.4.4] — 2026-06-02

### Fixed

- **an older fix.** one line

[unreleased]: https://example.test/compare/v0.4.4...HEAD
[0.5.0]: https://example.test/releases/tag/v0.5.0
[0.4.4]: https://example.test/releases/tag/v0.4.4
`

// run executes the gate against body and returns its combined output.
func run(t *testing.T, body, tag string) (string, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "CHANGELOG.md")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	cmd := exec.Command("bash", "check-release-notes.sh", tag)
	cmd.Env = append(os.Environ(), "CHANGELOG="+path)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func TestValidChangelogPasses(t *testing.T) {
	for _, tag := range []string{"v0.5.0", ""} {
		out, err := run(t, validChangelog, tag)
		if err != nil {
			t.Errorf("tag %q: gate failed on a valid changelog: %v\n%s", tag, err, out)
		}
	}
}

func TestGateReportsEachViolation(t *testing.T) {
	tests := []struct {
		name string
		body string
		tag  string
		want string
	}{
		{
			name: "tag without a section",
			body: validChangelog,
			tag:  "v0.6.0",
			want: "no '## [0.6.0]",
		},
		{
			name: "section without a link",
			body: strings.Replace(validChangelog,
				"[0.5.0]: https://example.test/releases/tag/v0.5.0\n", "", 1),
			tag:  "v0.5.0",
			want: "no link for [0.5.0]",
		},
		{
			name: "link without a section",
			body: validChangelog + "[0.9.9]: https://example.test/releases/tag/v0.9.9\n",
			tag:  "v0.5.0",
			want: "link [0.9.9] has no released section",
		},
		{
			name: "sections out of order",
			body: strings.Replace(validChangelog,
				"## [0.4.4] — 2026-06-02", "## [0.4.4] — 2026-06-02\n\n## [0.5.1] — 2026-09-28", 1),
			want: "not in descending version order",
		},
		{
			name: "two-digit minor below nine is out of order",
			body: `# Changelog

## [Unreleased]

## [0.9.0] — 2026-09-27

- something

## [0.10.0] — 2026-08-01

- something older

[unreleased]: https://example.test/compare/v0.10.0...HEAD
[0.9.0]: https://example.test/releases/tag/v0.9.0
[0.10.0]: https://example.test/releases/tag/v0.10.0
`,
			want: "not in descending version order",
		},
		{
			name: "no unreleased section",
			body: strings.Replace(validChangelog, "## [Unreleased]\n\n", "", 1),
			tag:  "v0.5.0",
			want: "no '## [Unreleased]' section",
		},
		{
			name: "tag is not semver",
			body: validChangelog,
			tag:  "latest",
			want: "is not a vMAJOR.MINOR.PATCH tag",
		},
		{
			name: "impact group repeated",
			body: strings.Replace(validChangelog,
				"## [0.4.4] — 2026-06-02", "## [0.4.4] — 2026-06-02\n\n### Fixed", 1),
			want: "a second Fixed group",
		},
		{
			name: "impact groups out of order",
			body: strings.Replace(validChangelog,
				"## [0.4.4] — 2026-06-02\n\n### Fixed",
				"## [0.4.4] — 2026-06-02\n\n### Security\n\n- **a hole was closed.** one line\n\n### Fixed", 1),
			want: "Fixed runs after a group below it",
		},
		{
			name: "unknown impact group",
			body: strings.Replace(validChangelog, "### Fixed", "### Deferred", 1),
			want: "unknown impact group Deferred",
		},
		{
			name: "one fix written up twice",
			body: strings.Replace(validChangelog,
				"- **the widget no longer leaks.** one line\n",
				"- **the widget no longer leaks.** one line\n\n- **Breaking: the widget no longer leaks.** and again\n", 1),
			want: "two entries state the same fix (the widget no longer leaks.)",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out, err := run(t, tc.body, tc.tag)
			if err == nil {
				t.Fatalf("gate passed, want a failure:\n%s", out)
			}
			if !strings.Contains(out, tc.want) {
				t.Errorf("gate output does not name the violation %q:\n%s", tc.want, out)
			}
		})
	}
}

// The order check compares versions component by component, numerically: 0.10.0
// is above 0.9.0. A string sort gets this backwards and would fail the valid
// changelog below, so the case pins the comparison the release gate needs.
func TestGateAcceptsTwoDigitMinorAboveNine(t *testing.T) {
	body := `# Changelog

## [Unreleased]

## [0.10.0] — 2026-09-27

- something

## [0.9.0] — 2026-08-01

- something older

[unreleased]: https://example.test/compare/v0.9.0...HEAD
[0.10.0]: https://example.test/releases/tag/v0.10.0
[0.9.0]: https://example.test/releases/tag/v0.9.0
`
	if out, err := run(t, body, "v0.10.0"); err != nil {
		t.Errorf("gate rejected 0.10.0 above 0.9.0: %v\n%s", err, out)
	}
}

// The repository's own changelog must satisfy the gate it ships, or the rule
// is decorative: the [Unreleased] block carried eight impact headings and ten
// entries written up twice until the gate checked for them, and a gate the
// changelog itself fails is a gate nobody runs.
func TestRepositoryChangelogPasses(t *testing.T) {
	cmd := exec.Command("bash", "check-release-notes.sh", "v0.4.4")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("repository CHANGELOG.md fails its own gate: %v\n%s", err, out)
	}
}
