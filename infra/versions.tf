terraform {
  required_version = ">= 1.13"

  required_providers {
    google = {
      source  = "hashicorp/google"
      version = "~> 7.45"
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
}
