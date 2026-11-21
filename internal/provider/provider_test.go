// SPDX-License-Identifier: MIT
// Copyright The Muster Authors

package provider_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	fwprovider "github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"

	"github.com/muster-io/terraform-provider-muster/internal/acctest"
	"github.com/muster-io/terraform-provider-muster/internal/client"
	"github.com/muster-io/terraform-provider-muster/internal/provider"
)

// configured checks that the provider was configured: the probe read the endpoint of its client.
func configured() resource.TestCheckFunc {
	return resource.TestCheckResourceAttr("data.muster_acceptance_probe.this", "endpoint",
		strings.TrimRight(os.Getenv(provider.EndpointEnv), "/"))
}

func TestAccProvider_configure(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { acctest.PreCheck(t) },
		ProtoV6ProviderFactories: acctest.ProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			// A configuration with only the provider block plans (C-33.AC-1); the CLI does not configure a provider
			// that nothing uses, so the probe step proves the configuration.
			{Config: acctest.ProviderConfig, PlanOnly: true},
			{Config: acctest.ProbeConfig, Check: configured()},
		},
	})

	for _, tc := range []struct {
		token string
		want  string
	}{
		{acctest.ResponderToken(), `The token needs the Admin Role`},
		{"mstr_sat_wrong", `Muster refused the token from MUSTER_TOKEN`},
	} {
		t.Setenv(provider.TokenEnv, tc.token)
		resource.Test(t, resource.TestCase{
			PreCheck:                 func() { acctest.PreCheck(t) },
			ProtoV6ProviderFactories: acctest.ProtoV6ProviderFactories,
			Steps: []resource.TestStep{{
				Config:      acctest.ProbeConfig,
				PlanOnly:    true,
				ExpectError: regexp.MustCompile(tc.want),
			}},
		})
	}
}

// TestAccProvider_configure_oldCLI runs only in the job old-cli (make testacc ACC_RUN='_oldCLI$$' ACC_SKIP=):
// Terraform 1.10 and OpenTofu 1.10 configure the provider and read through it.
func TestAccProvider_configure_oldCLI(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { acctest.PreCheck(t) },
		ProtoV6ProviderFactories: acctest.ProtoV6ProviderFactories,
		Steps:                    []resource.TestStep{{Config: acctest.ProbeConfig, Check: configured()}},
	})
}

// muster is a test server of getSystemStatus that answers status and body, and counts the requests with each token.
type muster struct {
	srv      *httptest.Server
	requests atomic.Int32
	token    atomic.Value
}

func newMuster(t *testing.T, status int, contentType, body string) *muster {
	t.Helper()
	m := &muster{}
	m.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.requests.Add(1)
		m.token.Store(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		if r.URL.Path != "/api/v1/system-status" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", contentType)
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(m.srv.Close)
	return m
}

func versionMuster(t *testing.T, version string) *muster {
	return newMuster(t, http.StatusOK, "application/json", `{"version":"`+version+`","replicas":[],`+
		`"recovery":{"active":false},"delivery_queues":[],"broken_destinations":[],"integrations_attention":[],`+
		`"template_errors":[],"active_storms":[],"outgoing_heartbeat":{}}`)
}

func problemMuster(t *testing.T, status int, title, code string) *muster {
	return newMuster(t, status, "application/problem+json", `{"type":"https://muster-io.github.io/muster/problems/x",`+
		`"title":"`+title+`","status":`+strconv.Itoa(status)+`,"code":"`+code+`"}`)
}

// configure runs the provider's Configure with endpoint and token in the provider block; nil leaves one out.
func configure(t *testing.T, endpoint, token *string) *fwprovider.ConfigureResponse {
	t.Helper()
	ctx := t.Context()
	p := provider.New("test")()
	schemaResp := &fwprovider.SchemaResponse{}
	p.Schema(ctx, fwprovider.SchemaRequest{}, schemaResp)
	typ := schemaResp.Schema.Type().TerraformType(ctx)
	value := func(s *string) tftypes.Value {
		if s == nil {
			return tftypes.NewValue(tftypes.String, nil)
		}
		return tftypes.NewValue(tftypes.String, *s)
	}
	raw := tftypes.NewValue(typ, map[string]tftypes.Value{"endpoint": value(endpoint), "token": value(token)})
	resp := &fwprovider.ConfigureResponse{}
	p.Configure(ctx, fwprovider.ConfigureRequest{Config: tfsdk.Config{Schema: schemaResp.Schema, Raw: raw}}, resp)
	return resp
}

func ptr(s string) *string { return &s }

func errorTexts(diags diag.Diagnostics) []string {
	var out []string
	for _, d := range diags.Errors() {
		out = append(out, d.Summary()+": "+d.Detail())
	}
	return out
}

func TestConfigure_neitherSetNamesBoth(t *testing.T) {
	t.Setenv(provider.EndpointEnv, "")
	t.Setenv(provider.TokenEnv, "")
	resp := configure(t, nil, nil)
	got := strings.Join(errorTexts(resp.Diagnostics), "\n")
	for _, want := range []string{"endpoint", "MUSTER_ENDPOINT", "token", "MUSTER_TOKEN"} {
		if !strings.Contains(got, want) {
			t.Errorf("errors without %s:\n%s", want, got)
		}
	}
	if len(resp.Diagnostics.Errors()) != 2 {
		t.Fatalf("errors %v", got)
	}
}

func TestConfigure_fallsBackToTheEnvironment(t *testing.T) {
	m := versionMuster(t, "0.2.0")
	t.Setenv(provider.EndpointEnv, m.srv.URL)
	t.Setenv(provider.TokenEnv, "env-token")
	resp := configure(t, nil, nil)
	if resp.Diagnostics.HasError() || m.token.Load() != "env-token" {
		t.Fatalf("%v, token %v", resp.Diagnostics, m.token.Load())
	}
	if _, ok := resp.ResourceData.(*client.Client); !ok {
		t.Fatalf("resource data %T", resp.ResourceData)
	}
	if _, ok := resp.DataSourceData.(*client.Client); !ok {
		t.Fatalf("data source data %T", resp.DataSourceData)
	}

	m.token.Store("")
	resp = configure(t, ptr(""), ptr(""))
	if resp.Diagnostics.HasError() || m.requests.Load() != 2 || m.token.Load() != "env-token" {
		t.Fatalf("empty attributes: %v, %d requests, token %v", resp.Diagnostics, m.requests.Load(), m.token.Load())
	}
}

func TestConfigure_theConfigurationWinsOverTheEnvironment(t *testing.T) {
	env := versionMuster(t, "0.2.0")
	configured := versionMuster(t, "0.2.0")
	t.Setenv(provider.EndpointEnv, env.srv.URL)
	t.Setenv(provider.TokenEnv, "env-token")
	resp := configure(t, ptr(configured.srv.URL), ptr("config-token"))
	if resp.Diagnostics.HasError() {
		t.Fatal(resp.Diagnostics)
	}
	if env.requests.Load() != 0 || configured.requests.Load() != 1 || configured.token.Load() != "config-token" {
		t.Fatalf("env %d, configured %d with %v", env.requests.Load(), configured.requests.Load(),
			configured.token.Load())
	}
}

func TestConfigure_checksTheVersion(t *testing.T) {
	t.Setenv(provider.TokenEnv, "token")
	for _, tc := range []struct {
		version string
		err     string
		warning bool
	}{
		{"0.1.4", "Muster 0.1.4 is older than 0.2.0, the oldest version this provider works with", false},
		{"0.2.0", "", false},
		{"0.9.3", "", false},
		{"0.2.1-5-gabc1234", "", true},
		{"dev", "", true},
	} {
		m := versionMuster(t, tc.version)
		resp := configure(t, ptr(m.srv.URL), nil)
		errs := resp.Diagnostics.Errors()
		switch {
		case tc.err == "" && len(errs) != 0:
			t.Errorf("%s: %v", tc.version, errs)
		case tc.err != "" && (len(errs) != 1 || errs[0].Summary() != tc.err):
			t.Errorf("%s: %v, want %q", tc.version, errs, tc.err)
		case tc.warning != (len(resp.Diagnostics.Warnings()) == 1):
			t.Errorf("%s: warnings %v", tc.version, resp.Diagnostics.Warnings())
		case tc.err != "" && resp.ResourceData != nil:
			t.Errorf("%s: configured despite the error", tc.version)
		}
	}
}

func TestConfigure_401NamesWhereTheTokenCameFrom(t *testing.T) {
	m := problemMuster(t, http.StatusUnauthorized, "Unauthorized", "invalid_token")
	t.Setenv(provider.EndpointEnv, m.srv.URL)
	t.Setenv(provider.TokenEnv, "wrong")

	resp := configure(t, nil, nil)
	got := errorTexts(resp.Diagnostics)
	if len(got) != 1 || !strings.HasPrefix(got[0], "Muster refused the token from MUSTER_TOKEN: ") ||
		strings.Count(got[0], "MUSTER_TOKEN") != 1 || !strings.Contains(got[0], "invalid_token") {
		t.Fatalf("errors %q", got)
	}

	resp = configure(t, nil, ptr("wrong"))
	got = errorTexts(resp.Diagnostics)
	if len(got) != 1 || !strings.HasPrefix(got[0], "Muster refused the token from the provider's token attribute") ||
		strings.Contains(got[0], "MUSTER_TOKEN") {
		t.Fatalf("errors %q", got)
	}
}

func TestConfigure_403SaysTheTokenNeedsTheAdminRole(t *testing.T) {
	m := problemMuster(t, http.StatusForbidden, "Forbidden", "permission_denied")
	resp := configure(t, ptr(m.srv.URL), ptr("responder"))
	got := errorTexts(resp.Diagnostics)
	if len(got) != 1 || strings.Count(got[0], "needs the Admin Role") != 1 ||
		!strings.Contains(got[0], "the provider's token attribute") {
		t.Fatalf("errors %q", got)
	}
}

func TestConfigure_otherAnswers(t *testing.T) {
	t.Setenv(provider.TokenEnv, "token")
	for _, tc := range []struct {
		m    *muster
		want string
	}{
		{problemMuster(t, http.StatusNotFound, "Not Found", ""), "Cannot read Muster's System status: GET /api/v1/system-status: 404"},
		{newMuster(t, http.StatusOK, "text/plain", "ok"), "Cannot read Muster's System status: Muster at"},
	} {
		resp := configure(t, ptr(tc.m.srv.URL), nil)
		got := errorTexts(resp.Diagnostics)
		if len(got) != 1 || !strings.HasPrefix(got[0], tc.want) {
			t.Errorf("errors %q, want %q", got, tc.want)
		}
	}

	resp := configure(t, ptr("ftp://muster.example.org"), nil)
	if got := errorTexts(resp.Diagnostics); len(got) != 1 || !strings.HasPrefix(got[0],
		"Invalid Muster endpoint: The endpoint from the provider's endpoint attribute") {
		t.Fatalf("errors %q", got)
	}
}

func TestConfigure_unknownValues(t *testing.T) {
	ctx := t.Context()
	p := provider.New("test")()
	schemaResp := &fwprovider.SchemaResponse{}
	p.Schema(ctx, fwprovider.SchemaRequest{}, schemaResp)
	raw := tftypes.NewValue(schemaResp.Schema.Type().TerraformType(ctx), map[string]tftypes.Value{
		"endpoint": tftypes.NewValue(tftypes.String, tftypes.UnknownValue),
		"token":    tftypes.NewValue(tftypes.String, "token"),
	})
	resp := &fwprovider.ConfigureResponse{}
	p.Configure(ctx, fwprovider.ConfigureRequest{Config: tfsdk.Config{Schema: schemaResp.Schema, Raw: raw}}, resp)
	if got := errorTexts(resp.Diagnostics); len(got) != 1 || !strings.HasPrefix(got[0], "Unknown endpoint") {
		t.Fatalf("errors %q", got)
	}

	both := tftypes.NewValue(schemaResp.Schema.Type().TerraformType(ctx), map[string]tftypes.Value{
		"endpoint": tftypes.NewValue(tftypes.String, tftypes.UnknownValue),
		"token":    tftypes.NewValue(tftypes.String, tftypes.UnknownValue),
	})
	for range 20 {
		resp := &fwprovider.ConfigureResponse{}
		p.Configure(ctx, fwprovider.ConfigureRequest{Config: tfsdk.Config{Schema: schemaResp.Schema, Raw: both}}, resp)
		got := errorTexts(resp.Diagnostics)
		if len(got) != 2 || !strings.HasPrefix(got[0], "Unknown endpoint") || !strings.HasPrefix(got[1], "Unknown token") {
			t.Fatalf("errors %q, want endpoint then token", got)
		}
	}
}

func TestProvider_metadataAndSchema(t *testing.T) {
	ctx := t.Context()
	p := provider.New("1.2.3")()
	meta := &fwprovider.MetadataResponse{}
	p.Metadata(ctx, fwprovider.MetadataRequest{}, meta)
	if meta.TypeName != "muster" || meta.Version != "1.2.3" {
		t.Fatalf("metadata %+v", meta)
	}
	schemaResp := &fwprovider.SchemaResponse{}
	p.Schema(ctx, fwprovider.SchemaRequest{}, schemaResp)
	token, ok := schemaResp.Schema.Attributes["token"]
	if !ok || !token.IsSensitive() || !token.IsOptional() || !schemaResp.Schema.Attributes["endpoint"].IsOptional() {
		t.Fatalf("schema %+v", schemaResp.Schema.Attributes)
	}
	if len(p.Resources(ctx)) != 0 || len(p.DataSources(ctx)) != 0 {
		t.Fatal("resources or data sources registered")
	}
}
