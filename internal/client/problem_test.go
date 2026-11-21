// SPDX-License-Identifier: MIT
// Copyright The Muster Authors

package client

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"unicode/utf8"
)

// check runs CheckResponse on an answer of PUT /api/v1/routes/RT1, or of no known request.
func check(status int, contentType, body string, withRequest bool) error {
	resp := &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{contentType}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
	defer resp.Body.Close()
	if withRequest {
		resp.Request = &http.Request{Method: http.MethodPut, URL: &url.URL{Path: "/api/v1/routes/RT1"}}
	}
	return CheckResponse(resp, []byte(body))
}

func TestCheckResponse_decodesAProblemWithFieldErrors(t *testing.T) {
	err := check(http.StatusUnprocessableEntity, "application/problem+json; charset=utf-8",
		`{"type":"https://muster-io.github.io/muster/problems/validation-failed","title":"Validation failed",`+
			`"status":422,"detail":"1 field is invalid.","code":"validation_failed",`+
			`"errors":[{"pointer":"/steps/0/wait_seconds","code":"out_of_range","detail":"at most 86400"}]}`, true)
	var apiErr *Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("error %v", err)
	}
	p := apiErr.Problem
	if p.Status != 422 || p.Title != "Validation failed" || p.Code != "validation_failed" || len(p.Errors) != 1 ||
		p.Errors[0] != (FieldError{Pointer: "/steps/0/wait_seconds", Code: "out_of_range", Detail: "at most 86400"}) {
		t.Fatalf("problem %+v", p)
	}
	if got, want := err.Error(),
		"PUT /api/v1/routes/RT1: 422 Validation failed (validation_failed): 1 field is invalid."; got != want {
		t.Fatalf("Error() = %q, want %q", got, want)
	}
	if StatusOf(fmt.Errorf("wrapped: %w", err)) != 422 {
		t.Fatal("StatusOf does not see through wrapping")
	}
}

func TestCheckResponse_keepsTheLineAndColumnOfATemplateError(t *testing.T) {
	err := check(http.StatusUnprocessableEntity, "application/problem+json",
		`{"type":"x","title":"Validation failed","status":422,"code":"validation_failed","errors":[`+
			`{"pointer":"/body_template","code":"template_syntax","detail":"unexpected }","line":3,"column":14},`+
			`{"pointer":"/name","code":"required"}]}`, true)
	var apiErr *Error
	if !errors.As(err, &apiErr) || len(apiErr.Problem.Errors) != 2 {
		t.Fatalf("error %v", err)
	}
	if got := apiErr.Problem.Errors[0]; got != (FieldError{Pointer: "/body_template", Code: "template_syntax",
		Detail: "unexpected }", Line: 3, Column: 14}) {
		t.Fatalf("field error %+v", got)
	}
	if got := apiErr.Problem.Errors[1]; got.Line != 0 || got.Column != 0 {
		t.Fatalf("field error %+v", got)
	}
}

func TestCheckResponse_cutsALongBodyAtARuneBoundary(t *testing.T) {
	for _, prefix := range []int{maxDetailBytes - 1, maxDetailBytes - 2, maxDetailBytes - 3, maxDetailBytes} {
		body := strings.Repeat("x", prefix) + strings.Repeat("ж€😀", 20)
		err := check(http.StatusBadGateway, "text/plain", body, true)
		var apiErr *Error
		if !errors.As(err, &apiErr) {
			t.Fatalf("error %v", err)
		}
		detail := apiErr.Problem.Detail
		text, ok := strings.CutSuffix(detail, "…")
		if !ok || !utf8.ValidString(detail) || len(text) > maxDetailBytes || len(text) < maxDetailBytes-3 ||
			!strings.HasPrefix(body, text) {
			t.Fatalf("prefix %d: detail of %d bytes, valid %v: %q", prefix, len(detail), utf8.ValidString(detail),
				detail[max(0, len(detail)-12):])
		}
	}
}

func TestCheckResponse_passes2xx(t *testing.T) {
	for _, status := range []int{200, 201, 204} {
		if err := check(status, "application/json", `{}`, true); err != nil {
			t.Fatalf("%d: %v", status, err)
		}
	}
}

func TestCheckResponse_withoutAProblem(t *testing.T) {
	err := check(http.StatusBadGateway, "text/html", "<html>"+strings.Repeat("x", 600)+"</html>", true)
	var apiErr *Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("error %v", err)
	}
	if apiErr.Problem.Title != "Bad Gateway" || len(apiErr.Problem.Detail) > maxDetailBytes+len("…") ||
		!strings.HasPrefix(apiErr.Problem.Detail, "<html>") {
		t.Fatalf("problem %+v", apiErr.Problem)
	}

	err = check(http.StatusNotFound, "application/json", `not json`, true)
	if StatusOf(err) != 404 || err.Error() != "PUT /api/v1/routes/RT1: 404 Not Found: not json" {
		t.Fatalf("error %v", err)
	}

	err = check(http.StatusConflict, "application/problem+json", `{"title":"Conflict","code":"duplicate"}`, true)
	if !errors.As(err, &apiErr) || apiErr.Problem.Status != 409 {
		t.Fatalf("error %v", err)
	}

	if err := check(http.StatusForbidden, "", ``, false); err.Error() != " : 403 Forbidden" {
		t.Fatalf("error %q", err)
	}
}

func TestCheckResponse_noAnswer(t *testing.T) {
	if err := CheckResponse(nil, nil); err == nil || StatusOf(err) != 0 {
		t.Fatalf("error %v", err)
	}
}
