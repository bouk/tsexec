{
  lib,
  buildGoModule,
  makeWrapper,
  passt,
}:

buildGoModule {
  pname = "tsexec";
  version = "0.1.0";

  src = ./.;

  vendorHash = "sha256-KMPIgX19Hw1MPE1vZXxolOMfbGYJC1gi3jDPtM1pddk=";

  nativeBuildInputs = [ makeWrapper ];

  postInstall = ''
    wrapProgram $out/bin/tsexec \
      --prefix PATH : ${lib.makeBinPath [ passt ]}
  '';

  meta = with lib; {
    description = "Run commands with all traffic routed through Tailscale";
    homepage = "https://github.com/bouk/tsexec";
    license = licenses.mit;
    platforms = platforms.linux;
  };
}
