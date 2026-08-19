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

  # Two policies, because they do different jobs: KEEP protects the five newest
  # images from deletion, and DELETE is what actually removes anything. A KEEP
  # policy on its own deletes nothing at all, and the repository grows until it
  # costs money.
  cleanup_policies {
    id     = "keep-recent"
    action = "KEEP"

    most_recent_versions {
      keep_count = 5
    }
  }

  cleanup_policies {
    id     = "delete-stale"
    action = "DELETE"

    condition {
      tag_state  = "ANY"
      older_than = "2592000s" # 30 days
    }
  }

  depends_on = [google_project_service.this]
}
