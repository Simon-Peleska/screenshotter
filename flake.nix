{
  description = "screenshotter";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    flake-utils.url = "github:numtide/flake-utils";
    go-test-tui = {
      url = "github:Simon-Peleska/go-test-tui";
      inputs.nixpkgs.follows = "nixpkgs";
      inputs.flake-utils.follows = "flake-utils";
    };
    mcp-dap-server-src = {
      url = "github:go-delve/mcp-dap-server";
      flake = false;
    };
  };

  outputs =
    {
      self,
      nixpkgs,
      flake-utils,
      go-test-tui,
      mcp-dap-server-src,
    }:
    flake-utils.lib.eachDefaultSystem (
      system:
      let
        pkgs = nixpkgs.legacyPackages.${system};

        mcp-dap-server = pkgs.buildGoModule {
          pname = "mcp-dap-server";
          version = "unstable";
          src = mcp-dap-server-src;
          vendorHash = "sha256-RpofdCGXwakl+ouhPEjrPjB+4uLhNrPNFpztEOxaJf0=";
          doCheck = false;
        };

        # gosseract's vendored CGO flags hardcode /usr/local/{include,lib}.
        # Override by prepending the Nix store paths.
        tessEnv = {
          CGO_CPPFLAGS = "-I${pkgs.tesseract}/include -I${pkgs.leptonica}/include";
          CGO_LDFLAGS = "-L${pkgs.tesseract}/lib -L${pkgs.leptonica}/lib";
        };

      in
      {
        packages.default = pkgs.buildGoModule (
          tessEnv
          // {
            pname = "screenshotter";
            version = "0.0.1";
            src = ./.;
            vendorHash = null;
            nativeBuildInputs = [ pkgs.pkg-config ];
            buildInputs = [
              pkgs.tesseract
              pkgs.leptonica
            ];
          }
        );

        apps.default = {
          type = "app";
          program = "${self.packages.${system}.default}/bin/screenshotter";
        };

        devShells.default = pkgs.mkShell (
          tessEnv
          // {
            packages = with pkgs; [
              go
              pkg-config
              tesseract
              leptonica
              delve
              mcp-dap-server
              go-test-tui.packages.${system}.default
            ];

            shellHook = ''
              echo "screenshotter dev shell"
              echo "  go build ./...          - build"
              echo "  go test ./...           - test"
              echo "  ./screenshotter         - fullscreen screenshot"
              echo "  ./screenshotter -region - region screenshot"
              echo "  ./screenshotter -ocr    - OCR to clipboard"
            '';
          }
        );
      }
    );
}
