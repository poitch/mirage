// Package ocs implements the Open Collaboration Services endpoints that
// Nextcloud clients probe before they will talk to a server.
package ocs

import (
	"encoding/json"
	"encoding/xml"
	"net/http"
	"strings"
)

// Version selects the OCS envelope dialect. The two differ in how success is
// signalled, and clients check the field that belongs to the version they
// called, so the distinction has to be preserved end to end.
type Version int

const (
	// V1 reports success as statuscode 100 and always answers HTTP 200.
	V1 Version = 1
	// V2 reports success as statuscode 200 and mirrors it into the HTTP status.
	V2 Version = 2
)

// OCS status codes.
const (
	statusOKV1       = 100
	statusOKV2       = 200
	StatusBadRequest = 400
	StatusForbidden  = 403
	StatusNotFound   = 404
	StatusError      = 996
)

// Meta is the envelope header every OCS response carries.
type Meta struct {
	Status       string `xml:"status" json:"status"`
	StatusCode   int    `xml:"statuscode" json:"statuscode"`
	Message      string `xml:"message" json:"message"`
	TotalItems   string `xml:"totalitems" json:"totalitems"`
	ItemsPerPage string `xml:"itemsperpage" json:"itemsperpage"`
}

// xmlEnvelope is the XML wire form: <ocs><meta/><data/></ocs>.
type xmlEnvelope struct {
	XMLName xml.Name `xml:"ocs"`
	Meta    Meta     `xml:"meta"`
	Data    any      `xml:"data"`
}

// jsonEnvelope is the JSON wire form, which nests everything under "ocs".
type jsonEnvelope struct {
	OCS jsonBody `json:"ocs"`
}

type jsonBody struct {
	Meta Meta `json:"meta"`
	Data any  `json:"data"`
}

// okMeta builds the success header for the given version.
func okMeta(v Version) Meta {
	code := statusOKV1
	if v == V2 {
		code = statusOKV2
	}
	return Meta{Status: "ok", StatusCode: code, Message: "OK"}
}

// Write emits a successful OCS response carrying data.
func Write(w http.ResponseWriter, r *http.Request, v Version, data any) {
	write(w, r, v, okMeta(v), data, http.StatusOK)
}

// WriteError emits a failure response. code is an OCS status code; for V2 it is
// also used as the HTTP status, which is the behaviour V2 clients expect.
func WriteError(w http.ResponseWriter, r *http.Request, v Version, code int, message string) {
	httpStatus := http.StatusOK
	if v == V2 {
		// Only real HTTP statuses may be sent on the status line; OCS-specific
		// codes such as 996 have no HTTP meaning and would produce an invalid
		// response, so they collapse to 500.
		if code >= 400 && code <= 599 {
			httpStatus = code
		} else {
			httpStatus = http.StatusInternalServerError
		}
	}
	meta := Meta{Status: "failure", StatusCode: code, Message: message}
	write(w, r, v, meta, struct{}{}, httpStatus)
}

func write(w http.ResponseWriter, r *http.Request, v Version, meta Meta, data any, httpStatus int) {
	if wantsJSON(r) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(httpStatus)
		//nolint:errcheck // the connection is gone; nothing useful to do
		json.NewEncoder(w).Encode(jsonEnvelope{OCS: jsonBody{Meta: meta, Data: data}})
		return
	}
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.WriteHeader(httpStatus)
	//nolint:errcheck // as above
	w.Write([]byte(xml.Header))
	enc := xml.NewEncoder(w)
	enc.Indent("", "  ")
	//nolint:errcheck // as above
	enc.Encode(xmlEnvelope{Meta: meta, Data: data})
}

// wantsJSON reports whether the client asked for JSON. Clients signal this
// either with ?format=json or an Accept header; XML is the OCS default, so
// anything else falls back to it.
func wantsJSON(r *http.Request) bool {
	if format := r.URL.Query().Get("format"); format != "" {
		return format == "json"
	}
	return strings.Contains(r.Header.Get("Accept"), "application/json")
}
