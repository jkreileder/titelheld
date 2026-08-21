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

# The one composite index this service needs.
#
# Every other access here is by document ID, which Firestore serves from the
# automatic single-field indexes. The title history is the exception: it asks
# for one athlete's titles ordered by when they were written, and an equality
# on one field with an ordering on another is exactly what a composite index
# is for.
#
# It has to exist before the sweep runs. A missing index is not a slow query,
# it is an error on every naming — which is why the apply comes before the
# scheduler is unpaused, and why the store reports the failure instead of
# quietly naming without history.
#
# Nothing in the emulator checks this. It serves any query without an index,
# so a mismatch between these fields and the query in RecentTitles passes
# every test and fails only here.
resource "google_firestore_index" "named_recent" {
  project    = var.project_id
  database   = google_firestore_database.this.name
  collection = "named"

  fields {
    field_path = "athlete_id"
    order      = "ASCENDING"
  }

  fields {
    field_path = "named_at"
    order      = "DESCENDING"
  }

  depends_on = [google_project_service.this]
}
