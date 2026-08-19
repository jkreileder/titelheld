# The unguessable segment of the sweep path, generated rather than configured.
#
# It never appears in code or in tfvars. It does live in state, which is why
# the bootstrap instructions insist the state bucket is private: Terraform
# cannot manage a value it cannot see.
resource "random_password" "sweep_path" {
  length  = 32
  special = false
}

locals {
  # The image Cloud Run starts with. CI replaces it on the first deploy and
  # owns it from then on, which is what the ignore_changes below is for.
  placeholder_image = "us-docker.pkg.dev/cloudrun/container/hello"

  sweep_path = "/sweep/${random_password.sweep_path.result}"
}

resource "google_cloud_run_v2_service" "this" {
  project  = var.project_id
  name     = var.service_name
  location = var.region

  # Strava cannot present a Google credential, so the webhook has to be
  # reachable without one. ingress only controls the network path - it is the
  # allUsers invoker binding in iam.tf that actually admits unauthenticated
  # requests, and Cloud Run IAM is service-wide, so it admits them to every
  # route including the sweep.
  ingress = "INGRESS_TRAFFIC_ALL"

  deletion_protection = false

  template {
    service_account = google_service_account.runtime.email

    # Scale to zero when idle, and never past one instance. More than one would
    # break the in-process OAuth state map and let two instances race for the
    # rotating refresh token.
    scaling {
      min_instance_count = 0
      max_instance_count = 1
    }

    containers {
      image = local.placeholder_image

      resources {
        limits = {
          cpu    = "1"
          memory = "512Mi"
        }
        cpu_idle = true
      }

      # Dry run stays on in the deployed service. Flipping it is a deliberate,
      # separate act.
      env {
        name  = "DRY_RUN"
        value = "1"
      }

      env {
        name  = "BASE_URL"
        value = var.base_url
      }

      env {
        name  = "FIRESTORE_PROJECT"
        value = var.project_id
      }

      env {
        name  = "FIRESTORE_DATABASE"
        value = google_firestore_database.this.name
      }

      env {
        name  = "PROCESS_DELAY"
        value = var.process_delay
      }

      env {
        name  = "SWEEP_PATH"
        value = local.sweep_path
      }

      env {
        name  = "NOMINATIM_USER_AGENT"
        value = "titelheld/1.0 (+${var.base_url})"
      }

      dynamic "env" {
        for_each = {
          STRAVA_CLIENT_ID     = "strava-client-id"
          STRAVA_CLIENT_SECRET = "strava-client-secret"
          STRAVA_VERIFY_TOKEN  = "strava-verify-token"
          WEBHOOK_PATH_SECRET  = "webhook-path-secret"
          LLM_API_KEY          = "llm-api-key"
        }

        content {
          name = env.key

          value_source {
            secret_key_ref {
              secret  = google_secret_manager_secret.this[env.value].secret_id
              version = "latest"
            }
          }
        }
      }
    }
  }

  lifecycle {
    # CI owns the image from the first deploy onwards. Without this, every plan
    # would propose reverting to the placeholder.
    ignore_changes = [
      template[0].containers[0].image,
      client,
      client_version,
    ]
  }

  depends_on = [
    google_project_service.this,
    google_secret_manager_secret_iam_member.runtime_accessor,
  ]
}
