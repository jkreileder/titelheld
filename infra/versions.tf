terraform {
  required_version = ">= 1.13"

  required_providers {
    google = {
      source  = "hashicorp/google"
      version = "~> 8.0"
    }
    random = {
      source  = "hashicorp/random"
      version = "~> 3.7"
    }
  }

  # State lives in a bucket created by hand before the first init; see the
  # bootstrap section of the README. It is configured with -backend-config so
  # the bucket name is not baked into the repository.
  backend "gcs" {
    prefix = "titelheld"
  }
}

provider "google" {
  project = var.project_id
  region  = var.region

  # Some APIs bill quota to a project of the caller's choosing rather than to
  # the project holding the resource, and refuse the call when the caller does
  # not name one. billingbudgets is one: the budget lives on the billing
  # account, so there is no resource project for it to infer.
  #
  # Applying with user credentials therefore fails on the budget with
  # SERVICE_DISABLED against Google's own default project, which is confusing
  # because the API *is* enabled on this one. These two send this project as
  # the quota project instead.
  user_project_override = true
  billing_project       = var.project_id
}
