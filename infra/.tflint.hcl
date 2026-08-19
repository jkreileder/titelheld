# tflint configuration.
#
# The bundled terraform ruleset covers style and structure; the google ruleset
# is what catches provider-specific mistakes — an invalid region, a role that
# does not exist — which is most of what can go wrong in this configuration.
tflint {
  required_version = ">= 0.64"
}

plugin "terraform" {
  enabled = true
  preset  = "recommended"
}

plugin "google" {
  enabled = true
  version = "0.39.0"
  source  = "github.com/terraform-linters/tflint-ruleset-google"
}
