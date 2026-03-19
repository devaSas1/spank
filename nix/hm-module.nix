flake:

{ config, lib, pkgs, ... }:

let
  cfg = config.programs.nina;
  ninaPkg = flake.packages.${pkgs.stdenv.hostPlatform.system}.default;
in
{
  options.programs.nina = {
    enable = lib.mkEnableOption "nina - yells when you slap the laptop";

    package = lib.mkOption {
      type = lib.types.package;
      default = ninaPkg;
      defaultText = lib.literalExpression "inputs.nina.packages.\${system}.default";
      description = "The nina package to use.";
    };

    mode = lib.mkOption {
      type = lib.types.enum [ "pain" "sexy" "halo" ];
      default = "pain";
      description = "Audio mode to use.";
    };

    fast = lib.mkOption {
      type = lib.types.bool;
      default = false;
      description = "Enable faster detection tuning.";
    };

    minAmplitude = lib.mkOption {
      type = lib.types.nullOr lib.types.float;
      default = null;
      description = "Minimum amplitude threshold (0.0-1.0).";
    };

    cooldown = lib.mkOption {
      type = lib.types.nullOr lib.types.int;
      default = null;
      description = "Cooldown between responses in milliseconds.";
    };

    volumeScaling = lib.mkOption {
      type = lib.types.bool;
      default = false;
      description = "Scale playback volume by slap amplitude.";
    };

    customPath = lib.mkOption {
      type = lib.types.nullOr lib.types.path;
      default = null;
      description = "Path to custom MP3 audio directory.";
    };
  };

  config = lib.mkIf cfg.enable {
    home.packages = [ cfg.package ];
  };
}
