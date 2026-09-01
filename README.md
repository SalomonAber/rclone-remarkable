# rclone-remarkable

An out-of-tree rclone backend named `remarkable`. The current model exposes reMarkable collections as directories and documents as raw ZIP-compatible `.rmdoc` objects. New `.rmdoc` imports and metadata mutations are supported; editing or replacing an existing compound document is not.

## NixOS Quick Start

You need a running rmfakecloud instance and an rmapi YAML file containing `devicetoken` and `usertoken`. Keep that file encrypted with agenix; only its runtime path is referenced by the Nix configuration.

1. Add this repository to your flake inputs and import its NixOS module:

	```nix
	{
	  inputs.agenix.url = "github:ryantm/agenix";
	  inputs.rclone-remarkable.url = "github:SalomonAber/rclone-remarkable";

	  outputs = inputs@{ nixpkgs, ... }: {
	    nixosConfigurations.your-host = nixpkgs.lib.nixosSystem {
	      system = "x86_64-linux";
	      modules = [
	        inputs.agenix.nixosModules.default
	        inputs.rclone-remarkable.nixosModules.default
	        ./configuration.nix
	      ];
	    };
	  };
	}
	```

2. Add the mount to `configuration.nix`:

	```nix
	{ config, ... }:
	{
	  age.secrets.rmapi-config = {
	    file = ./secrets/rmapi-config.age;
	    owner = "root";
	    group = "root";
	    mode = "0400";
	  };

	  environment.etc."rclone-remarkable.conf".text = ''
	    [remarkable]
	    type = remarkable
	    host = https://rmfakecloud.example.com
	    config = ${config.age.secrets.rmapi-config.path}
	    refresh_interval = 30s
	  '';

	  programs.fuse.userAllowOther = true;

	  systemd.tmpfiles.rules = [
	    "d /var/cache/rclone/remarkable 0750 root root -"
	  ];

	  fileSystems."/mnt/remarkable" = {
	    device = "remarkable:";
	    fsType = "rclone";
	    options = [
	      "nodev"
	      "nofail"
	      "_netdev"
	      "allow_other"
	      "args2env"
	      "config=/etc/rclone-remarkable.conf"
	      "cache_dir=/var/cache/rclone/remarkable"
	      "vfs_cache_mode=full"
	      "vfs_write_back=0s"
	      "dir_cache_time=30s"
	      "poll_interval=0"
	      "attr_timeout=1s"
	    ];
	  };
	}
	```

	Replace the host URL and agenix secret path for your system. If rmfakecloud runs as a local service, add `x-systemd.requires=rmfakecloud.service` and `x-systemd.after=rmfakecloud.service` to the mount options.

3. Rebuild and verify:

	```sh
	sudo nixos-rebuild switch --flake .#your-host
	mount | grep remarkable
	ls /mnt/remarkable
	```

The imported module installs this project's `mount.rclone` through `system.fsPackages`, so the normal NixOS `fileSystems` integration uses the custom backend without a hand-written systemd service. Existing `.rmdoc` files can be read, renamed, moved, and removed. Copying a new valid `.rmdoc` into the mount imports one reMarkable document; overwriting an existing document is intentionally rejected.

## Architecture

The custom binary follows rclone's official out-of-tree pattern: `rclone.go` imports rclone's commands and blank-imports `backend/remarkable`, whose `init` function calls `fs.Register`. It intentionally does not import `backend/all`, keeping the binary limited to this backend.

Rclone v1.75.0 defines the mandatory contracts in `fs/types.go`:

- `fs.Fs` embeds `fs.Info` and requires `List`, `NewObject`, `Put`, `Mkdir`, and `Rmdir`.
- `fs.Object` embeds `fs.ObjectInfo` and requires `SetModTime`, `Open`, `Update`, and `Remove`. `ObjectInfo` embeds `DirEntry`, so objects also provide `Fs`, `String`, `Remote`, `ModTime`, `Size`, `Hash`, and `Storable`.
- `fs.Mover` optionally adds `Move(context.Context, fs.Object, string) (fs.Object, error)`.
- `fs.DirMover` optionally adds `DirMove(context.Context, fs.Fs, string, string) error`.

Optional interfaces are detected by `fs.Features.Fill`. The backend advertises `fs.Mover` and `fs.DirMover` and implements create-only `Put`.

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

### Metadata mutations

Mutations resolve source entries and destination parents to UUIDs before calling the client. Destination names are checked in the synthesized rclone namespace, so collisions are rejected before any remote call. File destinations must end in `.rmdoc`; that suffix is stripped before changing the reMarkable visible name.

| rclone operation | Client/rmapi operation |
| --- | --- |
| `Mkdir` | `Client.Mkdir(parentUUID, visibleName)` / `ApiCtx.CreateDir` |
| empty `Rmdir` | `Client.Remove(collectionUUID)` / non-recursive `ApiCtx.DeleteEntry` |
| `Object.Remove` | `Client.Remove(documentUUID)` / non-recursive `ApiCtx.DeleteEntry` |
| `fs.Mover.Move` | `Client.Move(existingUUID, parentUUID, visibleName)` / `ApiCtx.MoveEntry` |
| `fs.DirMover.DirMove` | the same single `MoveEntry` call for the existing collection UUID |

The rmapi adapter updates its in-memory file tree immediately after successful create, move, and delete calls. Rename and move never download, delete, recreate, or upload document content. Existing raw cache entries remain available by UUID and version.

### `.rmdoc` creation

`Put` supports creating a new document at an absent `.rmdoc` path. Incoming data is streamed to a private temporary file below `<cache-dir>/remarkable/.uploads`; it is never buffered as one in-memory object. Before any remote call, the backend reads every ZIP member to verify structure and CRCs, rejects unsafe paths, and requires exactly one top-level UUID `.content` entry plus its matching `.metadata` entry. The embedded UUID must not already exist anywhere in the remote tree.

After validation, one `ApiCtx.UploadDocument` call imports the archive. rmapi rewrites the imported metadata with the destination visible name and parent, uploads component blobs, and publishes the document through the sync root. Failed validation, path/UUID collisions, and unsupported overwrite attempts are marked non-retryable; transport/API failures remain retryable.

rmapi normalizes imported archives, so a later downloaded `.rmdoc` is semantically equivalent but not byte-for-byte identical and may have a different ZIP size. The object returned directly from `Put` therefore reports unknown size (`-1`); a subsequent listing materializes the published representation and reports its actual size.

`Object.Update` deliberately rejects replacement. rmapi's `ReplaceDocumentFile` replaces one contained PDF/EPUB-style payload and does not safely replace an entire compound `.rmdoc`, so the backend does not pretend that POSIX editing semantics exist.

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

rmapi also creates a synthetic `trash` collection in its file tree. Deletion uses rmapi's normal non-recursive `DeleteEntry` operation; non-empty directories are rejected with rclone's `ErrorDirectoryNotEmpty` before that call.

`backend/remarkable/api.go` isolates this dependency behind a small `Client` using backend-owned `Item` values. The adapter constructs rmapi's sync 1.5 `api.ApiCtx` directly using `transport.CreateHttpClientCtx` and `api.CreateApiCtx`; no rmapi subprocess is used. It handles rmapi's path-based `FetchDocument` through a temporary `.rmdoc` file, while callers use an `io.Writer`.

## Configuration

The backend accepts these options:

| Option | Purpose |
| --- | --- |
| `host` | API base URL. Defaults to `RMAPI_HOST`, then `http://127.0.0.1:7632`. |
| `config` | YAML credentials file with `devicetoken` and `usertoken`. Defaults to `RMAPI_CONFIG`. |
| `device_token` | Overrides `devicetoken` from the config file. |
| `user_token` | Overrides `usertoken` from the config file. |

`usertoken` is required. This backend deliberately does not use rmapi's interactive token helper because it may terminate the process on configuration failures. Refresh an expired user token with rmapi or provide an updated YAML file/token.

With a compatible rmapi config already selected through `RMAPI_CONFIG`, use:

```sh
go build -o rclone .
./rclone lsf :remarkable,host=http://127.0.0.1:7632:
./rclone copyto :remarkable,host=http://127.0.0.1:7632:Work/Existing.rmdoc /tmp/Existing.rmdoc
```

## Integration Test

The integration suite is disabled by default. It creates uniquely named top-level folders, uploads a generated controlled `.rmdoc`, verifies list/rename/move/download/delete behavior, and removes its test folders. It never selects unrelated documents.

```sh
REMARKABLE_INTEGRATION=1 \
RMAPI_CONFIG=/path/to/rmapi.conf \
go test ./backend/remarkable -run TestRMFakecloudIntegration -count=1
```

Set `REMARKABLE_HOST` to override the integration endpoint and `REMARKABLE_USER_TOKEN`/`REMARKABLE_DEVICE_TOKEN` to supply credentials without a YAML file.

## Mount

The following configuration was tested against rmfakecloud with `ls`, `stat`, repeated and concurrent reads, copy-out, same-directory and cross-directory `mv`, `mkdir`, non-empty and empty `rmdir`, and `rm`:

```sh
RCLONE_REMARKABLE_REFRESH_INTERVAL=30s \
./rclone mount \
	:remarkable,host=http://127.0.0.1:7632: \
	/tmp/remarkable \
	--vfs-cache-mode full \
	--vfs-write-back 0s \
	--cache-dir /tmp/rclone-remarkable-cache \
	--dir-cache-time 30s \
	--poll-interval 0 \
	--attr-timeout 1s
```

Use an absolute persistent `--cache-dir` for normal operation. `full` mode gives editors and other POSIX applications stable local seek/read behavior; `--vfs-write-back 0s` starts remote creation as soon as a new file closes. Polling is disabled because this backend does not implement rclone's change-notify interface; instead, `refresh_interval` bounds rmapi metadata refreshes during listings. Keep `--dir-cache-time` near that interval. Successful mutations update rmapi's in-process tree immediately, so they do not wait for either interval.

The backend content cache remains keyed by UUID and remote version beneath `<cache-dir>/remarkable`. Metadata-only file moves atomically derive the new version from the cached `.rmdoc` and rewrite its embedded metadata, avoiding a remote content download. The VFS cache is a separate rclone-managed layer. Testing confirmed repeated stats, copy-out, and eight concurrent opens reused one VFS entry; unmount left valid ZIP archives and no `.materializing-*` or `.promoting-*` files.

Copying a valid new `.rmdoc` into the mount is supported. Opening an existing remote object for write, truncating it, or copying over it is rejected. With full VFS caching, writeback happens asynchronously, so scripts that must synchronously observe upload errors should prefer `rclone copyto`; mount writeback failures are retained by VFS and logged.

## Compatibility Notes

- rmapi's endpoint URLs are package-global values. The backend sets them from `host` before constructing its client, so different `remarkable` hosts are not safe to use simultaneously in one rclone process.
- Rclone connection strings use `:` to separate options from the remote path, which normally splits an unescaped `http://host:port` value. The backend recognizes and repairs the resulting parsed form for HTTP/HTTPS hosts with numeric ports, so the short command examples above work as written. Standard configured remotes and escaped connection strings do not need this compatibility path.
- rmapi persists its sync tree separately in the OS cache directory (`rmapi/tree.cache`), outside rclone's `--cache-dir`. The client refreshes that tree during construction and periodically while listing.
- rmfakecloud supports the sync 1.5/v3/v4 routes used by current rmapi. Its deployment must issue a valid sync 1.5 user token and configure a storage URL reachable by the client; newer reMarkable software has additional HTTPS/no-port restrictions documented by rmfakecloud.

## NixOS Packaging Details

The flake's default package builds the custom binary and provides `rclone`, `rclone-remarkable`, `rclonefs`, and `mount.rclone`. The last two are symlinks to the wrapped custom binary, matching nixpkgs' stock rclone package. The wrapper adds FUSE 3 utilities to its fallback PATH without shadowing NixOS's privileged `/run/wrappers/bin/fusermount3`.

Importing `nixosModules.default` adds the custom package to `system.fsPackages`. Current NixOS includes those packages in the systemd manager and fstab-generator PATH, so the standard mount unit resolves this package's `mount.rclone`; no custom service is needed. NixOS does not automatically add stock rclone for `fsType = "rclone"`. Avoid adding both helpers to `system.fsPackages`; the custom package has a higher package priority so it also wins profile collisions if stock rclone is installed separately.

```nix
{
	inputs.rclone-remarkable.url = "github:SalomonAber/rclone-remarkable";

	outputs = inputs@{ nixpkgs, rclone-remarkable, ... }: {
		nixosConfigurations.host = nixpkgs.lib.nixosSystem {
			system = "x86_64-linux";
			modules = [
				rclone-remarkable.nixosModules.default
				({ ... }: {
					environment.etc."rclone-remarkable.conf".text = ''
						[remarkable]
						type = remarkable
						host = http://127.0.0.1:7632
						config = /run/agenix/rmapi-config
						refresh_interval = 30s
					'';

					systemd.tmpfiles.rules = [
						"d /var/cache/rclone/remarkable 0750 root root -"
					];

          programs.fuse.userAllowOther = true;

					fileSystems."/mnt/remarkable" = {
						device = "remarkable:";
						fsType = "rclone";
						options = [
							"nodev"
							"nofail"
							"_netdev"
							"allow_other"
							"args2env"
							"config=/etc/rclone-remarkable.conf"
							"cache_dir=/var/cache/rclone/remarkable"
							"vfs_cache_mode=full"
							"vfs_write_back=0s"
							"dir_cache_time=30s"
							"poll_interval=0"
							"attr_timeout=1s"

							# Include these only when rmfakecloud is a local NixOS service.
							"x-systemd.requires=rmfakecloud.service"
							"x-systemd.after=rmfakecloud.service"
						];
					};
				})
			];
		};
	};
}
```

For a remote rmfakecloud, omit the two `rmfakecloud.service` options; `_netdev` supplies normal network-mount ordering. `nofail` prevents an unavailable cloud from failing boot. Add `x-systemd.automount` when on-demand mounting is preferable.

The non-secret rclone config can live in the Nix store because it only references `/run/agenix/rmapi-config`; the YAML containing `devicetoken` and `usertoken` must be deployed by agenix outside the store. After rebuilding, verify with:

```sh
mount | grep remarkable
ls /mnt/remarkable
mv /mnt/remarkable/Test.rmdoc /mnt/remarkable/Renamed.rmdoc
```

The final rename maps to one rmapi metadata move on the existing UUID. Verify identity with rmfakecloud's document metadata/API or by comparing the `ID` before and after in an integration test.

## Development

```sh
nix develop
go build ./...
go test ./...
go vet ./...
go build -o rclone .
./rclone help backends
```

The last command should list `remarkable`.

rclone v1.75.0 may also log `no overview data found for "remarkable"`. Its current `fs.Register` implementation unconditionally loads overview YAML from rclone's in-tree embedded filesystem, so out-of-tree backends cannot provide that record. This does not prevent registration or command use.