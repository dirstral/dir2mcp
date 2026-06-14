self:
{ config, lib, pkgs, ... }:

let
  cfg = config.services.dir2mcp;

  baseArgs = [
    "up"
    "--foreground"
    "--dir"
    cfg.rootDir
  ]
  ++ lib.optionals (cfg.stateDir != null) [ "--state-dir" cfg.stateDir ]
  ++ lib.optionals (cfg.listen != null) [ "--listen" cfg.listen ]
  ++ cfg.extraArgs;

  quotedArgs = lib.concatStringsSep " " (map lib.escapeShellArg baseArgs);

  # On darwin, launchd has no native EnvironmentFile, so we source the file in
  # a wrapper (same approach as the nix-darwin module). On linux, systemd's
  # native `EnvironmentFile=` is used instead and this wrapper does not need to
  # source anything — but using one wrapper for both keeps the ExecStart
  # identical across platforms.
  wrapper = pkgs.writeShellScript "dir2mcp-up" ''
    set -eu
    ${lib.optionalString (cfg.environmentFile != null && pkgs.stdenv.isDarwin) ''
      set -a
      . ${lib.escapeShellArg (toString cfg.environmentFile)}
      set +a
    ''}
    exec ${cfg.package}/bin/dir2mcp ${quotedArgs}
  '';
in
{
  options.services.dir2mcp = {
    enable = lib.mkEnableOption "the dir2mcp MCP knowledge server (home-manager service)";

    package = lib.mkOption {
      type = lib.types.package;
      default = self.packages.${pkgs.system}.default;
      defaultText = lib.literalExpression "dir2mcp.packages.\${system}.default";
      description = "The dir2mcp package to run.";
    };

    rootDir = lib.mkOption {
      type = lib.types.str;
      example = "/home/me/corpus";
      description = "Absolute path to the corpus directory to serve (dir2mcp `--dir`). Required when the service is enabled.";
    };

    stateDir = lib.mkOption {
      type = lib.types.nullOr lib.types.str;
      default = null;
      example = "/home/me/corpus/.dir2mcp";
      description = "State / cache directory (dir2mcp `--state-dir`). When null, dir2mcp derives `<rootDir>/.dir2mcp`.";
    };

    listen = lib.mkOption {
      type = lib.types.nullOr lib.types.str;
      default = null;
      example = "127.0.0.1:8765";
      description = "Listen address (dir2mcp `--listen`). When null, dir2mcp uses its built-in default.";
    };

    extraArgs = lib.mkOption {
      type = lib.types.listOf lib.types.str;
      default = [ ];
      example = [ "--public" "--auth" "auto" ];
      description = "Additional arguments appended to `dir2mcp up --foreground`.";
    };

    environmentFile = lib.mkOption {
      type = lib.types.nullOr lib.types.path;
      default = null;
      example = "/home/me/.config/dir2mcp/env";
      description = ''
        Path to a file containing `KEY=value` lines (e.g. MISTRAL_API_KEY,
        OPENAI_API_KEY, DIR2MCP_AUTH_TOKEN). On Linux it is wired to systemd's
        native `EnvironmentFile=`; on darwin it is sourced at start by a
        wrapper (launchd has no EnvironmentFile). Either way its contents are
        NOT copied into the nix store. Manage this file outside Nix with
        restrictive permissions.
      '';
    };
  };

  config = lib.mkIf cfg.enable {
    assertions = [
      {
        assertion = cfg.rootDir != "";
        message = "services.dir2mcp.rootDir must be set to the corpus directory when the service is enabled.";
      }
    ];

    # Darwin: home-manager installs a per-user LaunchAgent.
    launchd.agents.dir2mcp = lib.mkIf pkgs.stdenv.isDarwin {
      enable = true;
      config = {
        ProgramArguments = [ "${wrapper}" ];
        WorkingDirectory = cfg.rootDir;
        RunAtLoad = true;
        KeepAlive = true;
        ProcessType = "Background";
        StandardOutPath = "/tmp/dir2mcp.out.log";
        StandardErrorPath = "/tmp/dir2mcp.err.log";
      };
    };

    # Linux: home-manager installs a systemd --user unit. systemd supports
    # EnvironmentFile= natively, so secrets are loaded by systemd (not baked
    # into the store).
    systemd.user.services.dir2mcp = lib.mkIf pkgs.stdenv.isLinux {
      Unit = {
        Description = "dir2mcp MCP knowledge server";
        After = [ "network.target" ];
      };
      Service = {
        ExecStart = "${wrapper}";
        WorkingDirectory = cfg.rootDir;
        Restart = "on-failure";
        RestartSec = 5;
      } // lib.optionalAttrs (cfg.environmentFile != null) {
        EnvironmentFile = toString cfg.environmentFile;
      };
      Install = {
        WantedBy = [ "default.target" ];
      };
    };
  };
}
