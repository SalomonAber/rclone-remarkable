# rclone-remarkable

An out-of-tree rclone backend named `remarkable`. The current read-only model exposes reMarkable collections as directories and documents as raw ZIP-compatible `.rmdoc` objects. Live rmfakecloud client construction is not wired yet; the read model is exercised against the fake client.

## Architecture

The custom binary follows rclone's official out-of-tree pattern: `rclone.go` imports rclone's commands and blank-imports `backend/remarkable`, whose `init` function calls `fs.Register`. It intentionally does not import `backend/all`, keeping the binary limited to this backend.

Rclone v1.75.0 defines the mandatory contracts in `fs/types.go`:

- `fs.Fs` embeds `fs.Info` and requires `List`, `NewObject`, `Put`, `Mkdir`, and `Rmdir`.
- `fs.Object` embeds `fs.ObjectInfo` and requires `SetModTime`, `Open`, `Update`, and `Remove`. `ObjectInfo` embeds `DirEntry`, so objects also provide `Fs`, `String`, `Remote`, `ModTime`, `Size`, `Hash`, and `Storable`.
- `fs.Mover` optionally adds `Move(context.Context, fs.Object, string) (fs.Object, error)`.
- `fs.DirMover` optionally adds `DirMove(context.Context, fs.Fs, string, string) error`.

Optional interfaces are detected by `fs.Features.Fill`. Move interfaces are not advertised because the backend remains read-only.

### Read-only mapping

Paths are resolved one collection at a time through parent UUIDs. A collection's visible name is presented unchanged, while a document's visible name gains a local `.rmdoc` suffix. The suffix is never sent to reMarkable. `Object` retains the document `Item`, including UUID and version, separately from its rclone presentation path, so a later rename or move does not change identity.

Sibling entries are checked after applying the local naming rule. If two UUIDs produce the same local name, listing and path resolution return an explicit ambiguity error rather than selecting one.

`List` and `NewObject` materialize document content before returning an object, giving rclone a correct synthesized archive size. `Open` supports rclone range and seek options by opening and seeking the completed local cache file.

### Content cache

Raw archives are persisted below rclone's configured cache directory:

```text
remarkable/<document UUID>/<remote version>.rmdoc
```

Downloads go to a temporary file in the destination directory, are flushed and closed, and are then atomically renamed into place. A process-wide singleflight group keyed by the final cache path ensures simultaneous requests for the same UUID and version share one download. Different versions use different paths; stale versions are retained.

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

rmapi also creates a synthetic `trash` collection in its file tree. The read-only stage does not choose mutation or trash semantics.

`backend/remarkable/api.go` isolates this dependency behind a small `Client` using backend-owned `Item` values. The adapter handles rmapi's path-based `FetchDocument` through a temporary `.rmdoc` file, while callers use an `io.Writer`. Authentication and construction of a live `api.ApiCtx` remain deliberately unwired.

## Development

```sh
nix develop
go build ./...
go test ./...
go vet ./...
go build -o rclone-remarkable .
./rclone-remarkable help backends
```

The last command should list `remarkable`.

rclone v1.75.0 may also log `no overview data found for "remarkable"`. Its current `fs.Register` implementation unconditionally loads overview YAML from rclone's in-tree embedded filesystem, so out-of-tree backends cannot provide that record. This does not prevent registration or command use.