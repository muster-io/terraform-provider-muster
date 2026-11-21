// SPDX-License-Identifier: MIT
// Copyright The Muster Authors

// Package provider is the Terraform provider of Muster: its configuration, the version check and the machinery that
// every resource shares (resourcekit.go).
package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/provider/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/muster-io/terraform-provider-muster/internal/client"
)

const (
	// EndpointEnv and TokenEnv are the variables that endpoint and token fall back to.
	EndpointEnv = "MUSTER_ENDPOINT"
	TokenEnv    = "MUSTER_TOKEN"
)

var _ provider.Provider = (*musterProvider)(nil)

type musterProvider struct {
	version string
}

type providerModel struct {
	Endpoint types.String `tfsdk:"endpoint"`
	Token    types.String `tfsdk:"token"`
}

// New returns the provider factory of this version.
func New(version string) func() provider.Provider {
	return func() provider.Provider {
		return &musterProvider{version: version}
	}
}

func (p *musterProvider) Metadata(_ context.Context, _ provider.MetadataRequest, resp *provider.MetadataResponse) {
	resp.TypeName = "muster"
	resp.Version = p.version
}

func (p *musterProvider) Schema(_ context.Context, _ provider.SchemaRequest, resp *provider.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages the configuration of [Muster](https://github.com/muster-io/muster) through its " +
			"API, with the token of a Service account with the Admin Role.",
		Attributes: map[string]schema.Attribute{
			"endpoint": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "Muster's public URL, such as `https://muster.example.org`, without `/api/v1`. " +
					"Falls back to the environment variable `MUSTER_ENDPOINT` when unset or empty.",
			},
			"token": schema.StringAttribute{
				Optional:  true,
				Sensitive: true,
				MarkdownDescription: "The token of a Service account with the Admin Role (`mstr_sat_…`). Falls back to " +
					"the environment variable `MUSTER_TOKEN` when unset or empty.",
			},
		},
	}
}

// setting returns the value of an attribute, or of its variable when the configuration leaves it out or sets it empty,
// and where it came from.
func setting(value types.String, attribute, env string) (string, string) {
	if !value.IsNull() && !value.IsUnknown() && value.ValueString() != "" {
		return value.ValueString(), attribute
	}
	return os.Getenv(env), env
}

func (p *musterProvider) Configure(ctx context.Context, req provider.ConfigureRequest, resp *provider.ConfigureResponse) {
	var cfg providerModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}
	for _, a := range []struct {
		name  string
		value types.String
	}{{"endpoint", cfg.Endpoint}, {"token", cfg.Token}} {
		if a.value.IsUnknown() {
			resp.Diagnostics.AddAttributeError(path.Root(a.name), "Unknown "+a.name,
				fmt.Sprintf("The provider's %s depends on a value known only after apply; Muster's configuration needs "+
					"it to plan. Set it from a value known before, or from the environment.", a.name))
		}
	}
	if resp.Diagnostics.HasError() {
		return
	}

	endpoint, endpointSource := setting(cfg.Endpoint, "endpoint", EndpointEnv)
	token, tokenSource := setting(cfg.Token, "token", TokenEnv)
	if endpoint == "" {
		resp.Diagnostics.AddAttributeError(path.Root("endpoint"), "Missing Muster endpoint",
			"Set endpoint in the provider block or the environment variable MUSTER_ENDPOINT to Muster's public URL.")
	}
	if token == "" {
		resp.Diagnostics.AddAttributeError(path.Root("token"), "Missing Muster token",
			"Set token in the provider block or the environment variable MUSTER_TOKEN to the token of a Service "+
				"account with the Admin Role.")
	}
	if resp.Diagnostics.HasError() {
		return
	}

	c, err := client.New(client.Config{Endpoint: endpoint, Token: token, Version: p.version})
	if err != nil {
		addSourceError(&resp.Diagnostics, endpointSource, "endpoint", "Invalid Muster endpoint",
			fmt.Sprintf("The endpoint from %s: %s.", describeSource(endpointSource), err))
		return
	}
	status, err := c.GetSystemStatusWithResponse(ctx)
	if err != nil {
		resp.Diagnostics.AddError("Cannot reach Muster", fmt.Sprintf("Reading the System status of %s failed: %s.",
			c.Endpoint, err))
		return
	}
	if err := client.CheckResponse(status.HTTPResponse, status.Body); err != nil {
		var apiErr *client.Error
		errors.As(err, &apiErr)
		switch client.StatusOf(err) {
		case http.StatusUnauthorized:
			addSourceError(&resp.Diagnostics, tokenSource, "token",
				fmt.Sprintf("Muster refused the token from %s", describeSource(tokenSource)),
				fmt.Sprintf("Muster at %s answered 401: %s. Use a current token of an enabled Service account.",
					c.Endpoint, problemText(apiErr)))
		case http.StatusForbidden:
			addSourceError(&resp.Diagnostics, tokenSource, "token", "The token needs the Admin Role",
				fmt.Sprintf("Muster at %s answered 403 to the System status with the token from %s: %s. The provider "+
					"manages Muster's configuration, which only a Service account with the Admin Role may change.",
					c.Endpoint, describeSource(tokenSource), problemText(apiErr)))
		default:
			resp.Diagnostics.AddError("Cannot read Muster's System status", err.Error())
		}
		return
	}
	if status.JSON200 == nil {
		resp.Diagnostics.AddError("Cannot read Muster's System status",
			fmt.Sprintf("Muster at %s answered %d without the System status.", c.Endpoint, status.StatusCode()))
		return
	}
	resp.Diagnostics.Append(checkVersion(c.Endpoint, status.JSON200.Version)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.ResourceData = c
	resp.DataSourceData = c
}

func describeSource(source string) string {
	if source == EndpointEnv || source == TokenEnv {
		return source
	}
	return "the provider's " + source + " attribute"
}

func addSourceError(diags *diag.Diagnostics, source, attribute, summary, detail string) {
	if source == attribute {
		diags.AddAttributeError(path.Root(attribute), summary, detail)
		return
	}
	diags.AddError(summary, detail)
}

func problemText(e *client.Error) string {
	if e == nil {
		return "no problem"
	}
	text := e.Problem.Title
	if e.Problem.Code != "" {
		text += " (" + e.Problem.Code + ")"
	}
	if e.Problem.Detail != "" {
		text += ", " + e.Problem.Detail
	}
	return text
}

func (p *musterProvider) Resources(context.Context) []func() resource.Resource {
	return nil
}

func (p *musterProvider) DataSources(context.Context) []func() datasource.DataSource {
	return nil
}
