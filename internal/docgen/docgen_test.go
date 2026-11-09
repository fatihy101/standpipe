package docgen

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// repoRoot is the repository root relative to this package's directory.
const repoRoot = "../.."

// update makes TestGeneratedReferenceDocsAreCurrent rewrite the files instead
// of comparing them. `go generate ./...` runs the test with it; the generator
// is a test rather than a main package so that the repository's only main
// packages stay the two shipped binaries.
var update = flag.Bool("update", false, "rewrite docs/reference from the code instead of comparing")

// TestGeneratedReferenceDocsAreCurrent fails when a committed reference
// document differs from what the generator would write now. The repair is
// always the same: run `go generate ./...` and commit the result.
func TestGeneratedReferenceDocsAreCurrent(t *testing.T) {
	if *update {
		if err := Write(repoRoot); err != nil {
			t.Fatalf("writing: %v", err)
		}
	}
	files, err := Files()
	if err != nil {
		t.Fatalf("rendering: %v", err)
	}
	for _, rel := range sortedKeys(files) {
		want := files[rel]
		got, err := os.ReadFile(filepath.Join(repoRoot, filepath.FromSlash(rel)))
		if err != nil {
			t.Errorf("%s: %v\nRun `go generate ./...` to create it.", rel, err)
			continue
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s is out of date with the code it is generated from (%s).\n"+
				"Run `go generate ./...` and commit the result; do not edit the file by hand.",
				rel, firstDifference(got, want))
		}
	}
}

// TestNoStaleReferenceDocs fails when docs/reference holds a Markdown file the
// generator no longer writes, so a removed list cannot leave its old
// reference behind.
func TestNoStaleReferenceDocs(t *testing.T) {
	files, err := Files()
	if err != nil {
		t.Fatalf("rendering: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(repoRoot, filepath.FromSlash(RefDir)))
	if err != nil {
		t.Fatalf("reading %s: %v", RefDir, err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		if _, ok := files[RefDir+"/"+e.Name()]; !ok {
			t.Errorf("%s/%s is not produced by the generator; delete it or add a renderer for it", RefDir, e.Name())
		}
	}
}

// TestGeneratedDocsCarryTheNote pins the "do not edit" header on every file.
func TestGeneratedDocsCarryTheNote(t *testing.T) {
	files, err := Files()
	if err != nil {
		t.Fatalf("rendering: %v", err)
	}
	for rel, b := range files {
		if !bytes.HasPrefix(b, []byte(generatedNote)) {
			t.Errorf("%s does not start with the generated-file note", rel)
		}
		if bytes.Contains(b, []byte("\r")) {
			t.Errorf("%s contains a carriage return", rel)
		}
	}
}

// firstDifference describes where two renderings first diverge.
func firstDifference(got, want []byte) string {
	gl := strings.Split(string(got), "\n")
	wl := strings.Split(string(want), "\n")
	for i := 0; i < len(gl) || i < len(wl); i++ {
		var g, w string
		if i < len(gl) {
			g = gl[i]
		}
		if i < len(wl) {
			w = wl[i]
		}
		if g != w {
			return "first difference at line " + strconv.Itoa(i+1) + ":\n  committed: " + g + "\n  generated: " + w
		}
	}
	return "files differ"
}
