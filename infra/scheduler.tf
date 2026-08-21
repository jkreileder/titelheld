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

  # Paused. The handler exists, so this is no longer waiting on code - it is
  # unpaused by hand, deliberately, after the naming pipeline has been reviewed
  # end to end. Until then nothing fires, which is what keeps a service whose
  # min_instance_count is 0 genuinely idle, and the budget honest.
  paused = true

  retry_config {
    retry_count = 1
  }

  http_target {
    http_method = "POST"
    uri         = "${google_cloud_run_v2_service.this.uri}${local.sweep_path}"

    # The token is carried, but nothing on the platform checks it: while
    # allUsers holds roles/run.invoker, Cloud Run authenticates no one, so
    # this endpoint is reachable by anybody who learns the path.
    #
    # The sweep handler MUST therefore verify this OIDC token itself - issuer,
    # audience and the service account email below - and reject anything else.
    # The unguessable path is obfuscation, not authentication. See iam.tf and
    # README.md, "Why the service is publicly invokable".
    oidc_token {
      service_account_email = google_service_account.scheduler.email

      # The same local the service is told to expect. See run.tf.
      audience = local.sweep_audience
    }
  }

  depends_on = [google_project_service.this]
}
