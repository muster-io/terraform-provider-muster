// SPDX-License-Identifier: MIT
// Copyright The Muster Authors

package client

import (
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"strings"
	"unicode/utf8"
)

const maxDetailBytes = 512

// Problem is an RFC 9457 problem as Muster answers it, with the field errors of a refused request body.
type Problem struct {
	Type   string       `json:"type"`
	Title  string       `json:"title"`
	Status int          `json:"status"`
	Detail string       `json:"detail"`
	Code   string       `json:"code"`
	Errors []FieldError `json:"errors"`
}

// FieldError is one entry of a problem's errors: an RFC 6901 pointer into the request body and its code, and, for an
// error in a template, its line and column, 0 when the problem has none.
type FieldError struct {
	Pointer string `json:"pointer"`
	Code    string `json:"code"`
	Detail  string `json:"detail"`
	Line    int    `json:"line"`
	Column  int    `json:"column"`
}

// Error is an answer of Muster outside 2xx.
type Error struct {
	Method  string
	Path    string
	Status  int
	Problem Problem
}

func (e *Error) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s: %d", e.Method, e.Path, e.Status)
	if e.Problem.Title != "" {
		fmt.Fprintf(&b, " %s", e.Problem.Title)
	}
	if e.Problem.Code != "" {
		fmt.Fprintf(&b, " (%s)", e.Problem.Code)
	}
	if e.Problem.Detail != "" {
		fmt.Fprintf(&b, ": %s", e.Problem.Detail)
	}
	return b.String()
}

// StatusOf returns the status of an *Error in err's chain, or 0.
func StatusOf(err error) int {
	var apiErr *Error
	if errors.As(err, &apiErr) {
		return apiErr.Status
	}
	return 0
}

// CheckResponse returns nil for a 2xx answer and an *Error with the decoded problem otherwise.
func CheckResponse(resp *http.Response, body []byte) error {
	if resp == nil {
		return errors.New("no answer from Muster")
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	apiErr := &Error{Status: resp.StatusCode}
	if resp.Request != nil {
		apiErr.Method = resp.Request.Method
		apiErr.Path = resp.Request.URL.Path
	}
	apiErr.Problem = decodeProblem(resp.StatusCode, resp.Header.Get("Content-Type"), body)
	return apiErr
}

func decodeProblem(status int, contentType string, body []byte) Problem {
	var p Problem
	mediaType, _, _ := mime.ParseMediaType(contentType)
	if mediaType == "application/problem+json" || mediaType == "application/json" {
		if err := json.Unmarshal(body, &p); err == nil && (p.Title != "" || p.Code != "" || p.Type != "") {
			if p.Status == 0 {
				p.Status = status
			}
			return p
		}
	}
	p = Problem{Status: status, Title: http.StatusText(status)}
	if text := strings.TrimSpace(string(body)); text != "" {
		if len(text) > maxDetailBytes {
			cut := maxDetailBytes
			for cut > 0 && !utf8.RuneStart(text[cut]) {
				cut--
			}
			text = text[:cut] + "…"
		}
		p.Detail = text
	}
	return p
}
