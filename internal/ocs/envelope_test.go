package ocs

import (
	"encoding/json"
	"encoding/xml"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type sample struct {
	Value string `xml:"value" json:"value"`
}

// TestSuccessStatusCodesDifferByVersion is the whole reason Version exists:
// clients read the field belonging to the endpoint they called, so v1 must say
// 100 and v2 must say 200.
func TestSuccessStatusCodesDifferByVersion(t *testing.T) {
	tests := []struct {
		version    Version
		wantStatus int
	}{
		{V1, 100},
		{V2, 200},
	}
	for _, tc := range tests {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/?format=json", nil)
		Write(rec, req, tc.version, sample{Value: "x"})

		if rec.Code != http.StatusOK {
			t.Errorf("v%d: HTTP status = %d, want 200", tc.version, rec.Code)
		}
		var got jsonEnvelope
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("v%d: decode: %v", tc.version, err)
		}
		if got.OCS.Meta.StatusCode != tc.wantStatus {
			t.Errorf("v%d: statuscode = %d, want %d", tc.version, got.OCS.Meta.StatusCode, tc.wantStatus)
		}
		if got.OCS.Meta.Status != "ok" {
			t.Errorf("v%d: status = %q, want ok", tc.version, got.OCS.Meta.Status)
		}
	}
}

// TestErrorHTTPStatusMapping: v1 always answers HTTP 200 and puts the real
// outcome in the envelope, while v2 mirrors it onto the status line.
func TestErrorHTTPStatusMapping(t *testing.T) {
	tests := []struct {
		name     string
		version  Version
		ocsCode  int
		wantHTTP int
	}{
		{"v1 keeps HTTP 200", V1, StatusForbidden, http.StatusOK},
		{"v2 mirrors 403", V2, StatusForbidden, http.StatusForbidden},
		{"v2 mirrors 404", V2, StatusNotFound, http.StatusNotFound},
		// 996 has no meaning as an HTTP status and would produce an invalid
		// response line, so it has to collapse to a real 5xx.
		{"v2 collapses non-HTTP code", V2, StatusError, http.StatusInternalServerError},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/?format=json", nil)
			WriteError(rec, req, tc.version, tc.ocsCode, "nope")

			if rec.Code != tc.wantHTTP {
				t.Errorf("HTTP status = %d, want %d", rec.Code, tc.wantHTTP)
			}
			var got jsonEnvelope
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if got.OCS.Meta.StatusCode != tc.ocsCode {
				t.Errorf("statuscode = %d, want %d", got.OCS.Meta.StatusCode, tc.ocsCode)
			}
			if got.OCS.Meta.Status != "failure" {
				t.Errorf("status = %q, want failure", got.OCS.Meta.Status)
			}
		})
	}
}

func TestFormatNegotiation(t *testing.T) {
	tests := []struct {
		name       string
		target     string
		accept     string
		wantPrefix string
	}{
		{"XML is the default", "/", "", "application/xml"},
		{"format=json wins", "/?format=json", "", "application/json"},
		{"Accept header honoured", "/", "application/json", "application/json"},
		// An explicit format parameter is a stronger signal than Accept, which
		// clients often send indiscriminately.
		{"format=xml beats Accept", "/?format=xml", "application/json", "application/xml"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tc.target, nil)
			if tc.accept != "" {
				req.Header.Set("Accept", tc.accept)
			}
			rec := httptest.NewRecorder()
			Write(rec, req, V2, sample{Value: "x"})

			if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, tc.wantPrefix) {
				t.Errorf("Content-Type = %q, want prefix %q", ct, tc.wantPrefix)
			}
		})
	}
}

func TestXMLEnvelopeShape(t *testing.T) {
	rec := httptest.NewRecorder()
	Write(rec, httptest.NewRequest(http.MethodGet, "/", nil), V1, sample{Value: "hello"})

	body := rec.Body.String()
	if !strings.HasPrefix(body, xml.Header) {
		t.Error("response is missing the XML declaration")
	}

	var got struct {
		XMLName xml.Name `xml:"ocs"`
		Meta    Meta     `xml:"meta"`
		Data    sample   `xml:"data"`
	}
	if err := xml.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.XMLName.Local != "ocs" {
		t.Errorf("root element = %q, want ocs", got.XMLName.Local)
	}
	if got.Meta.StatusCode != 100 {
		t.Errorf("statuscode = %d, want 100", got.Meta.StatusCode)
	}
	if got.Data.Value != "hello" {
		t.Errorf("data value = %q, want hello", got.Data.Value)
	}
}
