terraform {
  required_version = ">= 1.10.5"
  required_providers {
    proxmox = {
      source  = "bpg/proxmox"
      version = "0.111.1"
    }
  }
}

provider "proxmox" {
  endpoint = var.proxmox_endpoint

  # TODO we should probably use api_token and ssh keys
  # api_token = var.proxmox_api_token
  #
  username = var.proxmox_username
  password = var.proxmox_password

  insecure = var.proxmox_insecure
  tmp_dir  = "/var/tmp"
}
