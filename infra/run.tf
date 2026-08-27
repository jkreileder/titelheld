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
  # Whether the selected naming provider authenticates with llm-api-key.
  llm_keyed = contains(["anthropic", "openrouter"], var.llm_provider)

  # The image Cloud Run starts with. CI replaces it on the first deploy and
  # owns it from then on, which is what the ignore_changes below is for.
  placeholder_image = "us-docker.pkg.dev/cloudrun/container/hello"

  # One instance, said once. The service keeps state that is correct only while
  # one instance serves — the in-process OAuth state map, the first-bind lock,
  # token-refresh serialization and the sweep lock — and the binary refuses to
  # start unless it is told the ceiling it is running under. Both sides read
  # this local, so the platform limit and what the container believes cannot
  # drift apart.
  #
  # The ceiling is per revision, so it does not make the process a singleton:
  # a rolling deploy or a traffic split has two revisions serving at once, and
  # each of their instances reads the same 1. See config.RequiredMaxInstances
  # for what that leaves exposed and why it is accepted here.
  max_instances = 1

  sweep_path = "/sweep/${random_password.sweep_path.result}"

  # The audience Cloud Scheduler mints its OIDC token for, and the audience the
  # sweep handler requires. One value feeds both sides, because a mismatch is
  # not a visible failure: the scheduler keeps firing, the handler keeps
  # answering 401, and the queue quietly stops draining.
  #
  # It cannot be read from google_cloud_run_v2_service.this.uri here. The env
  # block below is inside that resource, so referencing its own URL is a cycle.
  # var.base_url is the same string by the time either side runs - it is set
  # from the service_url output on the second apply - and it is the value that
  # stays correct if a custom domain is ever put in front.
  sweep_audience = var.base_url
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
      max_instance_count = local.max_instances
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

      # Writes are live. A release onto this service needs the dated
      # WRITES_ACKNOWLEDGED repository variable, or the gate refuses to deploy;
      # the rollback is this value back to "1" and the scheduler's paused flag
      # back to true, applied.
      env {
        name  = "DRY_RUN"
        value = "0"
      }

      # The scaling ceiling above, told to the container that depends on it.
      # The binary refuses to start without it, so a revision deployed against
      # infrastructure that predates this contract fails readiness and the
      # previous revision keeps serving. It reports the configured ceiling, not
      # a guarantee that no other instance is running.
      env {
        name  = "MAX_INSTANCES"
        value = tostring(local.max_instances)
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

      # What the handler checks the Scheduler's OIDC token against. Empty on
      # the first apply, when base_url is not known yet; the handler treats
      # that as "do not mount the sweep route at all" rather than as "accept
      # any audience", so the window before the second apply is closed rather
      # than open.
      env {
        name  = "SWEEP_AUDIENCE"
        value = local.sweep_audience
      }

      env {
        name  = "SWEEP_SERVICE_ACCOUNT"
        value = google_service_account.scheduler.email
      }

      env {
        name  = "NOMINATIM_USER_AGENT"
        value = "titelheld/1.0 (+${var.base_url})"
      }

      # The naming provider, only when one is chosen: an unset variable is
      # not passed as an empty string, so the binary sees exactly what the
      # loader's dormancy test sees — nothing — and resolves Vertex.
      dynamic "env" {
        for_each = {
          for name, value in {
            LLM_PROVIDER = var.llm_provider
            LLM_MODEL    = var.llm_model
            LLM_BASE_URL = var.llm_base_url
          } : name => value if value != ""
        }

        content {
          name  = env.key
          value = env.value
        }
      }

      # The key is mounted only for a provider that reads it. A mounted secret
      # must have a version or the revision cannot start, and the keyless
      # default has no key to give it — so a Vertex deployment mounts nothing
      # and llm-api-key may sit without a version until an A/B is wanted.
      dynamic "env" {
        for_each = merge(
          {
            STRAVA_CLIENT_ID     = "strava-client-id"
            STRAVA_CLIENT_SECRET = "strava-client-secret"
            STRAVA_VERIFY_TOKEN  = "strava-verify-token"
            WEBHOOK_PATH_SECRET  = "webhook-path-secret"
          },
          local.llm_keyed ? { LLM_API_KEY = "llm-api-key" } : {},
        )

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
    google_secret_manager_secret_iam_binding.runtime_accessor,
  ]
}
