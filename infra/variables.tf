variable "project_id" {
  description = "GCP project that holds every resource here. Project IDs are globally unique, so this is deliberately not defaulted to \"titelheld\"."
  type        = string

  validation {
    condition     = can(regex("^[a-z][a-z0-9-]{4,28}[a-z0-9]$", var.project_id))
    error_message = "project_id must be a valid GCP project ID: 6-30 characters, lowercase letters, digits and hyphens, starting with a letter."
  }
}

variable "billing_account" {
  description = "Billing account ID (the XXXXXX-XXXXXX-XXXXXX form) the budget alert is attached to. The budget lives on the billing account, not the project."
  type        = string
}

variable "region" {
  description = "Region for Cloud Run, Artifact Registry and the scheduler."
  type        = string
  default     = "europe-west3"
}

variable "firestore_location" {
  description = "Firestore location. Cannot be changed after the database is created."
  type        = string
  default     = "europe-west3"
}

variable "firestore_database" {
  description = "Firestore database ID. A named database rather than (default), so the runtime service account can be scoped to it alone."
  type        = string
  default     = "titelheld"
}

variable "service_name" {
  description = "Cloud Run service name."
  type        = string
  default     = "titelheld"
}

variable "github_repository" {
  description = "The only repository allowed to assume the deploy identity, as owner/name."
  type        = string
  default     = "jkreileder/titelheld"
}

variable "deploy_environment" {
  description = "GitHub Actions environment the deploy job must run in. Federation requires it, so a workflow that does not name this environment cannot assume the deploy identity even from this repository."
  type        = string
  default     = "production"
}

variable "base_url" {
  description = "Public base URL of the deployed service, used to build the OAuth redirect. Empty on the first apply: Cloud Run mints the URL, so it is read from the service_url output and set on a second apply. See the apply order in the README."
  type        = string
  default     = ""
}

variable "process_delay" {
  description = "How long to wait after a Strava event before naming, so the other automations in the chain have finished writing."
  type        = string
  default     = "10m"
}

variable "budget_amount_eur" {
  description = "Budget alert threshold in euros. The service is designed to fit inside the always-free tier; this exists to catch the case where it does not."
  type        = number
  default     = 1
}

variable "sweep_schedule" {
  description = "Cron schedule for the sweep that drains the delay queue. Every five minutes is far finer than the ten-minute processing delay needs."
  type        = string
  default     = "*/5 * * * *"
}

# The naming provider. Unset means the keyless Vertex default, and none of
# these reach the container then. Switching narrators for an A/B is a change
# to these three and an apply — plus a new version of the llm-api-key secret
# holding the selected provider's key, which Terraform never touches.
variable "llm_provider" {
  description = "LLM_PROVIDER: empty (Vertex, keyless), \"anthropic\" or \"openrouter\"."
  type        = string
  default     = ""

  validation {
    condition     = contains(["", "gemini", "anthropic", "openrouter"], var.llm_provider)
    error_message = "llm_provider must be empty, \"gemini\", \"anthropic\" or \"openrouter\"."
  }
}

variable "llm_model" {
  description = "LLM_MODEL: overrides the provider's shipped, pinned model ID. Empty means the provider default."
  type        = string
  default     = ""
}

variable "llm_base_url" {
  description = "LLM_BASE_URL: the https API root the openrouter provider calls, including its version path. Empty means OpenRouter's own."
  type        = string
  default     = ""
}

# Where the Vertex call goes. A region keeps the request there and serves what
# that region has; the two multi-regions reach models no European region does,
# "eu" from inside Europe and "global" from wherever there is capacity. The
# prompt carries place names derived from the athlete's GPS traces, which is
# what makes the difference between those two worth a variable.
variable "vertex_location" {
  description = "VERTEX_LOCATION: the Vertex location the naming call addresses. Empty means the binary's own default, europe-west3."
  type        = string
  default     = ""

  validation {
    # The shape the binary enforces, enforced again here: the value is
    # interpolated into the request host, so a malformed one is refused by an
    # apply rather than by a revision that will not start.
    condition     = var.vertex_location == "" || can(regex("^(?:eu|[a-z][a-z0-9-]{1,38}[a-z0-9])$", var.vertex_location))
    error_message = "vertex_location must be empty, a GCP region such as europe-west3, or a multi-region the service has a host for: eu or global."
  }
}

variable "geo_sample_count" {
  description = "GEO_SAMPLE_COUNT: interior track samples geocoded per activity. Empty means the binary's own default, 6."
  type        = string
  default     = ""

  validation {
    # The bounds the binary enforces, enforced again here: the interior
    # samples plus the farthest point must stay within Nominatim's
    # per-activity budget.
    condition     = var.geo_sample_count == "" || can(regex("^[1-6]$", var.geo_sample_count))
    error_message = "geo_sample_count must be empty or 1 to 6."
  }
}

variable "nominatim_zoom" {
  description = "NOMINATIM_ZOOM: the granularity Nominatim answers at. Empty means the binary's own default, 16."
  type        = string
  default     = ""

  validation {
    condition     = var.nominatim_zoom == "" || can(regex("^([3-9]|1[0-8])$", var.nominatim_zoom))
    error_message = "nominatim_zoom must be empty or 3 to 18."
  }
}

variable "nominatim_place_fields" {
  description = "NOMINATIM_PLACE_FIELDS: comma-separated address fields tried in order per point. Empty means the binary's own default, hamlet,village,suburb,town. The binary validates the keys; this only refuses shapes that cannot be a field list."
  type        = string
  default     = ""

  validation {
    condition     = var.nominatim_place_fields == "" || can(regex("^[a-z_]+(,[a-z_]+)*$", var.nominatim_place_fields))
    error_message = "nominatim_place_fields must be empty or a comma-separated list of lowercase field names."
  }
}
