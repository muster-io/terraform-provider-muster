// SPDX-License-Identifier: MIT
// Copyright The Muster Authors

// Package acctest is what the acceptance tests share: the provider factories, the pre-check, the provider block, and a
// client of Muster that changes objects behind Terraform's back for the drift and 412 tests. The tests run against
// muster dev, which test/muster-dev.sh starts and whose variables it prints.
package acctest

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/path"
	fwprovider "github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"

	"github.com/muster-io/terraform-provider-muster/internal/client"
	"github.com/muster-io/terraform-provider-muster/internal/provider"
)

// ResponderTokenEnv holds the token of a Service account with the Responder Role, which test/muster-dev.sh creates.
const ResponderTokenEnv = "MUSTER_RESPONDER_TOKEN"

// ProviderConfig is the provider block of the acceptance tests, configured from MUSTER_ENDPOINT and MUSTER_TOKEN.
const ProviderConfig = `
terraform {
  required_providers {
    muster = {
      source = "muster-io/muster"
    }
  }
}

provider "muster" {}
`

// ProbeConfig is ProviderConfig with the data source muster_acceptance_probe, which only the tests serve: Terraform
// and OpenTofu configure a provider only when something in the configuration uses it, and the provider block alone
// does not.
const ProbeConfig = ProviderConfig + `
data "muster_acceptance_probe" "this" {}
`

// ProtoV6ProviderFactories serve the provider in the test's process, with the data source muster_acceptance_probe.
var ProtoV6ProviderFactories = map[string]func() (tfprotov6.ProviderServer, error){
	"muster": providerserver.NewProtocol6WithError(probeProvider{Provider: provider.New("acc")()}),
}

// probeProvider is the provider with the data source muster_acceptance_probe added.
type probeProvider struct {
	fwprovider.Provider
}

func (p probeProvider) DataSources(ctx context.Context) []func() datasource.DataSource {
	return append(p.Provider.DataSources(ctx), func() datasource.DataSource { return &probe{} })
}

// probe reads the endpoint of the client that the provider's Configure made.
type probe struct {
	client *client.Client
}

func (d *probe) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_acceptance_probe"
}

func (d *probe) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{Attributes: map[string]schema.Attribute{
		"endpoint": schema.StringAttribute{Computed: true},
	}}
}

func (d *probe) Configure(_ context.Context, req datasource.ConfigureRequest, _ *datasource.ConfigureResponse) {
	if c, ok := req.ProviderData.(*client.Client); ok {
		d.client = c
	}
}

func (d *probe) Read(ctx context.Context, _ datasource.ReadRequest, resp *datasource.ReadResponse) {
	if d.client == nil {
		resp.Diagnostics.AddError("Provider not configured", "muster_acceptance_probe has no client.")
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("endpoint"), types.StringValue(d.client.Endpoint))...)
}

var addressOnce sync.Once

// PreCheck fails the test when the variables of test/muster-dev.sh are missing, and gives terraform-plugin-testing the
// provider's address: the namespace muster-io, on the OpenTofu Registry's host when TF_ACC_TERRAFORM_PATH is tofu.
func PreCheck(t *testing.T) {
	t.Helper()
	for _, env := range []string{provider.EndpointEnv, provider.TokenEnv, ResponderTokenEnv} {
		if os.Getenv(env) == "" {
			t.Fatalf(`%s is not set: run eval "$(test/muster-dev.sh up)" first, or make testacc`, env)
		}
	}
	addressOnce.Do(func() {
		setDefault("TF_ACC_PROVIDER_NAMESPACE", "muster-io")
		if IsOpenTofu() {
			setDefault("TF_ACC_PROVIDER_HOST", "registry.opentofu.org")
		}
	})
}

func setDefault(env, value string) {
	if os.Getenv(env) == "" {
		_ = os.Setenv(env, value)
	}
}

// IsOpenTofu reports whether the tests run OpenTofu: TF_ACC_TERRAFORM_PATH names tofu.
func IsOpenTofu() bool {
	return strings.HasPrefix(filepath.Base(os.Getenv("TF_ACC_TERRAFORM_PATH")), "tofu")
}

// ResponderToken is the token of the Service account with the Responder Role.
func ResponderToken() string {
	return os.Getenv(ResponderTokenEnv)
}

// Client is a client of Muster with the Admin token, for changes behind Terraform's back.
func Client(t *testing.T) *client.Client {
	t.Helper()
	c, err := client.New(client.Config{
		Endpoint: os.Getenv(provider.EndpointEnv),
		Token:    os.Getenv(provider.TokenEnv),
		Version:  "acc",
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}
