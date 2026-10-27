package smtpgateway

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var (
	clauseRowPattern = regexp.MustCompile(`^\| ([A-Z]+)([0-9]+) \|`)
	gapRowPattern    = regexp.MustCompile(`^\| (X[0-9]+) \|`)
	clauseRefPattern = regexp.MustCompile(`\b([A-Z]+)([0-9]+)\b`)
	gapRefPattern    = regexp.MustCompile(`\bX([0-9]{1,2})\b`)
	linkPattern      = regexp.MustCompile(`\[[^\]]*\]\(([^)]+)\)`)
)

var clausePrefixes = map[string]bool{
	"S": true, "C": true, "T": true, "P": true, "E": true,
	"H": true, "L": true, "G": true, "F": true, "Q": true,
}

// TestDocsIntegrity enforces the parts of the documentation that a machine
// can check.  It fails when a clause ID is duplicated, a status is unknown,
// a gap cites an unknown clause, a gap is never referenced, a document cites
// an unknown gap, or a relative link target is missing.
//
// The decisions log is exempt from gap checks: it is append-only history and
// may cite a gap that was later closed.
func TestDocsIntegrity(t *testing.T) {
	spec, err := os.ReadFile(filepath.Join("docs", "spec.md"))
	if err != nil {
		t.Fatalf("read spec: %v", err)
	}

	clauses := map[string]bool{}
	gaps := map[string]bool{}
	referencedGaps := map[string]bool{}

	for _, line := range strings.Split(string(spec), "\n") {
		if m := gapRowPattern.FindStringSubmatch(line); m != nil {
			if gaps[m[1]] {
				t.Errorf("duplicate gap %s", m[1])
			}
			gaps[m[1]] = true
			checkGapClauses(t, m[1], line, clauses)
			continue
		}
		m := clauseRowPattern.FindStringSubmatch(line)
		if m == nil || m[1] == "X" {
			continue
		}
		id := m[1] + m[2]
		if !clausePrefixes[m[1]] {
			t.Errorf("unknown clause prefix %q in %s", m[1], id)
		}
		if clauses[id] {
			t.Errorf("duplicate clause %s", id)
		}
		clauses[id] = true

		status := statusCell(line)
		if !validStatus(status) {
			t.Errorf("clause %s: unknown status %q", id, status)
			continue
		}
		for _, ref := range gapRefPattern.FindAllString(status, -1) {
			referencedGaps[ref] = true
		}
	}

	for id := range gaps {
		if !referencedGaps[id] {
			t.Errorf("gap %s is not referenced by any clause status", id)
		}
	}

	checkGapReferences(t, gaps)
	checkLinks(t)
}

// statusCell returns the last cell of a Markdown table row.
func statusCell(line string) string {
	parts := strings.Split(line, "|")
	if len(parts) < 4 {
		return ""
	}
	return strings.TrimSpace(parts[len(parts)-2])
}

func validStatus(s string) bool {
	if strings.HasPrefix(s, "known limit") {
		return true
	}
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return false
	}
	switch fields[0] {
	case "ok", "ok*", "gap", "contract", "partial", "withdrawn":
		return true
	}
	return false
}

// checkGapClauses verifies the clause column of a gap row.
func checkGapClauses(t *testing.T, gap, line string, clauses map[string]bool) {
	t.Helper()
	parts := strings.Split(line, "|")
	if len(parts) < 5 {
		t.Errorf("gap %s: malformed row", gap)
		return
	}
	for _, ref := range clauseRefPattern.FindAllString(parts[3], -1) {
		if !clausePrefixes[ref[:1]] {
			continue
		}
		if !clauses[ref] {
			t.Errorf("gap %s cites unknown clause %s", gap, ref)
		}
	}
}

// checkGapReferences scans documents for X IDs and verifies that each one
// exists in the gap table.
func checkGapReferences(t *testing.T, gaps map[string]bool) {
	t.Helper()
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != "." && strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(path) != ".md" || path == filepath.Join("docs", "decisions.md") {
			return nil
		}
		data, readErr := fs.ReadFile(os.DirFS("."), filepath.ToSlash(path))
		if readErr != nil {
			return readErr
		}
		for _, ref := range gapRefPattern.FindAllString(string(data), -1) {
			if !gaps[ref] {
				t.Errorf("%s cites unknown gap %s", path, ref)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk documents: %v", err)
	}
}

// checkLinks verifies that every relative Markdown link resolves.
func checkLinks(t *testing.T) {
	t.Helper()
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != "." && strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			if d.Name() == "vendor" {
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(path) != ".md" {
			return nil
		}
		data, readErr := fs.ReadFile(os.DirFS("."), filepath.ToSlash(path))
		if readErr != nil {
			return readErr
		}
		inFence := false
		for _, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "```") {
				inFence = !inFence
				continue
			}
			if inFence {
				continue
			}
			for _, m := range linkPattern.FindAllStringSubmatch(line, -1) {
				target := strings.TrimSpace(strings.SplitN(m[1], "#", 2)[0])
				if target == "" ||
					strings.HasPrefix(target, "http://") ||
					strings.HasPrefix(target, "https://") ||
					strings.HasPrefix(target, "mailto:") {
					continue
				}
				full := filepath.Join(filepath.Dir(path), filepath.FromSlash(target))
				//nolint:gosec // the target comes from a repository document, not from user input
				if _, statErr := os.Stat(full); statErr != nil {
					t.Errorf("%s: broken link %q", path, target)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk links: %v", err)
	}
}
