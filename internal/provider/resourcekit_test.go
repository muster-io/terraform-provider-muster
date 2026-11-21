// SPDX-License-Identifier: MIT
// Copyright The Muster Authors

package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
	"github.com/oapi-codegen/nullable"

	"github.com/muster-io/terraform-provider-muster/internal/client"
)

var chainSchema = schema.Schema{
	Attributes: map[string]schema.Attribute{
		"id":   schema.StringAttribute{Computed: true},
		"etag": ETagAttribute(),
		"name": schema.StringAttribute{Required: true},
		"steps": schema.ListNestedAttribute{
			Optional: true,
			NestedObject: schema.NestedAttributeObject{Attributes: map[string]schema.Attribute{
				"wait_seconds": schema.Int64Attribute{Optional: true},
			}},
		},
		"proxy": schema.SingleNestedAttribute{
			Optional:   true,
			Attributes: SecretAttributes("password", "The proxy's password."),
		},
	},
}

func chain() Object {
	return Object{Kind: "Escalation chain", Name: "payments", Schema: &chainSchema,
		Renames: map[string]string{"public_id": "id"}}
}

func apiError(status int, p client.Problem) error {
	return fmt.Errorf("wrapped: %w", &client.Error{Method: http.MethodPut, Path: "/api/v1/escalation-chains/EC1",
		Status: status, Problem: p})
}

func TestPreconditionFailed(t *testing.T) {
	d := chain().PreconditionFailed()
	if got := d.Summary() + ". " + d.Detail(); got != `Escalation chain "payments" changed in Muster since `+
		`Terraform last read it. Run terraform plan again, review the change and apply.` {
		t.Fatal(got)
	}
}

func TestErrorDiagnostics(t *testing.T) {
	ctx := t.Context()
	diags := chain().ErrorDiagnostics(ctx, "update", apiError(http.StatusPreconditionFailed, client.Problem{}))
	if len(diags) != 1 || !strings.Contains(diags[0].Summary(), "changed in Muster since Terraform last read it") {
		t.Fatalf("412: %v", diags)
	}

	diags = chain().ErrorDiagnostics(ctx, "update", apiError(http.StatusUnprocessableEntity, client.Problem{
		Title: "Validation failed", Detail: "2 fields are invalid.", Code: "validation_failed",
		Errors: []client.FieldError{
			{Pointer: "/steps/0/wait_seconds", Code: "out_of_range", Detail: "at most 86400"},
			{Pointer: "/owning_team_id", Code: "unknown_id"},
			{Pointer: "/public_id", Code: "invalid_format"},
		},
	}))
	if len(diags) != 3 {
		t.Fatalf("422: %v", diags)
	}
	first, ok := diags[0].(diag.DiagnosticWithPath)
	if !ok || !first.Path().Equal(path.Root("steps").AtListIndex(0).AtName("wait_seconds")) {
		t.Fatalf("first diagnostic %#v", diags[0])
	}
	if diags[0].Summary() != `Cannot update Escalation chain "payments"` || diags[0].Detail() !=
		"Validation failed: 2 fields are invalid. (validation_failed)\n\nat most 86400 (out_of_range)" {
		t.Fatalf("first diagnostic %q %q", diags[0].Summary(), diags[0].Detail())
	}
	if _, ok := diags[1].(diag.DiagnosticWithPath); ok || !strings.Contains(diags[1].Detail(), `unknown_id at "/owning_team_id"`) {
		t.Fatalf("second diagnostic %#v", diags[1])
	}
	if third, ok := diags[2].(diag.DiagnosticWithPath); !ok || !third.Path().Equal(path.Root("id")) {
		t.Fatalf("third diagnostic %#v", diags[2])
	}

	diags = chain().ErrorDiagnostics(ctx, "delete", apiError(http.StatusConflict, client.Problem{
		Title: "Conflict", Detail: "The chain is in use.", Code: "chain_in_use"}))
	if len(diags) != 1 || diags[0].Detail() != "Muster answered 409. Conflict: The chain is in use. (chain_in_use)" {
		t.Fatalf("409: %v", diags)
	}

	diags = chain().ErrorDiagnostics(ctx, "read", errors.New("connection refused"))
	if len(diags) != 1 || diags[0].Summary() != `Cannot read Escalation chain "payments"` ||
		diags[0].Detail() != "connection refused" {
		t.Fatalf("transport error: %v", diags)
	}

	diags = chain().ErrorDiagnostics(ctx, "create", apiError(http.StatusUnprocessableEntity, client.Problem{
		Title: "Validation failed", Code: "validation_failed",
		Errors: []client.FieldError{
			{Pointer: "/name", Code: "template_syntax", Detail: "unexpected }", Line: 3, Column: 14},
			{Pointer: "/name", Code: "template_syntax", Line: 2},
			{Pointer: "/query/cursor", Code: "invalid_format"},
			{Pointer: "/path/id", Code: "invalid_format"},
		},
	}))
	if len(diags) != 4 || diags[0].Detail() != "Validation failed (validation_failed)\n\nunexpected } "+
		"(template_syntax), line 3, column 14" || !strings.HasSuffix(diags[1].Detail(), "template_syntax, line 2") {
		t.Fatalf("template errors: %v", diags)
	}
	for i, pointer := range []string{"/query/cursor", "/path/id"} {
		d := diags[2+i]
		if _, ok := d.(diag.DiagnosticWithPath); ok || !strings.Contains(d.Detail(), fmt.Sprintf("at %q", pointer)) {
			t.Fatalf("%s: %#v", pointer, d)
		}
	}

	noSchema := Object{Kind: "Route", Name: "r"}
	diags = noSchema.ErrorDiagnostics(ctx, "create", apiError(422, client.Problem{Title: "Validation failed",
		Errors: []client.FieldError{{Pointer: "/name", Code: "required"}}}))
	if _, ok := diags[0].(diag.DiagnosticWithPath); ok {
		t.Fatal("a resource without a schema mapped a pointer")
	}
}

func TestPointerPath(t *testing.T) {
	renames := map[string]string{"public_id": "id"}
	for _, tc := range []struct {
		pointer string
		want    path.Path
		ok      bool
	}{
		{"/a/0/b", path.Root("a").AtListIndex(0).AtName("b"), true},
		{"/public_id", path.Root("id"), true},
		{"/x/public_id", path.Root("x").AtName("id"), true},
		{"/a~1b/c~0d", path.Root("a/b").AtName("c~d"), true},
		{"/steps/12", path.Root("steps").AtListIndex(12), true},
		{"/steps/012", path.Root("steps").AtName("012"), true},
		{"", path.Empty(), false},
		{"/", path.Empty(), false},
		{"steps", path.Empty(), false},
		{"/0/a", path.Empty(), false},
		{"/a//b", path.Empty(), false},
		{"/query/cursor", path.Empty(), false},
		{"/path/team_id", path.Empty(), false},
		{"/query", path.Root("query"), true},
		{"/queryable/a", path.Root("queryable").AtName("a"), true},
	} {
		got, ok := PointerPath(tc.pointer, renames)
		if ok != tc.ok || !got.Equal(tc.want) {
			t.Errorf("PointerPath(%q) = %s, %v; want %s, %v", tc.pointer, got, ok, tc.want, tc.ok)
		}
	}
}

func nullObject(t *testing.T, sch schema.Schema) tftypes.Value {
	t.Helper()
	return tftypes.NewValue(sch.Type().TerraformType(t.Context()), nil)
}

// objectValue is a value of sch with the given attributes and every other attribute null.
func objectValue(t *testing.T, sch schema.Schema, values map[string]tftypes.Value) tftypes.Value {
	t.Helper()
	typ, ok := sch.Type().TerraformType(t.Context()).(tftypes.Object)
	if !ok {
		t.Fatal("not an object type")
	}
	all := map[string]tftypes.Value{}
	for name, at := range typ.AttributeTypes {
		if v, ok := values[name]; ok {
			all[name] = v
		} else {
			all[name] = tftypes.NewValue(at, nil)
		}
	}
	return tftypes.NewValue(typ, all)
}

func TestRemoveIfNotFound(t *testing.T) {
	ctx := t.Context()
	state := tfsdk.State{Schema: chainSchema, Raw: objectValue(t, chainSchema, map[string]tftypes.Value{
		"id": tftypes.NewValue(tftypes.String, "EC1"), "name": tftypes.NewValue(tftypes.String, "payments")})}
	if RemoveIfNotFound(ctx, apiError(http.StatusConflict, client.Problem{}), &state) || state.Raw.IsNull() {
		t.Fatal("a 409 removed the resource")
	}
	if !RemoveIfNotFound(ctx, apiError(http.StatusNotFound, client.Problem{}), &state) || !state.Raw.IsNull() {
		t.Fatal("a 404 kept the resource")
	}
}

func TestImportByID(t *testing.T) {
	resp := &resource.ImportStateResponse{State: tfsdk.State{Schema: chainSchema, Raw: nullObject(t, chainSchema)}}
	ImportByID(t.Context(), resource.ImportStateRequest{ID: "EC1"}, resp)
	if resp.Diagnostics.HasError() {
		t.Fatal(resp.Diagnostics)
	}
	var id types.String
	resp.State.GetAttribute(t.Context(), path.Root("id"), &id)
	if id.ValueString() != "EC1" {
		t.Fatalf("id %s", id)
	}
}

func TestParseCompositeID(t *testing.T) {
	parts, err := ParseCompositeID("TM1/US2", "team id", "user id")
	if err != nil || len(parts) != 2 || parts[0] != "TM1" || parts[1] != "US2" {
		t.Fatalf("%v, %v", parts, err)
	}
	for _, id := range []string{"TM1", "TM1/", "/US2", "TM1/US2/x", ""} {
		if _, err := ParseCompositeID(id, "team id", "user id"); err == nil ||
			!strings.Contains(err.Error(), "is not <team id>/<user id>") {
			t.Errorf("%q: %v", id, err)
		}
	}
}

type managedByEnum string

func TestSetManagedBy(t *testing.T) {
	var plain struct {
		Name      string `json:"name"`
		ManagedBy string `json:"managed_by"`
	}
	var pointer struct {
		ManagedBy *string `json:"managed_by,omitempty"`
	}
	var enum struct {
		ManagedBy *managedByEnum `json:"managed_by,omitempty"`
	}
	var nullableString struct {
		ManagedBy nullable.Nullable[string] `json:"managed_by,omitempty"`
	}
	var nullableEnum struct {
		ManagedBy nullable.Nullable[managedByEnum] `json:"managed_by,omitempty"`
	}
	for _, body := range []any{&plain, &pointer, &enum, &nullableString, &nullableEnum} {
		if !SetManagedBy(body) {
			t.Fatalf("%T: not set", body)
		}
	}
	if plain.ManagedBy != ManagedBy || plain.Name != "" || *pointer.ManagedBy != ManagedBy ||
		*enum.ManagedBy != ManagedBy {
		t.Fatalf("%+v %+v %+v", plain, pointer, enum)
	}
	if v, err := nullableString.ManagedBy.Get(); err != nil || v != ManagedBy {
		t.Fatalf("nullable %v, %v", v, err)
	}
	if v, err := nullableEnum.ManagedBy.Get(); err != nil || v != ManagedBy {
		t.Fatalf("nullable enum %v, %v", v, err)
	}

	var without struct {
		Name    string `json:"name"`
		managed string
	}
	var wrongType struct {
		ManagedBy int `json:"managed_by"`
	}
	for _, body := range []any{&without, &wrongType, without, nil, (*struct{})(nil), new(string)} {
		if SetManagedBy(body) {
			t.Errorf("%T: set", body)
		}
	}
	if without.managed != "" || wrongType.ManagedBy != 0 {
		t.Fatal("a body without the field changed")
	}
}

// unionBody is a request body of a union, as oapi-codegen generates it: the variant is raw JSON.
type unionBody struct {
	union []byte
}

type embeddedManagedBy struct {
	ManagedBy *string `json:"managed_by,omitempty"`
}

func TestRequireManagedBy(t *testing.T) {
	var variant struct {
		Kind      string  `json:"kind"`
		ManagedBy *string `json:"managed_by,omitempty"`
	}
	if diags := RequireManagedBy(&variant); diags.HasError() || variant.ManagedBy == nil ||
		*variant.ManagedBy != ManagedBy {
		t.Fatalf("variant: %v, %+v", diags, variant)
	}

	var embedded struct {
		embeddedManagedBy
		Name string `json:"name"`
	}
	for _, body := range []any{&unionBody{union: []byte(`{"kind":"webhook"}`)}, &embedded, variant, nil} {
		diags := RequireManagedBy(body)
		if len(diags) != 1 || !diags.HasError() || !strings.Contains(diags[0].Summary(), "has no managed_by") ||
			!strings.Contains(diags[0].Detail(), fmt.Sprintf("%T", body)) {
			t.Errorf("%T: %v", body, diags)
		}
	}
	if embedded.ManagedBy != nil {
		t.Fatal("the field of an embedded struct was set")
	}
}

func TestLockObject_keepsNoLockAfterRelease(t *testing.T) {
	var wg sync.WaitGroup
	counter := 0
	for range 50 {
		wg.Go(func() {
			unlock := LockObject("TM1")
			counter++
			unlock()
		})
	}
	wg.Wait()
	if counter != 50 {
		t.Fatalf("counter %d", counter)
	}
	objectLocks.mu.Lock()
	defer objectLocks.mu.Unlock()
	if len(objectLocks.locks) != 0 {
		t.Fatalf("locks left: %v", objectLocks.locks)
	}
}

// teamServer is one shared object with an ETag that every write changes; a write with another ETag gets 412.
type teamServer struct {
	mu      sync.Mutex
	version int
	name    string
	log     []string
	writes  int
}

func (s *teamServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	etag := strconv.Quote(strconv.Itoa(s.version))
	switch r.Method {
	case http.MethodGet:
		s.log = append(s.log, "GET "+etag)
		w.Header().Set("ETag", etag)
		_, _ = io.WriteString(w, s.name)
	case http.MethodPut:
		ifMatch := r.Header.Get("If-Match")
		if ifMatch != etag {
			s.log = append(s.log, "PUT "+ifMatch+" 412")
			w.WriteHeader(http.StatusPreconditionFailed)
			return
		}
		s.log = append(s.log, "PUT "+ifMatch)
		s.writes++
		s.version++
		w.WriteHeader(http.StatusNoContent)
	}
}

func teamWrite(srv *httptest.Server, readHook func(), unchanged func(string) bool) SharedWrite[string] {
	return SharedWrite[string]{
		Key:    "TM1",
		Object: Object{Kind: "Team", Name: "payments"},
		Read: func(ctx context.Context) (string, string, error) {
			req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
			resp, err := srv.Client().Do(req)
			if err != nil {
				return "", "", err
			}
			defer resp.Body.Close()
			body, _ := io.ReadAll(resp.Body)
			if readHook != nil {
				readHook()
			}
			return string(body), client.ETag(resp), nil
		},
		Unchanged: unchanged,
		Write: func(ctx context.Context, _ string, etag string) error {
			req, _ := http.NewRequestWithContext(ctx, http.MethodPut, srv.URL, nil)
			req.Header.Set("If-Match", etag)
			resp, err := srv.Client().Do(req)
			if err != nil {
				return err
			}
			defer resp.Body.Close()
			return client.CheckResponse(resp, nil)
		},
	}
}

func lockWaiters(key string) int {
	objectLocks.mu.Lock()
	defer objectLocks.mu.Unlock()
	if m, ok := objectLocks.locks[key]; ok {
		return m.refs
	}
	return 0
}

func TestSharedWrite_twoWritersRunOneAfterTheOtherWithTheirOwnFreshETag(t *testing.T) {
	team := &teamServer{name: "payments"}
	srv := httptest.NewServer(team)
	defer srv.Close()

	inRead := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	first := teamWrite(srv, func() {
		once.Do(func() {
			close(inRead)
			<-release
		})
	}, nil)
	second := teamWrite(srv, nil, func(name string) bool { return name == "payments" })

	results := make(chan diag.Diagnostics, 2)
	go func() { results <- first.Run(t.Context()) }()
	<-inRead
	go func() { results <- second.Run(t.Context()) }()
	for lockWaiters("TM1") < 2 {
		runtime.Gosched()
	}
	close(release)
	for range 2 {
		if diags := <-results; diags.HasError() {
			t.Fatalf("a writer failed: %v", diags)
		}
	}
	want := []string{`GET "0"`, `PUT "0"`, `GET "1"`, `PUT "1"`}
	if strings.Join(team.log, ", ") != strings.Join(want, ", ") {
		t.Fatalf("requests %v, want %v", team.log, want)
	}
}

func TestSharedWrite_aChangedPartFailsWithoutWriting(t *testing.T) {
	team := &teamServer{name: "payments-renamed"}
	srv := httptest.NewServer(team)
	defer srv.Close()

	diags := teamWrite(srv, nil, func(name string) bool { return name == "payments" }).Run(t.Context())
	if len(diags) != 1 || diags[0].Summary() != `Team "payments" changed in Muster since Terraform last read it` {
		t.Fatalf("diagnostics %v", diags)
	}
	if team.writes != 0 || len(team.log) != 1 {
		t.Fatalf("requests %v", team.log)
	}
}

func TestSharedWrite_a412IsNotRetried(t *testing.T) {
	team := &teamServer{name: "payments"}
	srv := httptest.NewServer(team)
	defer srv.Close()
	w := teamWrite(srv, nil, nil)
	read := w.Read
	w.Read = func(ctx context.Context) (string, string, error) {
		name, _, err := read(ctx)
		return name, `"stale"`, err
	}
	diags := w.Run(t.Context())
	if len(diags) != 1 || !strings.Contains(diags[0].Summary(), "changed in Muster since Terraform last read it") {
		t.Fatalf("diagnostics %v", diags)
	}
	if want := []string{`GET "0"`, `PUT "stale" 412`}; strings.Join(team.log, ", ") != strings.Join(want, ", ") {
		t.Fatalf("requests %v, want %v", team.log, want)
	}
}

func TestSharedWrite_aFailedReadWritesNothing(t *testing.T) {
	w := SharedWrite[string]{
		Key:    "TM2",
		Object: Object{Kind: "Team", Name: "search"},
		Read: func(context.Context) (string, string, error) {
			return "", "", apiError(http.StatusNotFound, client.Problem{Title: "Not Found"})
		},
		Write: func(context.Context, string, string) error {
			t.Fatal("written")
			return nil
		},
	}
	if diags := w.Run(t.Context()); len(diags) != 1 || diags[0].Summary() != `Cannot read Team "search"` {
		t.Fatalf("diagnostics %v", diags)
	}
}

func secretValues(t *testing.T, sensitive, wo any, version any) map[string]tftypes.Value {
	t.Helper()
	return map[string]tftypes.Value{
		"name": tftypes.NewValue(tftypes.String, "payments"),
		"proxy": objectValue(t, schema.Schema{Attributes: SecretAttributes("password", "")},
			map[string]tftypes.Value{
				"password":            tftypes.NewValue(tftypes.String, sensitive),
				"password_wo":         tftypes.NewValue(tftypes.String, wo),
				"password_wo_version": tftypes.NewValue(tftypes.Number, version),
			}),
	}
}

func TestSecretAttributes(t *testing.T) {
	attrs := SecretAttributes("bot_token", "The bot's token.")
	sensitive, ok1 := attrs["bot_token"].(schema.StringAttribute)
	wo, ok2 := attrs["bot_token_wo"].(schema.StringAttribute)
	version, ok3 := attrs["bot_token_wo_version"].(schema.Int64Attribute)
	if !ok1 || !ok2 || !ok3 || len(attrs) != 3 {
		t.Fatalf("attributes %v", attrs)
	}
	if !sensitive.Sensitive || sensitive.WriteOnly || !wo.WriteOnly || !wo.Sensitive || !version.Optional {
		t.Fatalf("flags %+v %+v %+v", sensitive, wo, version)
	}
	if !strings.Contains(wo.MarkdownDescription, "Terraform 1.11 or OpenTofu 1.11") {
		t.Fatal(wo.MarkdownDescription)
	}
}

func TestSecretConflict(t *testing.T) {
	ctx := t.Context()
	v := secretConflict{other: "password_wo"}
	if v.MarkdownDescription(ctx) != "conflicts with password_wo" {
		t.Fatal(v.MarkdownDescription(ctx))
	}
	p := path.Root("proxy").AtName("password")
	for _, tc := range []struct {
		sensitive, wo any
		wantError     bool
	}{
		{"a", "b", true},
		{"a", nil, false},
		{nil, "b", false},
	} {
		config := tfsdk.Config{Schema: chainSchema, Raw: objectValue(t, chainSchema, secretValues(t, tc.sensitive,
			tc.wo, nil))}
		var value types.String
		config.GetAttribute(ctx, p, &value)
		resp := &validator.StringResponse{}
		v.ValidateString(ctx, validator.StringRequest{Path: p, ConfigValue: value, Config: config}, resp)
		if resp.Diagnostics.HasError() != tc.wantError {
			t.Errorf("%v/%v: %v", tc.sensitive, tc.wo, resp.Diagnostics)
		}
		if tc.wantError && !strings.Contains(resp.Diagnostics[0].Detail(),
			"Set proxy.password or proxy.password_wo, not both.") {
			t.Errorf("detail %q", resp.Diagnostics[0].Detail())
		}
	}
}

func TestSecretToSend(t *testing.T) {
	ctx := t.Context()
	base := path.Root("proxy")
	state := func(sensitive, version any) *tfsdk.State {
		return &tfsdk.State{Schema: chainSchema, Raw: objectValue(t, chainSchema, secretValues(t, sensitive, nil,
			version))}
	}
	for _, tc := range []struct {
		name          string
		sensitive, wo any
		version       any
		prior         *tfsdk.State
		want          string
		send          bool
	}{
		{"sensitive on create", "s1", nil, nil, nil, "s1", true},
		{"sensitive unchanged", "s1", nil, nil, state("s1", nil), "s1", false},
		{"sensitive changed", "s2", nil, nil, state("s1", nil), "s2", true},
		{"write-only on create", nil, "w1", 1, nil, "w1", true},
		{"write-only, same version", nil, "w2", 1, state(nil, 1), "w2", false},
		{"write-only, new version", nil, "w2", 2, state(nil, 1), "w2", true},
		{"write-only without version on create", nil, "w1", nil, &tfsdk.State{Schema: chainSchema,
			Raw: nullObject(t, chainSchema)}, "w1", true},
		{"neither", nil, nil, nil, state("s1", nil), "", false},
	} {
		// The plan never holds a write-only value; the configuration does.
		plan := tfsdk.Plan{Schema: chainSchema, Raw: objectValue(t, chainSchema, secretValues(t, tc.sensitive, nil,
			tc.version))}
		config := tfsdk.Config{Schema: chainSchema, Raw: objectValue(t, chainSchema, secretValues(t, tc.sensitive,
			tc.wo, tc.version))}
		got, send, diags := SecretToSend(ctx, base, "password", config, plan, tc.prior)
		if diags.HasError() || got != tc.want || send != tc.send {
			t.Errorf("%s: %q, %v, %v; want %q, %v", tc.name, got, send, diags, tc.want, tc.send)
		}
	}

	plan := tfsdk.Plan{Schema: chainSchema, Raw: nullObject(t, chainSchema)}
	if _, _, diags := SecretToSend(ctx, path.Root("nope"), "password", tfsdk.Config{Schema: chainSchema,
		Raw: nullObject(t, chainSchema)}, plan, nil); !diags.HasError() {
		t.Error("a path outside the schema passed")
	}
}

func TestValidateWriteOnly(t *testing.T) {
	ctx := t.Context()
	p := path.Root("proxy").AtName("password_wo")
	for _, tc := range []struct {
		allowed bool
		wo      any
		want    bool
	}{
		{false, "w", true},
		{true, "w", false},
		{false, nil, false},
	} {
		req := resource.ValidateConfigRequest{Config: tfsdk.Config{Schema: chainSchema,
			Raw: objectValue(t, chainSchema, secretValues(t, nil, tc.wo, nil))}}
		req.ClientCapabilities.WriteOnlyAttributesAllowed = tc.allowed
		resp := &resource.ValidateConfigResponse{}
		ValidateWriteOnly(ctx, req, resp, p)
		if resp.Diagnostics.HasError() != tc.want {
			t.Errorf("allowed %v, value %v: %v", tc.allowed, tc.wo, resp.Diagnostics)
		}
		if tc.want {
			d := resp.Diagnostics[0]
			if !strings.Contains(d.Summary(), "Terraform 1.11 or OpenTofu 1.11") ||
				!strings.Contains(d.Detail(), "set the Secret with proxy.password,") {
				t.Errorf("diagnostic %q %q", d.Summary(), d.Detail())
			}
		}
	}
}
