{
  description = "Voice-powered typing for Hyprland/Wayland";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    flake-utils.url = "github:numtide/flake-utils";
  };

  outputs = { self, nixpkgs, flake-utils }:
    flake-utils.lib.eachDefaultSystem (system:
      let
        pkgs = nixpkgs.legacyPackages.${system};
      in
      {
        packages = {
          default = pkgs.buildGoModule {
            pname = "hyprvoice";
            version = "0.1.8";

            src = ./.;

            vendorHash = "sha256-qYZGccprn+pRbpVeO1qzSOb8yz/j/jdzPMxFyIB9BNA=";

            # Skip tests that require runtime tools (wl-copy, wtype)
            doCheck = false;

            nativeBuildInputs = with pkgs; [
              pkg-config
              makeWrapper
            ];

            buildInputs = with pkgs; [
              pipewire
            ];

            # Runtime dependencies
            makeWrapperArgs = [
              "--prefix PATH : ${pkgs.lib.makeBinPath [
                pkgs.pipewire
                pkgs.wl-clipboard
                pkgs.wtype
                pkgs.libnotify
              ]}"
            ];

            postInstall = ''
              wrapProgram $out/bin/hyprvoice \
                ''${makeWrapperArgs[@]}
            '';

            meta = with pkgs.lib; {
              description = "Voice-powered typing for Hyprland/Wayland";
              homepage = "https://github.com/leonardotrapani/hyprvoice";
              license = licenses.mit;
              maintainers = [ ];
              platforms = platforms.linux;
            };
          };
        };

        devShells.default = pkgs.mkShell {
          buildInputs = with pkgs; [
            go
            gopls
            gotools
            go-tools

            # Runtime dependencies
            pipewire
            wl-clipboard
            wtype
            libnotify

            # Development tools
            pkg-config
          ];

          shellHook = ''
            echo "Hyprvoice development environment"
            echo "Run 'go build ./cmd/hyprvoice' to build"
            echo ""
            echo "Available commands:"
            echo "  go build ./cmd/hyprvoice  - Build the binary"
            echo "  go test ./...             - Run tests"
            echo "  ./hyprvoice serve         - Start daemon"
            echo "  ./hyprvoice configure     - Interactive setup"
          '';
        };

        apps.default = {
          type = "app";
          program = "${self.packages.${system}.default}/bin/hyprvoice";
        };
      }
    ) // {
      # NixOS module for system-wide installation
      nixosModules.default = { config, lib, pkgs, ... }:
        with lib;
        let
          cfg = config.services.hyprvoice;
        in
        {
          options.services.hyprvoice = {
            enable = mkEnableOption "hyprvoice voice-to-text service";

            package = mkOption {
              type = types.package;
              default = self.packages.${pkgs.system}.default;
              description = "The hyprvoice package to use";
            };
          };

          config = mkIf cfg.enable {
            environment.systemPackages = [ cfg.package ];

            # Ensure required services are available
            services.pipewire.enable = mkDefault true;

            # User service configuration
            systemd.user.services.hyprvoice = {
              description = "Hyprvoice voice-to-text daemon";
              wantedBy = [ "default.target" ];
              after = [ "pipewire.service" ];

              serviceConfig = {
                Type = "simple";
                ExecStart = "${cfg.package}/bin/hyprvoice serve";
                Restart = "on-failure";
                RestartSec = "5s";
              };
            };
          };
        };

      # Home Manager module
      homeManagerModules.default = { config, lib, pkgs, ... }:
        with lib;
        let
          cfg = config.services.hyprvoice;
        in
        {
          options.services.hyprvoice = {
            enable = mkEnableOption "hyprvoice voice-to-text service";

            package = mkOption {
              type = types.package;
              default = self.packages.${pkgs.system}.default;
              description = "The hyprvoice package to use";
            };

            settings = mkOption {
              type = types.attrs;
              default = { };
              description = "Configuration for hyprvoice";
              example = literalExpression ''
                {
                  transcription = {
                    provider = "openai";
                    api_key = "sk-...";
                    language = "";
                    model = "whisper-1";
                  };
                  injection = {
                    mode = "fallback";
                    restore_clipboard = true;
                  };
                }
              '';
            };
          };

          config = mkIf cfg.enable {
            home.packages = [ cfg.package ];

            systemd.user.services.hyprvoice = {
              Unit = {
                Description = "Hyprvoice voice-to-text daemon";
                After = [ "pipewire.service" ];
              };

              Service = {
                Type = "simple";
                ExecStart = "${cfg.package}/bin/hyprvoice serve";
                Restart = "on-failure";
                RestartSec = "5s";
              };

              Install = {
                WantedBy = [ "default.target" ];
              };
            };

            xdg.configFile."hyprvoice/config.toml" = mkIf (cfg.settings != { }) {
              source = (pkgs.formats.toml { }).generate "config.toml" cfg.settings;
            };
          };
        };
    };
}
