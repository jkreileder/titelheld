# The database that holds the OAuth token pair.
#
# A named database rather than (default): it is what lets the runtime service
# account be scoped to this data and nothing else, which is the finest grain IAM
# offers for a server client library. See docs/firestore-iam.md.
#
# The location cannot be changed after creation.
resource "google_firestore_database" "this" {
  project     = var.project_id
  name        = var.firestore_database
  location_id = var.firestore_location
  type        = "FIRESTORE_NATIVE"

  # The token pair cannot be re-derived from anywhere, so make it awkward to
  # delete by accident.
  delete_protection_state = "DELETE_PROTECTION_ENABLED"
  deletion_policy         = "ABANDON"

  depends_on = [google_project_service.this]
}
