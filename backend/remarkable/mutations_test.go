package remarkable

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"testing"

	rmarchive "github.com/juruen/rmapi/archive"
	"github.com/rclone/rclone/fs"
)

func TestFileMetadataMovesPreserveUUID(t *testing.T) {
	tests := []struct {
		name        string
		destination string
		wantParent  string
		wantName    string
	}{
		{name: "same directory rename strips suffix", destination: "Work/Bar.rmdoc", wantParent: "work", wantName: "Bar"},
		{name: "cross directory move", destination: "Archive/Foo.rmdoc", wantParent: "archive", wantName: "Foo"},
		{name: "move and rename", destination: "Archive/Bar.rmdoc", wantParent: "archive", wantName: "Bar"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			client := mutationClient(t)
			backend, err := newFs(ctx, "test", "", client, t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			src, err := backend.NewObject(ctx, "Work/Foo.rmdoc")
			if err != nil {
				t.Fatal(err)
			}
			beforeDownloads := client.downloadCount("abc123")
			dst, err := backend.Move(ctx, src, test.destination)
			if err != nil {
				t.Fatal(err)
			}
			if dst.(*Object).item.ID != "abc123" {
				t.Fatalf("destination UUID = %q", dst.(*Object).item.ID)
			}
			moves, mkdirs, removes := client.operations()
			if len(moves) != 1 || moves[0] != (moveCall{ID: "abc123", ParentID: test.wantParent, Name: test.wantName}) {
				t.Fatalf("move calls = %#v", moves)
			}
			if len(mkdirs) != 0 || len(removes) != 0 {
				t.Fatalf("unexpected operations: mkdir=%#v remove=%#v", mkdirs, removes)
			}
			if got := client.downloadCount("abc123"); got != beforeDownloads {
				t.Fatalf("rename/move downloaded content: before=%d after=%d", beforeDownloads, got)
			}
			reader, err := dst.Open(ctx)
			if err != nil {
				t.Fatal(err)
			}
			content, err := io.ReadAll(reader)
			if closeErr := reader.Close(); err == nil {
				err = closeErr
			}
			if err != nil {
				t.Fatal(err)
			}
			metadata := rmdocMetadata(t, content, "abc123")
			if metadata.DocName != test.wantName || metadata.Parent != test.wantParent {
				t.Fatalf("promoted metadata = %#v", metadata)
			}
			if got := client.downloadCount("abc123"); got != beforeDownloads {
				t.Fatalf("opening moved object downloaded content: before=%d after=%d", beforeDownloads, got)
			}
		})
	}
}

func rmdocMetadata(t *testing.T, content []byte, id string) rmarchive.MetadataFile {
	t.Helper()
	archive, err := zip.NewReader(bytes.NewReader(content), int64(len(content)))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range archive.File {
		if entry.Name != id+".metadata" {
			continue
		}
		reader, err := entry.Open()
		if err != nil {
			t.Fatal(err)
		}
		var metadata rmarchive.MetadataFile
		err = json.NewDecoder(reader).Decode(&metadata)
		_ = reader.Close()
		if err != nil {
			t.Fatal(err)
		}
		return metadata
	}
	t.Fatalf("metadata entry for %q not found", id)
	return rmarchive.MetadataFile{}
}

func TestMutationFeaturesAdvertised(t *testing.T) {
	backend, err := newFs(context.Background(), "test", "", mutationClient(t), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if backend.Features().Move == nil || backend.Features().DirMove == nil {
		t.Fatalf("mutation features not advertised: %#v", backend.Features())
	}
}

func TestMetadataReflectsRenameImmediately(t *testing.T) {
	ctx := context.Background()
	client := mutationClient(t)
	backend, err := newFs(ctx, "test", "", client, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	src, err := backend.NewObject(ctx, "Work/Foo.rmdoc")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := backend.Move(ctx, src, "Work/Bar.rmdoc"); err != nil {
		t.Fatal(err)
	}
	entries, err := backend.List(ctx, "Work")
	if err != nil {
		t.Fatal(err)
	}
	assertEntry(t, entries, "Work/Bar.rmdoc", false)
	for _, entry := range entries {
		if entry.Remote() == "Work/Foo.rmdoc" {
			t.Fatal("old name remains visible after rename")
		}
	}
}

func TestDirectoryMetadataMovesPreserveUUID(t *testing.T) {
	tests := []struct {
		name        string
		destination string
		wantParent  string
		wantName    string
	}{
		{name: "rename", destination: "Renamed", wantParent: "", wantName: "Renamed"},
		{name: "move", destination: "Archive/Project", wantParent: "archive", wantName: "Project"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			client := mutationClient(t)
			backend, err := newFs(ctx, "test", "", client, t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			if err := backend.DirMove(ctx, backend, "Project", test.destination); err != nil {
				t.Fatal(err)
			}
			moves, mkdirs, removes := client.operations()
			if len(moves) != 1 || moves[0] != (moveCall{ID: "project", ParentID: test.wantParent, Name: test.wantName}) {
				t.Fatalf("directory move calls = %#v", moves)
			}
			if len(mkdirs) != 0 || len(removes) != 0 {
				t.Fatalf("directory was recreated: mkdir=%#v remove=%#v", mkdirs, removes)
			}
			item, err := client.Get(ctx, "project")
			if err != nil || item.ID != "project" {
				t.Fatalf("collection identity = %#v, error = %v", item, err)
			}
		})
	}
}

func TestMutationDestinationCollisions(t *testing.T) {
	ctx := context.Background()
	client := mutationClient(t)
	client.items["bar"] = Item{ID: "bar", Name: "Bar", ParentID: "work", Kind: ItemDocument, Version: 1}
	client.contents["bar"] = rmdocArchive(t, "bar")
	backend, err := newFs(ctx, "test", "", client, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	src, err := backend.NewObject(ctx, "Work/Foo.rmdoc")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := backend.Move(ctx, src, "Work/Bar.rmdoc"); !errors.Is(err, errDestinationExists) {
		t.Fatalf("file collision error = %v", err)
	}
	if err := backend.DirMove(ctx, backend, "Project", "Archive/Existing"); !errors.Is(err, fs.ErrorDirExists) {
		t.Fatalf("directory collision error = %v", err)
	}
	moves, _, _ := client.operations()
	if len(moves) != 0 {
		t.Fatalf("collision invoked remote move: %#v", moves)
	}
}

func TestObjectRemoveOnce(t *testing.T) {
	ctx := context.Background()
	client := mutationClient(t)
	backend, err := newFs(ctx, "test", "", client, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	object, err := backend.NewObject(ctx, "Work/Foo.rmdoc")
	if err != nil {
		t.Fatal(err)
	}
	if err := object.Remove(ctx); err != nil {
		t.Fatal(err)
	}
	_, _, removes := client.operations()
	if len(removes) != 1 || removes[0] != "abc123" {
		t.Fatalf("remove calls = %#v", removes)
	}
}

func TestRmdirSafetyAndMkdir(t *testing.T) {
	ctx := context.Background()
	client := mutationClient(t)
	backend, err := newFs(ctx, "test", "", client, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := backend.Rmdir(ctx, "Work"); !errors.Is(err, fs.ErrorDirectoryNotEmpty) {
		t.Fatalf("non-empty Rmdir error = %v", err)
	}
	if err := backend.Mkdir(ctx, "Archive/New Folder"); err != nil {
		t.Fatal(err)
	}
	if err := backend.Rmdir(ctx, "Archive/New Folder"); err != nil {
		t.Fatal(err)
	}
	_, mkdirs, removes := client.operations()
	if len(mkdirs) != 1 || mkdirs[0] != (mkdirCall{ParentID: "archive", Name: "New Folder"}) {
		t.Fatalf("mkdir calls = %#v", mkdirs)
	}
	if len(removes) != 1 || removes[0] != "created-1" {
		t.Fatalf("Rmdir remove calls = %#v", removes)
	}
}

func TestMkdirCreatesMissingConfiguredRoot(t *testing.T) {
	ctx := context.Background()
	client := &fakeClient{items: map[string]Item{}}
	backend, err := newFs(ctx, "test", "Parent/Work", client, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if !backend.rootMissing {
		t.Fatal("missing root was not retained for creation")
	}
	if err := backend.Mkdir(ctx, ""); err != nil {
		t.Fatal(err)
	}
	if backend.rootMissing || backend.rootID == "" {
		t.Fatalf("root not created: missing=%t id=%q", backend.rootMissing, backend.rootID)
	}
	_, mkdirs, _ := client.operations()
	if len(mkdirs) != 2 || mkdirs[0].Name != "Parent" || mkdirs[1].Name != "Work" {
		t.Fatalf("root mkdir calls = %#v", mkdirs)
	}
}

func TestRmdirRemovesConfiguredRoot(t *testing.T) {
	ctx := context.Background()
	client := &fakeClient{items: map[string]Item{
		"work": {ID: "work", Name: "Work", Kind: ItemDirectory},
	}}
	backend, err := newFs(ctx, "test", "Work", client, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := backend.Rmdir(ctx, ""); err != nil {
		t.Fatal(err)
	}
	_, _, removes := client.operations()
	if len(removes) != 1 || removes[0] != "work" {
		t.Fatalf("configured root removes = %#v", removes)
	}
}

func mutationClient(t *testing.T) *fakeClient {
	t.Helper()
	return &fakeClient{
		items: map[string]Item{
			"work":     {ID: "work", Name: "Work", Kind: ItemDirectory},
			"archive":  {ID: "archive", Name: "Archive", Kind: ItemDirectory},
			"project":  {ID: "project", Name: "Project", Kind: ItemDirectory},
			"existing": {ID: "existing", Name: "Existing", ParentID: "archive", Kind: ItemDirectory},
			"abc123":   {ID: "abc123", Name: "Foo", ParentID: "work", Kind: ItemDocument, Version: 4},
		},
		contents: map[string][]byte{
			"abc123": rmdocArchive(t, "abc123"),
		},
	}
}
