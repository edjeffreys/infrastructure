variable "vm_id" {
  description = "Proxmox VM id"
  type        = number
}

variable "name" {
  description = "Name of VM"
  type        = string
}

variable "node" {
  description = "Proxmox node to launch VM on"
  type        = string
}

variable "bios" {
  type    = string
  default = "seabios"
}

variable "cpu_cores" {
  type    = number
  default = 2
}

variable "memory" {
  type    = number
  default = 2048
}

variable "disk_size" {
  type    = number
  default = 8
}

variable "vm_image" {
  type = string
}

variable "datastore" {
  description = "VM disk location source"
  type        = string
}

variable "cloud_init" {
  description = "Cloud init options"
  type = object({
    username = optional(string)
    ssh_keys = optional(list(string))
  })
  default = {}
}
