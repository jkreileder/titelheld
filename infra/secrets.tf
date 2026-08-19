# Secret *resources* only. No google_secret_manager_secret_version appears
# anywhere in this configuration, and none should: adding a version here would
# put the value in the plan, in state, and in whatever tfvars fed it.
#
# Values are added out of band, once, per the README:
#   printf %s "$VALUE" | gcloud secrets versions add strava-client-secret --data-file=-
locals {
  secrets = [
    "strava-client-id",
    "strava-client-secret",
    "strava-verify-token",
    "webhook-path-secret",
    "llm-api-key",
  ]
}

resource "google_secret_manager_secret" "this" {
  for_each = toset(local.secrets)

  project   = var.project_id
  secret_id = each.value

  replication {
    user_managed {
      replicas {
        location = var.region
      }
    }
  }

  depends_on = [google_project_service.this]
}

# The runtime account may read these secrets and no others. Granted per secret
# rather than as a project-wide accessor, because that distinction is one IAM
# can actually express here — unlike Firestore, where the database is the
# finest grain available.
resource "google_secret_manager_secret_iam_member" "runtime_accessor" {
  for_each = google_secret_manager_secret.this

  project   = var.project_id
  secret_id = each.value.secret_id
  role      = "roles/secretmanager.secretAccessor"
  member    = "serviceAccount:${google_service_account.runtime.email}"
}
