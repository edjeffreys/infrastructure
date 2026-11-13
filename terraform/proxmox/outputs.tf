locals {
  vms = [
    proxmox_virtual_environment_vm.talos-cp-0,
    proxmox_virtual_environment_vm.talos-cp-1,
    proxmox_virtual_environment_vm.talos-cp-2,
    proxmox_virtual_environment_vm.talos-worker-0,
    proxmox_virtual_environment_vm.talos-worker-1,
    proxmox_virtual_environment_vm.talos-worker-2,
    proxmox_virtual_environment_vm.talos-worker-pia-0,
    proxmox_virtual_environment_vm.talos-worker-pia-1,
  ]
}

# The firewall workspace reads this through remote state for its DHCP reservations.
output "vms" {
  description = "Each VM's ID, VLAN and MAC address, by name."
  value = {
    for vm in local.vms : vm.name => {
      vm_id   = vm.vm_id
      vlan_id = vm.network_device[0].vlan_id
      # Proxmox reports MACs upper case; UniFi stores them lower case.
      mac = lower(vm.network_device[0].mac_address)
    }
  }
}
