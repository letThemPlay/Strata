# Host monitoring: Prometheus node exporter, reachable only through the
# management interface (see strata.networking).
{ config, lib, ... }:
let
  cfg = config.strata;
in
{
  options.strata.monitoring = {
    enable = lib.mkEnableOption "Strata host monitoring (Prometheus node exporter)";

    nodeExporter.port = lib.mkOption {
      type = lib.types.port;
      default = 9100;
      description = "TCP port of the node exporter on the management interface.";
    };
  };

  config = lib.mkIf cfg.monitoring.enable {
    assertions = [
      {
        assertion = cfg.enable;
        message = "strata.monitoring.enable requires strata.enable.";
      }
      {
        assertion = cfg.networking.enable;
        message = "strata.monitoring.enable requires strata.networking.enable (the exporter is only exposed on the management interface).";
      }
    ];

    services.prometheus.exporters.node = {
      enable = true;
      inherit (cfg.monitoring.nodeExporter) port;
      enabledCollectors = [ "systemd" ];
      openFirewall = false;
    };
  };
}
