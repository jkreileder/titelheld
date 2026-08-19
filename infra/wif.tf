# Workload Identity Federation for GitHub Actions.
#
# No service-account key is exported, ever. CI presents its GitHub OIDC token
# and receives a short-lived credential.
resource "google_iam_workload_identity_pool" "github" {
  project                   = var.project_id
  workload_identity_pool_id = "github"
  display_name              = "GitHub Actions"
  description               = "Federated identities for GitHub Actions workflows."

  depends_on = [google_project_service.this]
}

resource "google_iam_workload_identity_pool_provider" "github" {
  project                            = var.project_id
  workload_identity_pool_id          = google_iam_workload_identity_pool.github.workload_identity_pool_id
  workload_identity_pool_provider_id = "github"
  display_name                       = "GitHub Actions OIDC"

  # attribute.repository has to be mapped before it can be asserted on, either
  # in the condition below or in the principalSet member on the deploy account.
  attribute_mapping = {
    "google.subject"             = "assertion.sub"
    "attribute.actor"            = "assertion.actor"
    "attribute.repository"       = "assertion.repository"
    "attribute.repository_owner" = "assertion.repository_owner"
    "attribute.environment"      = "assertion.environment"
  }

  # One repository is not narrow enough on its own. Several workflows in this
  # repository already request id-token: write - Scorecard, for one - and any
  # of them could otherwise exchange a token for the deploy identity. The
  # environment claim is only present when a job declares that environment, so
  # requiring it means the deploy job, and nothing else, can federate.
  #
  # It also means the environment protection rules apply: required reviewers on
  # "production" become required reviewers on deploying.
  # The ref clause is the third narrowing: a release only ever runs from a
  # tag, so a token minted on a branch cannot federate even if it names the
  # environment.
  attribute_condition = "assertion.repository == \"${var.github_repository}\" && assertion.environment == \"${var.deploy_environment}\" && assertion.ref.startsWith(\"refs/tags/v\")"

  oidc {
    issuer_uri = "https://token.actions.githubusercontent.com"
  }
}

# The identity CI assumes. Distinct from the runtime identity: CI may deploy a
# revision and push an image, but has no access to Firestore or the secrets.
resource "google_service_account" "deploy" {
  project      = var.project_id
  account_id   = "titelheld-deploy"
  display_name = "Titelheld deploy"
  description  = "Assumed by GitHub Actions through Workload Identity Federation. Deploys revisions; reads no data."

  depends_on = [google_project_service.this]
}

# Only this repository may assume the deploy identity.
#
# This member is repository-scoped and nothing more: a principalSet can key on
# one attribute, so it cannot also require the environment or the ref. The
# narrowing to the deploy job specifically lives entirely in the provider's
# attribute_condition above - this binding is not a second, independent gate,
# and removing that condition would leave every workflow in the repository able
# to assume this identity.
#
# The environment's own protection rules are the remaining layer, and they are
# set on GitHub rather than here; see README.md, "Wire CI".
resource "google_service_account_iam_member" "deploy_workload_identity" {
  service_account_id = google_service_account.deploy.name
  role               = "roles/iam.workloadIdentityUser"
  member             = "principalSet://iam.googleapis.com/${google_iam_workload_identity_pool.github.name}/attribute.repository/${var.github_repository}"
}

resource "google_project_iam_member" "deploy_run" {
  project = var.project_id
  role    = "roles/run.developer"
  member  = "serviceAccount:${google_service_account.deploy.email}"
}

# Deploying a service that runs as the runtime account requires acting as it.
# Granted on that one account rather than project-wide.
resource "google_service_account_iam_member" "deploy_acts_as_runtime" {
  service_account_id = google_service_account.runtime.name
  role               = "roles/iam.serviceAccountUser"
  member             = "serviceAccount:${google_service_account.deploy.email}"
}

resource "google_artifact_registry_repository_iam_member" "deploy_writer" {
  project    = var.project_id
  location   = google_artifact_registry_repository.containers.location
  repository = google_artifact_registry_repository.containers.name
  role       = "roles/artifactregistry.writer"
  member     = "serviceAccount:${google_service_account.deploy.email}"
}
