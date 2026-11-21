# terraform-provider-muster

The Terraform and OpenTofu provider of [Muster](https://github.com/muster-io/muster), the self-hosted alert grouping
and on-call service that sits after Alertmanager: it keeps Muster's configuration as code, through Muster's API.

Work in progress: this release has the provider's configuration and no resources yet. The resources and data sources
come with the next releases; the documentation in [`docs/`](docs/index.md) is what the Registries will show.

## Installation

Once released, the provider is published in the Terraform Registry and the OpenTofu Registry as `muster-io/muster`:

```terraform
terraform {
  required_providers {
    muster = {
      source = "muster-io/muster"
    }
  }
}

provider "muster" {}
```

`terraform init` or `tofu init` installs it.

## Configuration

| Attribute | Environment variable | Meaning |
|---|---|---|
| `endpoint` | `MUSTER_ENDPOINT` | Muster's public URL, such as `https://muster.example.org`, without `/api/v1` |
| `token` | `MUSTER_TOKEN` | the token of a Service account with the Admin Role; sensitive |

A value in the provider block wins over the environment. In Muster, an Admin creates a Service account with the Admin
Role, such as `terraform`, and a token for it; the pipeline that runs Terraform sets the token in `MUSTER_TOKEN`.

When it is configured, the provider reads Muster's System status, which only the Admin Role may read: a refused token
stops with an error that names `MUSTER_TOKEN` or `token`, and a token without the Admin Role with "The token needs the
Admin Role".

## Versions

- **Muster** 0.2.0 or newer. An older release stops the provider with "Muster 0.1.4 is older than 0.2.0, the oldest
  version this provider works with". Only a plain release version `X.Y.Z` (an optional leading `v` aside) is compared;
  any other version — a development build such as `0.0.0-dev`, a pre-release, or a build with a suffix such as
  `0.2.1-5-gabc1234` — passes with a warning that it cannot be compared.
- **Terraform** 1.11 or newer, or **OpenTofu** 1.11 or newer, the first with write-only attributes, which the Secrets'
  `*_wo` attributes need. The sensitive attributes of Secrets also work on older CLIs, which are tested only for
  refusing the write-only ones.

## Development

[`AGENTS.md`](AGENTS.md) and [`CONTRIBUTING.md`](CONTRIBUTING.md) describe the repository, the Makefile and how a
change is made. In short: `make ci` runs the checks of a pull request, and on Linux with Docker

```sh
eval "$(test/muster-dev.sh up)"   # muster dev from Muster's image, with PostgreSQL and two Service accounts
make testacc
test/muster-dev.sh down
```

runs the acceptance tests.

## Licence

MIT, without a Contributor License Agreement: see [`LICENSE`](LICENSE). Muster itself is under the AGPL; this provider
contains none of its code, only a copy of its API specification, from which the client is generated.
