# The service is designed to fit inside the always-free tier. This exists to
# catch the case where it does not — a runaway sweep, an accidental
# min-instances change — before it becomes a real bill.
#
# Budgets live on the billing account, not the project, so applying this needs
# billing-account permission that project-level roles do not grant. See the
# bootstrap notes in the README.
data "google_billing_account" "this" {
  billing_account = var.billing_account
}

resource "google_billing_budget" "this" {
  billing_account = data.google_billing_account.this.id
  display_name    = "titelheld"

  budget_filter {
    projects = ["projects/${data.google_project.this.number}"]
  }

  amount {
    specified_amount {
      currency_code = "EUR"
      units         = tostring(var.budget_amount_eur)
    }
  }

  # Warn on the way up, not only once the money is gone.
  threshold_rules {
    threshold_percent = 0.5
  }

  threshold_rules {
    threshold_percent = 0.9
  }

  threshold_rules {
    threshold_percent = 1.0
  }

  depends_on = [google_project_service.this]
}

data "google_project" "this" {
  project_id = var.project_id
}
