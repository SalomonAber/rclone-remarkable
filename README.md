# rclone-remarkable

Stage 1 scaffold for an out-of-tree rclone backend named `remarkable`. The eventual backend will expose the reMarkable document tree as directories and each document as its raw ZIP-compatible `.rmdoc` archive. No filesystem operations or live cloud connection are implemented yet.

## Architecture

The custom binary follows rclone's official out-of-tree pattern: `rclone.go` imports rclone's commands and blank-imports `backend/remarkable`, whose `init` function calls `fs.Register`. It intentionally does not import `backend/all`, keeping the binary limited to this backend.

Rclone v1.75.0 defines the mandatory contracts in `fs/types.go`:

- `fs.Fs` embeds `fs.Info` and requires `List`, `NewObject`, `Put`, `Mkdir`, and `Rmdir`.
- `fs.Object` embeds `fs.ObjectInfo` and requires `SetModTime`, `Open`, `Update`, and `Remove`. `ObjectInfo` embeds `DirEntry`, so objects also provide `Fs`, `String`, `Remote`, `ModTime`, `Size`, `Hash`, and `Storable`.
- `fs.Mover` optionally adds `Move(context.Context, fs.Object, string) (fs.Object, error)`.
- `fs.DirMover` optionally adds `DirMove(context.Context, fs.Fs, string, string) error`.

Optional interfaces are detected by `fs.Features.Fill`. Move interfaces are not advertised in this stage because their filesystem behavior is not implemented.

## rmapi findings

The maintained `poplicola/rmapi` fork is usable as a Go library. Its module still declares `github.com/juruen/rmapi`, and its packages import that path internally. `go.mod` therefore requires the declared path and replaces it with the maintained fork at commit `2bcde75bf5436626f29497df5390870aa63ea4d0` (2026-04-30).

The relevant library surface is the sync 1.5 `api.ApiCtx` and its `filetree`/`model` packages:

| Backend concept | rmapi representation/API |
| --- | --- |
| UUID | `model.Document.ID` |
| visible name | `model.Document.Name`; serialized as metadata `visibleName` |
| parent UUID | `model.Document.Parent` |
| item type | `CollectionType`, `DocumentType`, or `TemplateType` |
| document version | `model.Document.Version` |
| modification time | `model.Document.ModifiedClient`, RFC3339Nano; derived from metadata milliseconds |
| raw `.rmdoc` | `ApiCtx.FetchDocument(id, path)` writes the document blob set as a ZIP archive |
| rename/move | `ApiCtx.MoveEntry(srcNode, dstDirNode, name)` performs both atomically in metadata |
| mkdir | `ApiCtx.CreateDir(parentID, name, notify)` |
| delete | `ApiCtx.DeleteEntry(node, recursive, notify)` removes the entry from the sync tree |

rmapi also creates a synthetic `trash` collection in its file tree. Stage 1 does not choose user-visible trash semantics; that belongs with real filesystem behavior.

`backend/remarkable/api.go` isolates this dependency behind a small `Client` using backend-owned `Item` values. The adapter handles rmapi's path-based `FetchDocument` through a temporary `.rmdoc` file, while callers use an `io.Writer`. Authentication and construction of a live `api.ApiCtx` remain deliberately unwired.

## Development

```sh
nix develop
go build ./...
go test ./...
go build -o rclone-remarkable .
./rclone-remarkable help backends
```

The last command should list `remarkable`.

rclone v1.75.0 may also log `no overview data found for "remarkable"`. Its current `fs.Register` implementation unconditionally loads overview YAML from rclone's in-tree embedded filesystem, so out-of-tree backends cannot provide that record. This does not prevent registration or command use.