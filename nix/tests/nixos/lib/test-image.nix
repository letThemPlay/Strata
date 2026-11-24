# A minimal Incus container image for VM tests: static busybox and an init
# that idles. Built offline; no downloads.
{ pkgs }:
pkgs.runCommand "strata-incus-test-image" { nativeBuildInputs = [ pkgs.xz ]; } ''
  mkdir -p rootfs/bin rootfs/sbin rootfs/etc rootfs/dev rootfs/proc rootfs/sys rootfs/tmp rootfs/root
  cp ${pkgs.pkgsStatic.busybox}/bin/busybox rootfs/bin/busybox
  for applet in sh cat echo id ls sleep true ip ping; do ln -s busybox rootfs/bin/$applet; done
  printf '#!/bin/sh\nwhile true; do sleep 3600; done\n' > rootfs/sbin/init
  chmod 755 rootfs/sbin/init
  cat > metadata.yaml <<EOF
  architecture: ${pkgs.stdenv.hostPlatform.uname.processor}
  creation_date: 1700000000
  properties:
    description: Strata test image (busybox)
  EOF
  mkdir $out
  tar --owner=0 --group=0 -cJf $out/metadata.tar.xz metadata.yaml
  tar --owner=0 --group=0 -cJf $out/rootfs.tar.xz -C rootfs .
''
