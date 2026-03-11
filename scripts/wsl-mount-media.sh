#!/usr/bin/env bash
set -euo pipefail

ROOT="${WSL_MOUNT_ROOT:-/mnt/usbipd}"

mkdir -p "$ROOT"

mount_block_devices() {
  # Mount any unmounted /dev/sdXN partitions that look removable.
  # Note: "usbipd attach" hot-plugs devices; lsblk should see them.
  lsblk -prno NAME,TYPE,FSTYPE,MOUNTPOINT,SIZE | while read -r name type fstype mnt size; do
    [[ "$type" == "part" ]] || continue
    [[ -n "$fstype" ]] || continue
    [[ -z "${mnt:-}" ]] || continue

    # Heuristic: avoid mounting internal disks by only mounting partitions under USB transport.
    parent="/dev/$(lsblk -pno PKNAME "$name" 2>/dev/null || true)"
    [[ -n "$parent" && -e "$parent" ]] || continue
    tran="$(lsblk -pno TRAN "$parent" 2>/dev/null | tr -d ' ' || true)"
    [[ "$tran" == "usb" ]] || continue

    label="$(blkid -o value -s LABEL "$name" 2>/dev/null || true)"
    if [[ -z "$label" ]]; then
      label="$(basename "$name")"
    fi
    safe_label="$(echo "$label" | tr -cs 'A-Za-z0-9._-' '_' | sed 's/^_\\+//;s/_\\+$//')"
    target="$ROOT/usb/$safe_label"
    mkdir -p "$target"
    if mount "$name" "$target" 2>/dev/null || mount -o ro "$name" "$target" 2>/dev/null; then
      echo "$target"
    else
      if mountpoint -q "$target"; then
        echo "$target"
      fi
    fi
  done
}

mount_optical() {
  if [[ -e /dev/sr0 ]]; then
    mkdir -p "$ROOT/cdrom"
    if ! mountpoint -q "$ROOT/cdrom"; then
      if mount -t udf /dev/sr0 "$ROOT/cdrom" 2>/dev/null || mount -t iso9660 /dev/sr0 "$ROOT/cdrom" 2>/dev/null; then
        echo "$ROOT/cdrom"
      else
        if mountpoint -q "$ROOT/cdrom"; then
          echo "$ROOT/cdrom"
        fi
      fi
    else
      echo "$ROOT/cdrom"
    fi
  fi
}

mount_block_devices
mount_optical
