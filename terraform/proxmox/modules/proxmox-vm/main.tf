resource "proxmox_virtual_environment_vm" "vm" {
  name      = var.name
  node_name = var.node
  vm_id     = var.vm_id

  bios = var.bios

  network_device {
    bridge  = "vmbr0"
    vlan_id = 10
  }

  on_boot = true

  operating_system {
    type = "l26"
  }

  cpu {
    cores   = var.cpu_cores
    sockets = 1
    type    = "host"
  }

  memory {
    dedicated = var.memory
  }

  disk {
    datastore_id = var.datastore
    file_id      = var.vm_image
    interface    = "virtio0"
    size         = var.disk_size
  }

  dynamic "initialization" {
    for_each = var.cloud_init[*]
    content {
      ip_config {
        ipv4 {
          address = "dhcp"
        }
      }

      user_account {
        username = var.cloud_init.username
        keys     = var.cloud_init.ssh_keys
      }
    }
  }
}
