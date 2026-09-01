{
  lib,
  buildGoModule,
  makeWrapper,
  fuse3,
}:

buildGoModule {
  pname = "rclone-remarkable";
  version = "0.1.0";

  src = lib.cleanSource ./.;
  vendorHash = "sha256-9v6Q3+AyE3jjjiPa6U70CqZvPUGm/TJc4e4eZPexQ/A=";

  subPackages = [ "." ];

  nativeBuildInputs = [ makeWrapper ];

  checkPhase = ''
    runHook preCheck
    go test ./...
    runHook postCheck
  '';

  postInstall = ''
    mv "$out/bin/rclone-remarkable" "$out/bin/rclone"
    ln -s rclone "$out/bin/rclone-remarkable"
    ln -s rclone "$out/bin/rclonefs"
    ln -s rclone "$out/bin/mount.rclone"

    wrapProgram "$out/bin/rclone" \
      --suffix PATH : "${lib.makeBinPath [ fuse3 ]}"
  '';

  doInstallCheck = true;
  installCheckPhase = ''
    runHook preInstallCheck
    test -L "$out/bin/mount.rclone"
    "$out/bin/rclone" help backends 2>/dev/null | grep '^  remarkable' >/dev/null
    "$out/bin/mount.rclone" --help 2>/dev/null | grep '^Rclone mount' >/dev/null
    runHook postInstallCheck
  '';

  meta = {
    description = "Custom rclone binary with the remarkable backend";
    homepage = "https://github.com/poplicola/rclone-remarkable";
    license = lib.licenses.mit;
    mainProgram = "rclone";
    platforms = lib.platforms.linux;
    priority = 4;
  };
}