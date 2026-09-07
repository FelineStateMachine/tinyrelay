{ pkgs, ... }:
{
  fileSystems."/srv/tiny" = {
    device = "/dev/disk/by-uuid/98f49cba-bd8c-432d-a91a-41db66920e90";
    fsType = "ext4";
    options = [ "noatime" "nofail" "x-systemd.device-timeout=15s" ];
  };

  services.fstrim.enable = true;

  systemd.services.tiny-certificates = {
    description = "Renew tiny HTTPS certificates";
    after = [ "tiny.service" ];
    requires = [ "tiny.service" ];
    unitConfig.RequiresMountsFor = "/srv/tiny";
    path = [ pkgs.bash pkgs.coreutils pkgs.docker pkgs.openssl pkgs.util-linux ];
    serviceConfig = {
      Type = "oneshot";
      ExecStart = "${pkgs.bash}/bin/bash /srv/tiny/deploy/renew-certificates.sh";
    };
  };

  systemd.timers.tiny-certificates = {
    wantedBy = [ "timers.target" ];
    timerConfig = {
      OnCalendar = "daily";
      Persistent = true;
      RandomizedDelaySec = "1h";
    };
  };

  systemd.services.tiny = {
    description = "Personal tiny relay";
    wantedBy = [ "multi-user.target" ];
    after = [ "docker.service" "network-online.target" "srv-tiny.mount" ];
    requires = [ "docker.service" ];
    bindsTo = [ "srv-tiny.mount" ];
    wants = [ "network-online.target" ];
    unitConfig.RequiresMountsFor = "/srv/tiny";
    path = [ pkgs.docker pkgs.docker-compose pkgs.util-linux ];
    preStart = ''
      test "$(findmnt -n -o UUID --target /srv/tiny)" = 98f49cba-bd8c-432d-a91a-41db66920e90
    '';
    serviceConfig = {
      Type = "oneshot";
      RemainAfterExit = true;
      WorkingDirectory = "/srv/tiny/deploy";
      ExecStart = "${pkgs.docker-compose}/bin/docker-compose -f compose.yaml up -d --no-build";
      ExecStop = "${pkgs.docker-compose}/bin/docker-compose -f compose.yaml down --timeout 30";
      TimeoutStartSec = 180;
      TimeoutStopSec = 90;
    };
  };
}
