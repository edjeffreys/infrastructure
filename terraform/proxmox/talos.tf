locals {
  pve_node = "proxmox-mini-1"

  # MUST be kept in step with talosVersion in talos/talconfig.yaml. Renovate
  # bumps both together (see the siderolabs/installer regexManagers in
  # renovate.json) — do not edit this by hand alone.
  #
  # This is the ISO a VM falls back to when its install disk has no bootable
  # system, i.e. maintenance mode. boot_order pins virtio0 so a healthy node
  # boots off disk — but re-attaching the media is enough to send it back into
  # the installer on next reboot, which is why the VMs ignore cdrom changes.
  #
  # Letting this drift behind talconfig.yaml is invisible until a node needs
  # reinstalling, and then the stale ISO refuses the config: Talos validates
  # the machine config against the running maintenance kernel, not against the
  # version being installed. v1.12.6 media rejected a v1.36.1 Kubernetes config
  # and blocked talos-worker-0's rebuild entirely.
  talos_version = "v1.13.10"
  talos_disk_store = "local-lvm"

  # Worker schematic: i915, intel-ucode, iscsi-tools, nfs-utils,
  # qemu-guest-agent, tailscale, util-linux-tools.
  #
  # MUST match the worker systemExtensions list in talos/talconfig.yaml — the
  # ID is a hash of that list, and talhelper derives the installer image from
  # talconfig while this derives the maintenance-mode ISO. Drift between them
  # is invisible until a node is rebuilt, and then it comes back missing
  # extensions.
  #
  # The hash covers list ORDER, not just membership, so don't hand-compute it
  # from a re-sorted list. Take it from:
  #   talhelper genurl installer -c talos/talconfig.yaml
  talos_worker_schematic_id = "bb2bf97151447e3e0b8e3b0726805f1050a59e4e780b366211ba71bc19c07a7c"
  talos_worker_image_url    = "https://factory.talos.dev/image/${local.talos_worker_schematic_id}/${local.talos_version}/metal-amd64.iso"

}

# --- Talos ISO ---
# Downloaded to ISO storage (local:iso/) and attached as CDROM.
# Talos boots from CDROM, installs to the blank VirtIO disk, then registers
# a UEFI boot entry — subsequent reboots use the VirtIO disk automatically.
#
# file_name is deliberately NOT versioned. The VMs pin cdrom in
# ignore_changes (re-attaching media makes them boot the installer again),
# so the file_id they reference must stay constant for the lifetime of the
# VM. Bumping talos_version therefore changes the download URL but keeps the
# path: the same local:iso/talos-worker.iso is replaced with newer content,
# and every VM picks up the new version the next time it drops to
# maintenance mode — without Terraform touching a single VM resource.
#
# A versioned file_name would destroy the old ISO on every bump and leave
# the cdrom-ignoring VMs pointing at a file that no longer exists, which
# Proxmox refuses to start.

resource "proxmox_download_file" "talos_worker_image" {
  content_type        = "iso"
  datastore_id        = "local"
  node_name           = local.pve_node
  url                 = local.talos_worker_image_url
  file_name           = "talos-worker.iso"
  overwrite_unmanaged = false
}

# --- Talos Control Plane ---

resource "proxmox_virtual_environment_vm" "talos-cp-0" {
  name      = "talos-cp-0"
  node_name = local.pve_node
  vm_id     = 630

  bios       = "ovmf"
  machine    = "q35"
  on_boot    = true
  boot_order = ["virtio0"]

  agent {
    enabled = true
  }

  lifecycle {
    ignore_changes = [initialization, clone, boot_order]
  }

  operating_system {
    type = "l26"
  }

  cpu {
    cores   = 2
    sockets = 1
    type    = "host"
  }

  memory {
    dedicated = 4096
  }

  network_device {
    bridge  = "vmbr0"
    vlan_id = 10
  }

  efi_disk {
    datastore_id      = local.talos_disk_store
    pre_enrolled_keys = false
    type              = "4m"
  }

  disk {
    datastore_id = local.talos_disk_store
    interface    = "virtio0"
    size         = 20
    file_format  = "raw"
  }
}

locals {
  worker_disk_size = 64
}

# --- Talos Standard Worker ---

resource "proxmox_virtual_environment_vm" "talos-worker-0" {
  name      = "talos-worker-0"
  node_name = local.pve_node
  vm_id     = 631

  bios       = "ovmf"
  machine    = "q35"
  on_boot    = true
  boot_order = ["virtio0"]

  agent {
    enabled = true
  }

  lifecycle {
    # cdrom is ignored: re-attaching media makes the VM boot the installer
    # again on next reboot instead of the installed system on virtio0.
    # The ISO is kept current by content, not by re-attachment — see the
    # stable file_name on proxmox_download_file.talos_worker_image.
    ignore_changes = [initialization, clone, cdrom]
  }

  operating_system {
    type = "l26"
  }

  cpu {
    cores   = 4
    sockets = 1
    type    = "host"
  }

  memory {
    dedicated = 8192
  }

  # Intel iGPU (Alder Lake-P, 8086:46a6) passed through for Plex QuickSync
  # transcoding. Plain passthrough is exclusive — this is the ONLY VM that can
  # hold the device, which is why plex carries `nodeSelector: gpu: intel` and
  # this node alone carries that label in talconfig.yaml.
  #
  # No x-vga: the guest drives the render node (/dev/dri/renderD128) for
  # compute only and never initialises a display, so it needs no vBIOS/GOP ROM
  # shim. Passing x-vga=1 asks Proxmox for primary-GPU semantics the headless
  # Talos guest cannot satisfy.
  #
  # The host must have the device bound to vfio-pci before this VM starts
  # (intel_iommu=on, i915 blacklisted, softdep i915 pre: vfio-pci) — otherwise
  # the host's i915 holds it and the VM refuses to start. Passthrough also pins
  # the whole 8 GiB of guest RAM in the host; it is no longer reclaimable.
  hostpci {
    device = "hostpci0"
    id     = "0000:00:02.0"
    pcie   = true
    rombar = true
  }

  network_device {
    bridge  = "vmbr0"
    vlan_id = 11
  }

  efi_disk {
    datastore_id      = local.talos_disk_store
    pre_enrolled_keys = false
    type              = "4m"
  }

  cdrom {
    file_id   = proxmox_download_file.talos_worker_image.id
    interface = "ide2"
  }

  disk {
    datastore_id = local.talos_disk_store
    interface    = "virtio0"
    size         = local.worker_disk_size
    file_format  = "raw"
  }
}

# --- Talos PIA Worker ---

resource "proxmox_virtual_environment_vm" "talos-worker-pia-0" {
  name      = "talos-worker-pia-0"
  node_name = local.pve_node
  vm_id     = 632

  bios       = "ovmf"
  machine    = "q35"
  on_boot    = true
  boot_order = ["virtio0"]

  agent {
    enabled = true
  }

  lifecycle {
    # cdrom is ignored: re-attaching media makes the VM boot the installer
    # again on next reboot instead of the installed system on virtio0.
    # The ISO is kept current by content, not by re-attachment — see the
    # stable file_name on proxmox_download_file.talos_worker_image.
    ignore_changes = [initialization, clone, cdrom]
  }

  operating_system {
    type = "l26"
  }

  cpu {
    cores   = 2
    sockets = 1
    type    = "host"
  }

  memory {
    dedicated = 8192
  }

  network_device {
    bridge  = "vmbr0"
    vlan_id = 200
  }

  efi_disk {
    datastore_id      = local.talos_disk_store
    pre_enrolled_keys = false
    type              = "4m"
  }

  cdrom {
    file_id   = proxmox_download_file.talos_worker_image.id
    interface = "ide2"
  }

  disk {
    datastore_id = local.talos_disk_store
    interface    = "virtio0"
    size         = local.worker_disk_size
    file_format  = "raw"
  }
}

# --- Talos CP-1 ---

resource "proxmox_virtual_environment_vm" "talos-cp-1" {
  name      = "talos-cp-1"
  node_name = local.pve_node
  vm_id     = 633

  bios       = "ovmf"
  machine    = "q35"
  on_boot    = true
  boot_order = ["virtio0"]

  agent {
    enabled = true
  }

  lifecycle {
    ignore_changes = [initialization, clone, boot_order]
  }

  operating_system {
    type = "l26"
  }

  cpu {
    cores   = 2
    sockets = 1
    type    = "host"
  }

  memory {
    dedicated = 4096
  }

  network_device {
    bridge  = "vmbr0"
    vlan_id = 10
  }

  efi_disk {
    datastore_id      = local.talos_disk_store
    pre_enrolled_keys = false
    type              = "4m"
  }

  disk {
    datastore_id = local.talos_disk_store
    interface    = "virtio0"
    size         = 20
    file_format  = "raw"
  }
}

# --- Talos CP-2 ---

resource "proxmox_virtual_environment_vm" "talos-cp-2" {
  name      = "talos-cp-2"
  node_name = local.pve_node
  vm_id     = 634

  bios       = "ovmf"
  machine    = "q35"
  on_boot    = true
  boot_order = ["virtio0"]

  agent {
    enabled = true
  }

  lifecycle {
    ignore_changes = [initialization, clone, boot_order]
  }

  operating_system {
    type = "l26"
  }

  cpu {
    cores   = 2
    sockets = 1
    type    = "host"
  }

  memory {
    dedicated = 4096
  }

  network_device {
    bridge  = "vmbr0"
    vlan_id = 10
  }

  efi_disk {
    datastore_id      = local.talos_disk_store
    pre_enrolled_keys = false
    type              = "4m"
  }

  disk {
    datastore_id = local.talos_disk_store
    interface    = "virtio0"
    size         = 20
    file_format  = "raw"
  }
}

# --- Talos Standard Worker-1 ---

resource "proxmox_virtual_environment_vm" "talos-worker-1" {
  name      = "talos-worker-1"
  node_name = local.pve_node
  vm_id     = 635

  bios       = "ovmf"
  machine    = "q35"
  on_boot    = true
  boot_order = ["virtio0"]

  agent {
    enabled = true
  }

  operating_system {
    type = "l26"
  }

  cpu {
    cores   = 8
    sockets = 1
    type    = "host"
  }

  memory {
    dedicated = 8192
  }

  network_device {
    bridge  = "vmbr0"
    vlan_id = 11
  }

  efi_disk {
    datastore_id      = local.talos_disk_store
    pre_enrolled_keys = false
    type              = "4m"
  }

  cdrom {
    file_id   = proxmox_download_file.talos_worker_image.id
    interface = "ide2"
  }

  disk {
    datastore_id = local.talos_disk_store
    interface    = "virtio0"
    size         = local.worker_disk_size
    file_format  = "raw"
  }
}

# --- Talos Standard Worker-2 ---

resource "proxmox_virtual_environment_vm" "talos-worker-2" {
  name      = "talos-worker-2"
  node_name = local.pve_node
  vm_id     = 636

  bios       = "ovmf"
  machine    = "q35"
  on_boot    = true
  boot_order = ["virtio0"]

  agent {
    enabled = true
  }

  operating_system {
    type = "l26"
  }

  cpu {
    cores   = 8
    sockets = 1
    type    = "host"
  }

  memory {
    dedicated = 8192
  }

  network_device {
    bridge  = "vmbr0"
    vlan_id = 11
  }

  efi_disk {
    datastore_id      = local.talos_disk_store
    pre_enrolled_keys = false
    type              = "4m"
  }

  cdrom {
    file_id   = proxmox_download_file.talos_worker_image.id
    interface = "ide2"
  }

  disk {
    datastore_id = local.talos_disk_store
    interface    = "virtio0"
    size         = local.worker_disk_size
    file_format  = "raw"
  }
}

# --- Talos PIA Worker-1 ---

resource "proxmox_virtual_environment_vm" "talos-worker-pia-1" {
  name      = "talos-worker-pia-1"
  node_name = local.pve_node
  vm_id     = 637

  bios       = "ovmf"
  machine    = "q35"
  on_boot    = true
  boot_order = ["virtio0"]

  agent {
    enabled = true
  }

  operating_system {
    type = "l26"
  }

  cpu {
    cores   = 4
    sockets = 1
    type    = "host"
  }

  memory {
    dedicated = 8192
  }

  network_device {
    bridge  = "vmbr0"
    vlan_id = 200
  }

  efi_disk {
    datastore_id      = local.talos_disk_store
    pre_enrolled_keys = false
    type              = "4m"
  }

  cdrom {
    file_id   = proxmox_download_file.talos_worker_image.id
    interface = "ide2"
  }

  disk {
    datastore_id = local.talos_disk_store
    interface    = "virtio0"
    size         = local.worker_disk_size
    file_format  = "raw"
  }
}
