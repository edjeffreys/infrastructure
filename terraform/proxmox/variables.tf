variable "proxmox_api_token" {
  description = "auth token to run and apply plan on proxmox node"
  type        = string
}

variable "proxmox_username" {
  description = "proxmox username"
  type        = string
}

variable "proxmox_password" {
  description = "proxmox password"
  type        = string
}

variable "proxmox_endpoint" {
  description = "target proxmox node"
  type        = string
}

variable "proxmox_insecure" {
  description = "false if endpoint it https"
  type        = bool
  default     = true
}
