package download

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNameFromTitle(t *testing.T) {
	// The expectations mirror names already in the user's library, so a change
	// here silently re-downloads everything.
	cases := []struct {
		title string
		want  string
	}{
		{"Chip Chipping", "chip-chipping"},
		{"It's Sow Time", "its-sow-time"},
		{"Midge's Christmas Wish", "midges-christmas-wish"},
		{"A-mazing Day", "a-mazing-day"},
		{"Big Machines 2", "big-machines-2"},
		{"Timberrrrr!", "timberrrrr"},
		{"Spuds in Mud", "spuds-in-mud"},
		{"Midge’s Christmas Wish", "midges-christmas-wish"},
		{"Café Crème", "cafe-creme"},
		{"  Leading & trailing  ", "leading-trailing"},
		{"Multiple   ---   separators", "multiple-separators"},
		{"", ""},
		{"!!! ??? ...", ""},
		{"中文", ""},
	}
	for _, c := range cases {
		if got := NameFromTitle(c.title); got != c.want {
			t.Errorf("NameFromTitle(%q) = %q, want %q", c.title, got, c.want)
		}
	}
}

func TestNameFromTitleTruncates(t *testing.T) {
	got := NameFromTitle(strings.Repeat("verylongword ", 40))
	if len(got) > maxNameLen {
		t.Errorf("length %d exceeds maxNameLen %d", len(got), maxNameLen)
	}
	if strings.HasPrefix(got, "-") || strings.HasSuffix(got, "-") {
		t.Errorf("truncation left a dangling hyphen: %q", got)
	}
	if !strings.HasPrefix(got, "verylongword-verylongword") {
		t.Errorf("unexpected stem: %q", got)
	}
}

// touch creates an empty file in dir, failing the test if it cannot.
func touch(t *testing.T, dir, name string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), nil, 0o644); err != nil {
		t.Fatalf("WriteFile %s: %v", name, err)
	}
}

func TestExistingFileIgnoresExtensions(t *testing.T) {
	cases := []struct {
		file string
		stem string
	}{
		{"chip-chipping.ts.mp4", "chip-chipping"}, // HLS leaves a double extension
		{"chip-chipping.ts", "chip-chipping"},
		{"chip-chipping.mp4", "chip-chipping"},
		{"chip-chipping.mkv", "chip-chipping"},
		{"chip-chipping.webm", "chip-chipping"},
		{"chip-chipping.mov", "chip-chipping"},
		{"chip-chipping.m4v", "chip-chipping"},
		{"chip-chipping.avi", "chip-chipping"},
		{"chip-chipping", "chip-chipping"},
		{"Chip-Chipping.TS.MP4", "chip-chipping"}, // case-insensitive
	}
	for _, c := range cases {
		dir := t.TempDir()
		touch(t, dir, c.file)
		got, ok := ExistingFile(dir, c.stem)
		if !ok {
			t.Errorf("ExistingFile(%q) found nothing, want %q", c.stem, c.file)
			continue
		}
		if want := filepath.Join(dir, c.file); got != want {
			t.Errorf("ExistingFile(%q) = %q, want %q", c.stem, got, want)
		}
	}
}

func TestExistingFileRequiresWholeStem(t *testing.T) {
	// "munchy-crunchy-1" is a collision-suffixed sibling: a different video.
	// Matching it would silently skip a real download.
	dir := t.TempDir()
	touch(t, dir, "munchy-crunchy-1.ts.mp4")
	if got, ok := ExistingFile(dir, "munchy-crunchy"); ok {
		t.Errorf("ExistingFile matched %q, want no match", got)
	}
	// The suffixed name still matches its own stem.
	if _, ok := ExistingFile(dir, "munchy-crunchy-1"); !ok {
		t.Error("ExistingFile did not match the suffixed stem")
	}
}

func TestExistingFileSkipsPartials(t *testing.T) {
	for _, name := range []string{"chip-chipping.mp4.part", "chip-chipping.part", "chip-chipping.parts"} {
		dir := t.TempDir()
		touch(t, dir, name)
		if got, ok := ExistingFile(dir, "chip-chipping"); ok {
			t.Errorf("ExistingFile matched partial %q as %q", name, got)
		}
	}
}

func TestExistingFileIgnoresDirsAndOtherNames(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "chip-chipping"), 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	touch(t, dir, "spuds-in-mud.mp4")
	if got, ok := ExistingFile(dir, "chip-chipping"); ok {
		t.Errorf("ExistingFile matched %q, want no match", got)
	}
}

func TestExistingFileMissingDir(t *testing.T) {
	if got, ok := ExistingFile(filepath.Join(t.TempDir(), "nope"), "chip-chipping"); ok || got != "" {
		t.Errorf("ExistingFile(missing dir) = (%q, %v), want (\"\", false)", got, ok)
	}
}

func TestExistingFileEmptyStem(t *testing.T) {
	dir := t.TempDir()
	touch(t, dir, ".mp4")
	if got, ok := ExistingFile(dir, ""); ok {
		t.Errorf("ExistingFile with empty stem matched %q", got)
	}
}
