terraform {
  # 1.10+ for S3 native state locking (use_lockfile).
  required_version = ">= 1.10"

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 6.73"
    }
    cloudflare = {
      source  = "cloudflare/cloudflare"
      version = "~> 5.29"
    }
  }

  # Remote state, recommended (see README.md "State"). Partial config: pass
  # the bucket with `terraform init -backend-config=backend.hcl`.
  # For a local trial, run `terraform init -backend=false`.
  backend "s3" {}
}

# Credentials come from the environment, never from variables, so they are
# not written to state or plan files:
#   AWS:        AWS_PROFILE (or AWS_ACCESS_KEY_ID/AWS_SECRET_ACCESS_KEY)
#   Cloudflare: CLOUDFLARE_API_TOKEN
provider "aws" {
  region = var.region
  default_tags {
    tags = {
      Project     = "infosecstreams"
      Environment = "Production"
      ManagedBy   = "terraform/edge"
    }
  }
}

provider "cloudflare" {}
