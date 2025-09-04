{
  sources ? import ./sources.nix,
  system ? builtins.currentSystem,
}:
import sources.nixpkgs {
  overlays = [
    (_: pkgs: {
      flake-compat = import sources.flake-compat;
    })
    (import "${sources.poetry2nix}/overlay.nix")
    (import "${sources.gomod2nix}/overlay.nix")
    (import ./build_overlay.nix)
    (pkgs: prev: {
      go = pkgs.go_1_23;
      lint-ci = pkgs.writeShellScriptBin "lint-ci" ''
        EXIT_STATUS=0
        ${pkgs.go}/bin/go mod verify || EXIT_STATUS=$?
      '';
      chain-maind-zemu = pkgs.callPackage ../. { ledger_zemu = true; };
      # chain-maind for integration test
      chain-maind-test = pkgs.callPackage ../. {
        ledger_zemu = true;
        coverage = true;
      };
    })
  ];
  config = { };
  inherit system;
}
