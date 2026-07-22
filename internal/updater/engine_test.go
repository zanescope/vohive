package updater

import (
	"archive/tar"
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"
)

func TestExtractReleaseArchive(t *testing.T) {
	archive := filepath.Join(t.TempDir(), "release.tar.gz")
	writeTestArchive(t, archive, map[string]string{
		"vohive": "main", "vohivectl": "control", "LICENSE": "license",
	})
	destination := filepath.Join(t.TempDir(), "release")
	if err := os.Mkdir(destination, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := extractReleaseArchive(archive, destination); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"vohive", "vohivectl", "LICENSE"} {
		if _, err := os.Stat(filepath.Join(destination, name)); err != nil {
			t.Fatal(err)
		}
	}
}

func TestExtractReleaseArchiveRejectsTraversalAndUnexpectedFiles(t *testing.T) {
	for _, entries := range []map[string]string{
		{"vohive": "main", "vohivectl": "control", "LICENSE": "license", "../escape": "bad"},
		{"vohive": "main", "LICENSE": "license"},
		{"vohive": "main", "vohivectl": "control", "LICENSE": "license", "config.yaml": "bad"},
	} {
		archive := filepath.Join(t.TempDir(), "release.tar.gz")
		writeTestArchive(t, archive, entries)
		destination := filepath.Join(t.TempDir(), "release")
		if err := os.Mkdir(destination, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := extractReleaseArchive(archive, destination); err == nil {
			t.Fatalf("unsafe archive accepted: %#v", entries)
		}
	}
}

func writeTestArchive(t *testing.T, path string, entries map[string]string) {
	t.Helper()
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	gzipWriter := gzip.NewWriter(file)
	tarWriter := tar.NewWriter(gzipWriter)
	for name, content := range entries {
		header := &tar.Header{Name: name, Mode: 0o755, Size: int64(len(content)), Typeflag: tar.TypeReg}
		if err := tarWriter.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if _, err := tarWriter.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}
