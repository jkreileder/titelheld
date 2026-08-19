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
  # One release pushes several versions, not one: the OCI index, the amd64
  # manifest, BuildKit's provenance and SBOM attestations, and the Sigstore
  # referrer the attest job pushes. A keep_count of 5 therefore protected
  # barely a single release.
  cleanup_policies {
    id     = "keep-recent"
    action = "KEEP"

    most_recent_versions {
      keep_count = 50
    }
  }

  # Untagged only. Rolling back means deploying an old digest - that is the
  # documented recovery path - so deleting a released image by age would
  # remove the thing recovery depends on. Untagged leftovers are safe to
  # reap; tagged releases are kept until keep-recent ages them out.
  cleanup_policies {
    id     = "delete-stale"
    action = "DELETE"

    condition {
      tag_state  = "UNTAGGED"
      older_than = "7776000s" # 90 days
    }
  }

  depends_on = [google_project_service.this]
}
