{
  lib,
  stdenv,
  makeWrapper,
  passt,
  tailscale,
  util-linux,
  coreutils,
}:

stdenv.mkDerivation {
  pname = "tsexec";
  version = "0.1.0";

  src = ./.;

  nativeBuildInputs = [ makeWrapper ];

  installPhase = ''
    runHook preInstall
    install -Dm755 tsexec $out/bin/tsexec
    wrapProgram $out/bin/tsexec \
      --prefix PATH : ${lib.makeBinPath [ passt tailscale util-linux coreutils ]}
    runHook postInstall
  '';

  meta = with lib; {
    description = "Run commands with all traffic routed through Tailscale";
    homepage = "https://github.com/bouk/tsexec";
    license = licenses.bsd3;
    platforms = platforms.linux;
  };
}
