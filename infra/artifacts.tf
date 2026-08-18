# Somewhere for CI to push the image Cloud Run runs.
#
# Not in the enumerated list for this phase, but a deploy job with nowhere to
# push is fiction, and Cloud Run cannot start without an image.
resource "google_artifact_registry_repository" "containers" {
  project       = var.project_id
  location      = var.region
  repository_id = "containers"
  format        = "DOCKER"
  description   = "Container images for the Cloud Run service."

  # The service keeps one revision; there is no reason to accumulate images.
  cleanup_policies {
    id     = "keep-recent"
    action = "KEEP"

    most_recent_versions {
      keep_count = 5
    }
  }

  depends_on = [google_project_service.this]
}
