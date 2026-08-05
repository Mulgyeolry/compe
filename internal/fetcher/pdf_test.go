package fetcher

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestPDFUsesPdftotext(t *testing.T) {
	dir := t.TempDir()
	name := "pdftotext"
	content := "#!/bin/sh\necho '2026 AI Agent PDF competition registration'\n"
	if runtime.GOOS == "windows" {
		name += ".bat"
		content = "@echo 2026 AI Agent PDF competition registration\r\n"
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		_, _ = io.WriteString(w, "%PDF-simulated")
	}))
	defer server.Close()
	collector := newTestFetchCollector(t, server.URL)
	doc, err := collector.Fetch(context.Background(), testBaseURL+"/notice.pdf")
	if err != nil {
		t.Fatal(err)
	}
	if doc.ContentType != "application/pdf" || !strings.Contains(doc.Text, "AI Agent") {
		t.Fatalf("unexpected pdf extraction result: %#v", doc)
	}
}
