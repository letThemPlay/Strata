# Strata "ext4" system disk layout (ADR-0012).
#
# A single GPT disk, UEFI only:
#
#   ESP   1 GiB   vfat  /boot   (systemd-boot, kernels and initrds)
#   root  rest    ext4  /
#
# Returns a value for `disko.devices`. Filesystems are mounted by GPT
# partition label (disko's default), so `device` is only used to find the
# disk at installation time; after that, renamed /dev/sdX nodes do not
# matter.
#
# WARNING: formatting with this layout destroys all data on `device`.
{ device }:
{
  disk.system = {
    type = "disk";
    inherit device;
    content = {
      type = "gpt";
      partitions = {
        ESP = {
          type = "EF00";
          size = "1G";
          content = {
            type = "filesystem";
            format = "vfat";
            mountpoint = "/boot";
            # Boot loader entries and the random seed must not be world
            # readable.
            mountOptions = [ "umask=0077" ];
          };
        };
        root = {
          size = "100%";
          content = {
            type = "filesystem";
            format = "ext4";
            mountpoint = "/";
          };
        };
      };
    };
  };
}
