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

        # Runtime dependencies for wrapping
        runtimeDeps = with pkgs; [
          pipewire
          wl-clipboard
          wtype
          libnotify
        ];

        # Build hyprvoice package
        buildHyprvoice = { doCheck ? false }: pkgs.buildGoModule {
          pname = "hyprvoice";
          version = "0.1.8";

          src = ./.;

          vendorHash = "sha256-qYZGccprn+pRbpVeO1qzSOb8yz/j/jdzPMxFyIB9BNA=";

          inherit doCheck;

          nativeBuildInputs = with pkgs; [
            pkg-config
            makeWrapper
          ] ++ pkgs.lib.optionals doCheck runtimeDeps;

          buildInputs = with pkgs; [
            pipewire
          ];

          # Make runtime tools available during tests
          preCheck = pkgs.lib.optionalString doCheck ''
            export PATH="${pkgs.lib.makeBinPath runtimeDeps}:$PATH"
          '';

          # Runtime dependencies
          makeWrapperArgs = [
            "--prefix PATH : ${pkgs.lib.makeBinPath runtimeDeps}"
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
            mainProgram = "hyprvoice";
          };
        };
      in
      {
        packages = {
          default = buildHyprvoice { };
          hyprvoice = buildHyprvoice { };
        };

        # Checks for CI/CD
        checks = {
          # Build check - ensures the package builds successfully
          build = self.packages.${system}.default;

          # Unit tests with runtime dependencies available
          test = buildHyprvoice { doCheck = true; };
        };

        devShells.default = pkgs.mkShell {
          buildInputs = with pkgs; [
            go
            gopls
            gotools
            go-tools
            golangci-lint

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
            echo "  nix flake check           - Run all checks"
            echo "  nix fmt                   - Format code"
            echo "  ./hyprvoice serve         - Start daemon"
            echo "  ./hyprvoice configure     - Interactive setup"
          '';
        };

        # Formatter for 'nix fmt'
        formatter = pkgs.writeShellScriptBin "formatter" ''
          ${pkgs.go}/bin/gofmt -w .
          ${pkgs.nixpkgs-fmt}/bin/nixpkgs-fmt flake.nix
        '';

        apps = {
          default = {
            type = "app";
            program = "${self.packages.${system}.default}/bin/hyprvoice";
          };
          hyprvoice = {
            type = "app";
            program = "${self.packages.${system}.default}/bin/hyprvoice";
          };
          serve = {
            type = "app";
            program = "${pkgs.writeShellScript "hyprvoice-serve" ''
              exec ${self.packages.${system}.default}/bin/hyprvoice serve "$@"
            ''}";
          };
          configure = {
            type = "app";
            program = "${pkgs.writeShellScript "hyprvoice-configure" ''
              exec ${self.packages.${system}.default}/bin/hyprvoice configure "$@"
            ''}";
          };
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

            autoStart = mkOption {
              type = types.bool;
              default = false;
              description = ''
                Automatically start hyprvoice service on login (graphical-session.target).
                If false, you should start the service manually or via compositor exec-once.
                Example: exec-once = uwsm-app -- systemctl --user start hyprvoice.service
              '';
            };

            environmentFile = mkOption {
              type = types.nullOr types.path;
              default = null;
              description = ''
                Path to a file containing environment variables (like API keys).
                Format: KEY=value (one per line, no 'export' keyword).
                Example: OPENAI_API_KEY=sk-...
              '';
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
                After = [ "pipewire.service" "graphical-session.target" ];
                PartOf = [ "graphical-session.target" ];
              };

              Service = ({
                Type = "simple";
                # Wait for Wayland display to be available before starting
                ExecStartPre = "/bin/sh -c 'until [ -n \"$WAYLAND_DISPLAY\" ]; do sleep 0.1; done'";
                ExecStart = "${cfg.package}/bin/hyprvoice serve";
                Restart = "on-failure";
                RestartSec = "5s";
                # Pass Wayland session environment for clipboard/wtype tools
                PassEnvironment = "WAYLAND_DISPLAY DISPLAY";
              } // optionalAttrs (cfg.environmentFile != null) {
                EnvironmentFile = cfg.environmentFile;
              });

              Install = mkIf cfg.autoStart {
                WantedBy = [ "graphical-session.target" ];
              };
            };

            xdg.configFile."hyprvoice/config.toml" = mkIf (cfg.settings != { }) {
              source = (pkgs.formats.toml { }).generate "config.toml" cfg.settings;
            };
          };
        };

      # Overlay for easy integration into existing nixpkgs
      overlays.default = final: prev: {
        hyprvoice = self.packages.${final.system}.default;
      };
    };
}
