// SPDX-License-Identifier: MIT
// Copyright The Muster Authors

package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"sync"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/muster-io/terraform-provider-muster/internal/client"
)

// ManagedBy is the managed_by mark the provider writes on every create and update, under OpenTofu too.
const ManagedBy = "terraform"

// RouteListLockKey is the lock of the Route list, which muster_route_order and the creates and deletes of muster_route
// take.
const RouteListLockKey = "route-list"

// Object names an object of Muster in diagnostics, and maps the pointers of its problems to attributes.
type Object struct {
	// Kind is the object's kind as people read it, such as "Escalation chain".
	Kind string
	// Name is the object's name, or its public_id when it has none.
	Name string
	// Schema is the resource's schema: a pointer of a problem becomes an attribute path only when the schema has it.
	Schema *schema.Schema
	// Renames maps the API's field names to attribute names where they differ.
	Renames map[string]string
}

// ETagAttribute is the computed etag of every resource: the object's ETag from Muster's last answer.
func ETagAttribute() schema.StringAttribute {
	return schema.StringAttribute{
		Computed: true,
		MarkdownDescription: "The object's ETag from Muster's last answer, sent as `If-Match` with every update and " +
			"delete.",
	}
}

// PreconditionFailed is the error of a 412, and of a shared object whose managed part changed since the prior state.
func (o Object) PreconditionFailed() diag.Diagnostic {
	return diag.NewErrorDiagnostic(
		fmt.Sprintf(`%s "%s" changed in Muster since Terraform last read it`, o.Kind, o.Name),
		"Run terraform plan again, review the change and apply.")
}

// ErrorDiagnostics turns an error of the API, from the verb ("create", "read", "update", "delete") of the object, into
// diagnostics: a 412 into PreconditionFailed, each field error of a problem into an error at the attribute it points
// at, or on the resource with the pointer, with the line and column of a template error, and any other error into one
// error with the problem's title, detail and code.
func (o Object) ErrorDiagnostics(ctx context.Context, verb string, err error) diag.Diagnostics {
	var diags diag.Diagnostics
	summary := fmt.Sprintf(`Cannot %s %s "%s"`, verb, o.Kind, o.Name)
	var apiErr *client.Error
	if !errors.As(err, &apiErr) {
		diags.AddError(summary, err.Error())
		return diags
	}
	if apiErr.Status == http.StatusPreconditionFailed {
		diags.Append(o.PreconditionFailed())
		return diags
	}
	p := apiErr.Problem
	head := p.Title
	if p.Detail != "" {
		head += ": " + p.Detail
	}
	if p.Code != "" {
		head += fmt.Sprintf(" (%s)", p.Code)
	}
	if len(p.Errors) == 0 {
		diags.AddError(summary, fmt.Sprintf("Muster answered %d. %s", apiErr.Status, head))
		return diags
	}
	for _, fe := range p.Errors {
		detail := fe.Code
		if fe.Detail != "" {
			detail = fe.Detail + " (" + fe.Code + ")"
		}
		if fe.Line > 0 {
			detail += fmt.Sprintf(", line %d", fe.Line)
			if fe.Column > 0 {
				detail += fmt.Sprintf(", column %d", fe.Column)
			}
		}
		if attr, ok := o.attributePath(ctx, fe.Pointer); ok {
			diags.AddAttributeError(attr, summary, head+"\n\n"+detail)
			continue
		}
		diags.AddError(summary, fmt.Sprintf("%s\n\n%s at %q", head, detail, fe.Pointer))
	}
	return diags
}

func (o Object) attributePath(ctx context.Context, pointer string) (path.Path, bool) {
	if o.Schema == nil {
		return path.Empty(), false
	}
	p, ok := PointerPath(pointer, o.Renames)
	if !ok {
		return path.Empty(), false
	}
	if _, d := o.Schema.AttributeAtPath(ctx, p); d.HasError() {
		return path.Empty(), false
	}
	return p, true
}

// PointerPath turns the RFC 6901 pointer of a problem's field error, such as /a/0/b, into the attribute path a[0].b,
// with the API's field names renamed. A pointer at a query or path parameter (/query/<name>, /path/<name>) is not in
// the request body and has no attribute path.
func PointerPath(pointer string, renames map[string]string) (path.Path, bool) {
	if !strings.HasPrefix(pointer, "/") || len(pointer) == 1 || strings.HasPrefix(pointer, "/query/") ||
		strings.HasPrefix(pointer, "/path/") {
		return path.Empty(), false
	}
	var p path.Path
	for i, segment := range strings.Split(pointer[1:], "/") {
		segment = strings.ReplaceAll(strings.ReplaceAll(segment, "~1", "/"), "~0", "~")
		if index, err := strconv.Atoi(segment); err == nil && index >= 0 && segment == strconv.Itoa(index) {
			if i == 0 {
				return path.Empty(), false
			}
			p = p.AtListIndex(index)
			continue
		}
		if segment == "" {
			return path.Empty(), false
		}
		if renamed, ok := renames[segment]; ok {
			segment = renamed
		}
		if i == 0 {
			p = path.Root(segment)
		} else {
			p = p.AtName(segment)
		}
	}
	return p, true
}

// NotFound reports whether err is Muster's 404.
func NotFound(err error) bool {
	return client.StatusOf(err) == http.StatusNotFound
}

// RemoveIfNotFound removes the resource from the state when a read answered 404: the object was deleted in Muster,
// and the next plan creates it again.
func RemoveIfNotFound(ctx context.Context, err error, state *tfsdk.State) bool {
	if !NotFound(err) {
		return false
	}
	state.RemoveResource(ctx)
	return true
}

// ImportByID imports a resource by the public_id of its object into the attribute id.
func ImportByID(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}

// ParseCompositeID splits an import id <a>/<b> into its parts, named for the error, such as "team id", "user id".
func ParseCompositeID(id string, names ...string) ([]string, error) {
	parts := strings.Split(id, "/")
	ok := len(parts) == len(names)
	for _, part := range parts {
		ok = ok && part != ""
	}
	if !ok {
		want := make([]string, len(names))
		for i, n := range names {
			want[i] = "<" + n + ">"
		}
		return nil, fmt.Errorf("the import id %q is not %s", id, strings.Join(want, "/"))
	}
	return parts, nil
}

// RequireManagedBy is what every create and update calls on its request body: SetManagedBy, and an error when body
// has no managed_by field it can set. A union body (Connections, Destinations) has none: call it on the variant struct
// before From<Variant>. A field of an embedded struct is not found either.
func RequireManagedBy(body any) diag.Diagnostics {
	if SetManagedBy(body) {
		return nil
	}
	return diag.Diagnostics{diag.NewErrorDiagnostic("Provider bug: the request body has no managed_by",
		fmt.Sprintf("The provider cannot mark the request body %T as managed by Terraform: it is not a pointer to "+
			"a struct with a string field managed_by. Report this to the provider's maintainers.", body))}
}

// SetManagedBy sets the field of body, a pointer to a request body struct, whose JSON name is managed_by, to ManagedBy:
// a string, a pointer to one, or a nullable string, of any string type. It reports whether body has such a field; it
// returns false for a union body and for a field of an embedded struct. Resources call RequireManagedBy instead.
func SetManagedBy(body any) bool {
	v := reflect.ValueOf(body)
	if v.Kind() != reflect.Pointer || v.IsNil() || v.Elem().Kind() != reflect.Struct {
		return false
	}
	s := v.Elem()
	for i := range s.NumField() {
		f := s.Type().Field(i)
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name != "managed_by" || !f.IsExported() {
			continue
		}
		return setString(s.Field(i), ManagedBy)
	}
	return false
}

func setString(field reflect.Value, value string) bool {
	t := field.Type()
	switch {
	case t.Kind() == reflect.String:
		field.SetString(value)
	case t.Kind() == reflect.Pointer && t.Elem().Kind() == reflect.String:
		p := reflect.New(t.Elem())
		p.Elem().SetString(value)
		field.Set(p)
	case t.Kind() == reflect.Map && t.Key().Kind() == reflect.Bool && t.Elem().Kind() == reflect.String:
		m := reflect.MakeMapWithSize(t, 1)
		v := reflect.New(t.Elem()).Elem()
		v.SetString(value)
		m.SetMapIndex(reflect.ValueOf(true).Convert(t.Key()), v)
		field.Set(m)
	default:
		return false
	}
	return true
}

// keyedMutex holds one lock per key, kept only while someone holds or waits for it.
type keyedMutex struct {
	mu    sync.Mutex
	locks map[string]*countedMutex
}

type countedMutex struct {
	sync.Mutex
	refs int
}

func (k *keyedMutex) lock(key string) func() {
	k.mu.Lock()
	if k.locks == nil {
		k.locks = map[string]*countedMutex{}
	}
	m, ok := k.locks[key]
	if !ok {
		m = &countedMutex{}
		k.locks[key] = m
	}
	m.refs++
	k.mu.Unlock()

	m.Lock()
	return func() {
		m.Unlock()
		k.mu.Lock()
		m.refs--
		if m.refs == 0 {
			delete(k.locks, key)
		}
		k.mu.Unlock()
	}
}

var objectLocks keyedMutex

// LockObject takes the provider's lock of an object, by its public_id, or of a list, such as RouteListLockKey, and
// returns its release.
func LockObject(key string) func() {
	return objectLocks.lock(key)
}

// SharedWrite is a write of an object that several resources write (C-33.FR-8): under the object's lock it reads the
// object, compares the part the resource manages with its prior state, and writes with the ETag of that read.
type SharedWrite[T any] struct {
	// Key is the lock: the object's public_id, or the name of a list.
	Key    string
	Object Object
	// Read is the fresh read: the object and its ETag.
	Read func(ctx context.Context) (T, string, error)
	// Unchanged reports whether the part the resource manages is as in its prior state; nil skips the comparison.
	Unchanged func(current T) bool
	// Write sends the change with the ETag of the fresh read.
	Write func(ctx context.Context, current T, etag string) error
}

// Run runs the write; a part that changed since the prior state, or a 412, ends in PreconditionFailed without a retry.
func (w SharedWrite[T]) Run(ctx context.Context) diag.Diagnostics {
	unlock := LockObject(w.Key)
	defer unlock()
	current, etag, err := w.Read(ctx)
	if err != nil {
		return w.Object.ErrorDiagnostics(ctx, "read", err)
	}
	if w.Unchanged != nil && !w.Unchanged(current) {
		return diag.Diagnostics{w.Object.PreconditionFailed()}
	}
	if err := w.Write(ctx, current, etag); err != nil {
		return w.Object.ErrorDiagnostics(ctx, "update", err)
	}
	return nil
}

// SecretAttributes are the two ways of a write-only Secret of the API (C-33.FR-10): <name>, sensitive and stored in
// the state, and <name>_wo, write-only, with <name>_wo_version, whose change sends it again. At most one of the two is
// set.
func SecretAttributes(name, description string) map[string]schema.Attribute {
	wo := name + "_wo"
	return map[string]schema.Attribute{
		name: schema.StringAttribute{
			Optional:  true,
			Sensitive: true,
			MarkdownDescription: fmt.Sprintf("%s Stored in the state and hidden in plans. Conflicts with `%s`.",
				description, wo),
			Validators: []validator.String{secretConflict{other: wo}},
		},
		wo: schema.StringAttribute{
			Optional:  true,
			Sensitive: true,
			WriteOnly: true,
			MarkdownDescription: fmt.Sprintf("%s Write-only: never stored in the plan or the state; needs Terraform "+
				"1.11 or OpenTofu 1.11 or later. Sent on create and whenever `%s_version` changes. Conflicts with `%s`.",
				description, wo, name),
		},
		wo + "_version": schema.Int64Attribute{
			Optional:            true,
			MarkdownDescription: fmt.Sprintf("Change it to send `%s` again.", wo),
		},
	}
}

type secretConflict struct {
	other string
}

func (v secretConflict) Description(context.Context) string {
	return "conflicts with " + v.other
}

func (v secretConflict) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}

func (v secretConflict) ValidateString(ctx context.Context, req validator.StringRequest, resp *validator.StringResponse) {
	if req.ConfigValue.IsNull() {
		return
	}
	other := req.Path.ParentPath().AtName(v.other)
	var value types.String
	resp.Diagnostics.Append(req.Config.GetAttribute(ctx, other, &value)...)
	if resp.Diagnostics.HasError() || value.IsNull() {
		return
	}
	resp.Diagnostics.AddAttributeError(req.Path, "Conflicting Secret attributes",
		fmt.Sprintf("Set %s or %s, not both.", req.Path, other))
}

// SecretToSend reports whether a create or update sends the Secret <name> under base (path.Empty() at the root), and
// its value: the sensitive attribute on create and when it changed; the write-only one on create and when
// <name>_wo_version changed. state is nil on create.
func SecretToSend(ctx context.Context, base path.Path, name string, config tfsdk.Config, plan tfsdk.Plan,
	state *tfsdk.State) (string, bool, diag.Diagnostics) {
	var diags diag.Diagnostics
	sensitive := base.AtName(name)
	wo := base.AtName(name + "_wo")
	version := base.AtName(name + "_wo_version")
	creating := state == nil || state.Raw.IsNull()

	var planned, prior types.String
	diags.Append(plan.GetAttribute(ctx, sensitive, &planned)...)
	if !creating {
		diags.Append(state.GetAttribute(ctx, sensitive, &prior)...)
	}
	var writeOnly types.String
	diags.Append(config.GetAttribute(ctx, wo, &writeOnly)...)
	var plannedVersion, priorVersion types.Int64
	diags.Append(plan.GetAttribute(ctx, version, &plannedVersion)...)
	if !creating {
		diags.Append(state.GetAttribute(ctx, version, &priorVersion)...)
	}
	if diags.HasError() {
		return "", false, diags
	}
	switch {
	case !planned.IsNull() && !planned.IsUnknown():
		return planned.ValueString(), creating || !planned.Equal(prior), diags
	case !writeOnly.IsNull() && !writeOnly.IsUnknown():
		return writeOnly.ValueString(), creating || !plannedVersion.Equal(priorVersion), diags
	}
	return "", false, diags
}

// ValidateWriteOnly refuses every write-only attribute at paths that the configuration sets when the CLI cannot take
// write-only attributes, naming the versions that can.
func ValidateWriteOnly(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse,
	paths ...path.Path) {
	if req.ClientCapabilities.WriteOnlyAttributesAllowed {
		return
	}
	for _, p := range paths {
		var value types.String
		resp.Diagnostics.Append(req.Config.GetAttribute(ctx, p, &value)...)
		if value.IsNull() {
			continue
		}
		sensitive := strings.TrimSuffix(p.String(), "_wo")
		resp.Diagnostics.AddAttributeError(p, "Write-only attribute needs Terraform 1.11 or OpenTofu 1.11",
			fmt.Sprintf("%s is write-only, which needs Terraform 1.11 or OpenTofu 1.11 or later. Upgrade the CLI, or "+
				"set the Secret with %s, which is stored in the state.", p, sensitive))
	}
}
