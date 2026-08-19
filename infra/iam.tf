# The identity the service runs as. The default compute service account is
# deliberately unused: it is over-privileged and shared with anything else in
# the project.
resource "google_service_account" "runtime" {
  project      = var.project_id
  account_id   = "titelheld-runtime"
  display_name = "Titelheld runtime"
  description  = "Cloud Run runtime identity. Firestore data access and the five secrets, nothing else."

  depends_on = [google_project_service.this]
}

# Firestore access, scoped to the one database.
#
# roles/datastore.user is read/write on data — datastore.entities.* plus
# metadata reads — and excludes datastore.owner, import/export, and index
# administration. Per-collection restriction is not expressible for a server
# service account, so the database is the finest grain there is; the condition
# below is what keeps this binding from covering every database in the project.
#
# If the condition is ever rejected or fails to match, the effect is that the
# service loses access rather than gains it. Verify it with:
#   gcloud projects get-iam-policy "$PROJECT" \
#     --flatten="bindings[].members" \
#     --filter="bindings.members:$SA AND bindings.role:roles/datastore.user" \
#     --format="yaml(bindings.condition)"
resource "google_project_iam_member" "runtime_firestore" {
  project = var.project_id
  role    = "roles/datastore.user"
  member  = "serviceAccount:${google_service_account.runtime.email}"

  condition {
    title       = "titelheld-database-only"
    description = "Only the titelheld Firestore database, not every database in the project."
    expression  = "resource.name == \"projects/${var.project_id}/databases/${var.firestore_database}\""
  }
}

# The service must accept unauthenticated requests, because Strava's webhook has
# no way to present a Google credential. There is no narrower option: Cloud Run
# invoker permission is service-wide, so this admits anonymous callers to every
# route, the sweep included.
#
# Where the real defences are, given that:
#
#   * The webhook is guarded by an unguessable path segment and by the verify
#     token, compared in constant time.
#   * The sweep is guarded by an unguessable path segment and by the OIDC token
#     Cloud Scheduler attaches - but Cloud Run will NOT check that token while
#     this binding exists. The application has to verify it. That is a hard
#     requirement for the phase that adds the sweep handler, not a
#     nice-to-have, and it is why the scheduler keeps its own identity below:
#     the token has to come from something verifiable.
#
# The alternative is two services, one public and one private. That is the right
# answer if the sweep ever does anything expensive; today it doubles the
# deployment to protect an endpoint whose only power is draining the queue
# slightly early.
resource "google_cloud_run_v2_service_iam_member" "public_invoker" {
  project  = var.project_id
  location = google_cloud_run_v2_service.this.location
  name     = google_cloud_run_v2_service.this.name
  role     = "roles/run.invoker"
  member   = "allUsers"
}

# The identity Cloud Scheduler uses to call the sweep endpoint. Separate from
# the runtime identity so the thing that triggers work cannot read the data,
# and it is this account's token the sweep handler will verify.
resource "google_service_account" "scheduler" {
  project      = var.project_id
  account_id   = "titelheld-scheduler"
  display_name = "Titelheld scheduler"
  description  = "Invokes the sweep endpoint. Invoke permission only."

  depends_on = [google_project_service.this]
}

# Redundant while allUsers holds the same role, and kept deliberately: if the
# public binding ever goes away - because the webhook moved to its own service,
# say - the sweep still has to work.
resource "google_cloud_run_v2_service_iam_member" "scheduler_invoker" {
  project  = var.project_id
  location = google_cloud_run_v2_service.this.location
  name     = google_cloud_run_v2_service.this.name
  role     = "roles/run.invoker"
  member   = "serviceAccount:${google_service_account.scheduler.email}"
}
