output "service_url" {
  description = "Public URL of the Cloud Run service. Feed this back in as base_url on the second apply, and use it to build the Strava callback URL."
  value       = google_cloud_run_v2_service.this.uri
}

output "runtime_service_account" {
  description = "Identity the service runs as."
  value       = google_service_account.runtime.email
}

output "deploy_service_account" {
  description = "Identity GitHub Actions assumes. Set this as the DEPLOY_SERVICE_ACCOUNT repository variable."
  value       = google_service_account.deploy.email
}

output "workload_identity_provider" {
  description = "Full provider resource name. Set this as the WIF_PROVIDER repository variable."
  value       = google_iam_workload_identity_pool_provider.github.name
}

output "artifact_repository" {
  description = "Artifact Registry repository images are pushed to."
  value       = "${google_artifact_registry_repository.containers.location}-docker.pkg.dev/${var.project_id}/${google_artifact_registry_repository.containers.repository_id}"
}

output "sweep_url" {
  description = "Full sweep URL, including its unguessable segment. Sensitive: anyone who can reach it can trigger a sweep, though Cloud Run still requires an OIDC token."
  value       = "${google_cloud_run_v2_service.this.uri}${local.sweep_path}"
  sensitive   = true
}
