output "app_name" {
  description = "Fly.io application name"
  value       = fly_app.bosso.name
}

output "app_hostname" {
  description = "Fly.io application hostname"
  value       = "${fly_app.bosso.name}.fly.dev"
}
