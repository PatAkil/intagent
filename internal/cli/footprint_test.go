package cli

import (
	"context"
	"path/filepath"
	"slices"
	"testing"

	"github.com/patakil/intagent/internal/board"
	"github.com/patakil/intagent/internal/gitx"
)

// Once git's time is up, the files left go to the server without an area,
// which it reads as an area it does not know, rather than not at all.
func TestFootprintAreasStopWithGitsTime(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "svc/pay/go.mod"), "module pay\n")
	files := []string{"README.md", "svc/pay/retry.go", "svc/pay/api/client.go", "web/app.ts"}
	ctx, cancel := context.WithCancel(context.Background())
	w := &workspace{areas: gitx.NewAreas(root, nil)}
	want := []board.PathRef{{Path: "README.md"}, {Path: "svc/pay/retry.go", Area: "svc/pay"},
		{Path: "svc/pay/api/client.go", Area: "svc/pay"}, {Path: "web/app.ts", Area: "web"}}
	if got := w.pathRefs(ctx, files); !slices.Equal(got, want) {
		t.Fatalf("with time left: %v", got)
	}
	cancel()
	w = &workspace{areas: gitx.NewAreas(root, nil)}
	got := w.pathRefs(ctx, files)
	for i := range want {
		want[i].Area = ""
	}
	if !slices.Equal(got, want) {
		t.Fatalf("out of time: %v, want every path without an area", got)
	}
}
