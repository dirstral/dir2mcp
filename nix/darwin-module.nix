self:
{ config, lib, pkgs, ... }:

let
  cfg = config.services.dir2mcp;

  # launchd has no native EnvironmentFile facility (unlike systemd). To keep
  # secrets (MISTRAL_API_KEY, OPENAI_API_KEY, DIR2MCP_AUTH_TOKEN, ...) OUT of
  # the world-readable nix store, we never bake them into the plist. Instead we
  # generate a tiny wrapper script whose only nix-store-resident content is the
  # *path* to the operator-managed environmentFile; the secrets themselves are
  # sourced at runtime with `set -a` so they are exported into dir2mcp's
  # environment. dir2mcp's built-in providers read these via ${VAR} expansion.
  baseArgs = [
    "up"
    "--foreground"
    "--dir"
    cfg.rootDir
  ]
  ++ lib.optionals (cfg.stateDir != null) [ "--state-dir" cfg.stateDir ]
  ++ lib.optionals (cfg.listen != null) [ "--listen" cfg.listen ]
  ++ cfg.extraArgs;

  # Quote each argument for safe embedding in the shell wrapper.
  quotedArgs = lib.concatStringsSep " " (map lib.escapeShellArg baseArgs);

  wrapper = pkgs.writeShellScript "dir2mcp-up" ''
    set -eu
    ${lib.optionalString (cfg.environmentFile != null) ''
      set -a
      . ${lib.escapeShellArg (toString cfg.environmentFile)}
      set +a
    ''}
    exec ${cfg.package}/bin/dir2mcp ${quotedArgs}
  '';
in
{
  options.services.dir2mcp = {
    enable = lib.mkEnableOption "the dir2mcp MCP knowledge server (launchd-supervised)";

    package = lib.mkOption {
      type = lib.types.package;
      default = self.packages.${pkgs.system}.default;
      defaultText = lib.literalExpression "dir2mcp.packages.\${system}.default";
      description = "The dir2mcp package to run.";
    };

    rootDir = lib.mkOption {
      type = lib.types.str;
      example = "/Users/me/Documents/corpus";
      description = "Absolute path to the corpus directory to serve (dir2mcp `--dir`). Required when the service is enabled.";
    };

    stateDir = lib.mkOption {
      type = lib.types.nullOr lib.types.str;
      default = null;
      example = "/Users/me/Documents/corpus/.dir2mcp";
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
      example = "/Users/me/.config/dir2mcp/env";
      description = ''
        Path to a file containing `KEY=value` lines (e.g. MISTRAL_API_KEY,
        OPENAI_API_KEY, DIR2MCP_AUTH_TOKEN). The file is read at service start
        and its variables are exported into dir2mcp's environment. The file's
        *path* is referenced from the launchd job; its contents are NOT copied
        into the nix store, so secrets stay outside the world-readable store.
        Manage this file outside Nix (e.g. with restrictive permissions).
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

    # nix-darwin's `launchd.user.agents` installs a per-user LaunchAgent
    # (~/Library/LaunchAgents). This is the right scope for a personal corpus
    # server that needs the operator's home directory and credentials; use
    # `launchd.daemons` instead only for a system-wide (root) service.
    launchd.user.agents.dir2mcp = {
      serviceConfig = {
        ProgramArguments = [ "${wrapper}" ];
        WorkingDirectory = cfg.rootDir;
        RunAtLoad = true;
        KeepAlive = true;
        ProcessType = "Background";
        StandardOutPath = "/tmp/dir2mcp.out.log";
        StandardErrorPath = "/tmp/dir2mcp.err.log";
      };
    };
  };
}
