# The delay queue is drained by a scheduled sweep rather than by Cloud Tasks:
# no second service, no client library, and a failed activity simply stays
# queued until the next run instead of needing its own retry policy. Ten-minute
# precision makes a five-minute cron more than fine.
resource "google_cloud_scheduler_job" "sweep" {
  project     = var.project_id
  region      = var.region
  name        = "titelheld-sweep"
  description = "Drains the delay queue: names activities whose processing delay has elapsed."
  schedule    = var.sweep_schedule
  time_zone   = "Etc/UTC"

  attempt_deadline = "320s"

  retry_config {
    retry_count = 1
  }

  http_target {
    http_method = "POST"
    uri         = "${google_cloud_run_v2_service.this.uri}${local.sweep_path}"

    # Two independent gates, the same pattern as the webhook: the path segment
    # is unguessable, and the request additionally carries an OIDC token that
    # Cloud Run checks before the request reaches the process.
    oidc_token {
      service_account_email = google_service_account.scheduler.email
      audience              = google_cloud_run_v2_service.this.uri
    }
  }

  depends_on = [google_project_service.this]
}
