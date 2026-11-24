{
  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    flake-utils.url = "github:numtide/flake-utils";
  };
  outputs =
    {
      self,
      nixpkgs,
      flake-utils,
      ...
    }:
    # nixpkgs unstable no longer supports x86_64-darwin.
    flake-utils.lib.eachSystem [ "x86_64-linux" "aarch64-linux" "aarch64-darwin" ] (
      system:
      let
        pkgs = import nixpkgs { inherit system; };
        # The upstream release this branch is based on.
        release = "0.3.0";
        # A prerelease version, which turns off the update notifier.
        version = "${release}-drew.${builtins.substring 0 8 self.lastModifiedDate}";
        gh-stack = pkgs.buildGoModule {
          pname = "gh-stack";
          inherit version;
          src = self;
          vendorHash = "sha256-y+eTfmZTdSkHh943o/XtFrrMWv/lxvWekKUp/JKb0h4=";
          # The CI test runner is a main package and would install as `test`.
          excludedPackages = [ "scripts/test" ];
          ldflags = [
            "-s"
            "-X=github.com/github/gh-stack/cmd.Version=${version}"
          ];
          nativeBuildInputs = [ pkgs.installAgentSkills ];
          nativeCheckInputs = [ pkgs.gitMinimal ];
          # The tests have only run in the Nix sandbox on Linux.
          doCheck = pkgs.stdenv.hostPlatform.isLinux;
          # Some tests resolve branches and need to run inside a repository.
          preCheck = ''
            git init --quiet
          '';
          nativeInstallCheckInputs = [ pkgs.versionCheckHook ];
          doInstallCheck = true;
          __structuredAttrs = true;
          meta = {
            description = "GitHub CLI extension to use stacked PRs";
            homepage = "https://github.com/drew-council/gh-stack";
            license = pkgs.lib.licenses.mit;
            mainProgram = "gh-stack";
          };
        };
      in
      {
        devShells.default = pkgs.mkShell {
          packages = with pkgs; [
            git
            go
            gopls
          ];
        };

        packages = {
          inherit gh-stack;
          default = gh-stack;
        };

        checks = { inherit gh-stack; };

        formatter = pkgs.nixfmt-tree;
      }
    );
}
