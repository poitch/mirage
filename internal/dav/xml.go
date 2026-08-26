// Package dav implements the WebDAV surface Nextcloud clients speak.
package dav

import (
	"bufio"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// XML namespaces used on the wire.
const (
	NSDav       = "DAV:"
	NSOwnCloud  = "http://owncloud.org/ns"
	NSNextcloud = "http://nextcloud.org/ns"
	NSSabre     = "http://sabredav.org/ns"
)

// prefixes maps each namespace to the short prefix used in responses.
//
// Responses are written by hand rather than through encoding/xml because Go's
// encoder cannot emit prefixed names: it declares a default xmlns on every
// element instead. That is valid XML, but it produces output visibly unlike
// what clients receive from Nextcloud, and matching the real thing removes a
// whole class of compatibility risk for no real cost.
var prefixes = map[string]string{
	NSDav:       "d",
	NSOwnCloud:  "oc",
	NSNextcloud: "nc",
	NSSabre:     "s",
}

// ComplianceClasses is the DAV header Mirage sends.
//
// Class 2 is deliberately absent: it means WebDAV locking, which Mirage does
// not implement. Nextcloud advertises the same set unless its locking plugin is
// enabled, and its own sync clients do not use locks.
const ComplianceClasses = "1, 3, extended-mkcol"

// PropName identifies a WebDAV property.
type PropName struct {
	Space string
	Local string
}

func (p PropName) String() string { return p.Space + ":" + p.Local }

// tag renders the property as a prefixed element name, declaring the namespace
// inline when it is one we do not have a prefix for.
func (p PropName) tag() (name string, extraNS string) {
	if prefix, ok := prefixes[p.Space]; ok {
		return prefix + ":" + p.Local, ""
	}
	// An unrecognised namespace still has to be echoed back in the 404
	// propstat, since clients match the request against the response.
	return "x:" + p.Local, ` xmlns:x="` + escapeAttr(p.Space) + `"`
}

// propfindRequest is the parsed body of a PROPFIND.
type propfindRequest struct {
	XMLName  xml.Name       `xml:"DAV: propfind"`
	Prop     *propContainer `xml:"DAV: prop"`
	AllProp  *struct{}      `xml:"DAV: allprop"`
	PropName *struct{}      `xml:"DAV: propname"`
}

type propContainer struct {
	Names []anyElement `xml:",any"`
}

type anyElement struct {
	XMLName xml.Name
}

// parsePropfind reads a PROPFIND body and returns the requested properties.
// allProps is true when the client asked for everything, which is also how an
// empty body is defined to behave by RFC 4918.
func parsePropfind(r io.Reader) (names []PropName, allProps bool, err error) {
	body, err := io.ReadAll(io.LimitReader(r, maxPropfindBody))
	if err != nil {
		return nil, false, err
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		return nil, true, nil
	}

	var req propfindRequest
	if err := xml.Unmarshal(body, &req); err != nil {
		return nil, false, fmt.Errorf("malformed PROPFIND body: %w", err)
	}
	if req.AllProp != nil || req.Prop == nil {
		return nil, true, nil
	}

	names = make([]PropName, 0, len(req.Prop.Names))
	for _, n := range req.Prop.Names {
		names = append(names, PropName{Space: n.XMLName.Space, Local: n.XMLName.Local})
	}
	// A <prop/> with no children asks for nothing, which is legal but useless;
	// treat it as allprop so the client gets something back.
	if len(names) == 0 {
		return nil, true, nil
	}
	return names, false, nil
}

// maxPropfindBody bounds how much of a request body is read. Real PROPFIND
// bodies are a few hundred bytes.
const maxPropfindBody = 1 << 20

// proppatchRequest is the parsed body of a PROPPATCH.
type proppatchRequest struct {
	XMLName xml.Name `xml:"DAV: propertyupdate"`
	Set     []struct {
		Prop propContainer `xml:"DAV: prop"`
	} `xml:"DAV: set"`
	Remove []struct {
		Prop propContainer `xml:"DAV: prop"`
	} `xml:"DAV: remove"`
}

// parseProppatch returns every property a PROPPATCH tried to set or remove.
func parseProppatch(r io.Reader) ([]PropName, error) {
	body, err := io.ReadAll(io.LimitReader(r, maxPropfindBody))
	if err != nil {
		return nil, err
	}
	var req proppatchRequest
	if err := xml.Unmarshal(body, &req); err != nil {
		return nil, fmt.Errorf("malformed PROPPATCH body: %w", err)
	}

	var names []PropName
	collect := func(c propContainer) {
		for _, n := range c.Names {
			names = append(names, PropName{Space: n.XMLName.Space, Local: n.XMLName.Local})
		}
	}
	for _, set := range req.Set {
		collect(set.Prop)
	}
	for _, rm := range req.Remove {
		collect(rm.Prop)
	}
	return names, nil
}

// prop is a resolved property and its rendered XML content.
type prop struct {
	Name PropName
	// Value is raw XML placed inside the property element. Producers are
	// responsible for escaping; use escapeText for character data.
	Value string
}

// multistatus streams a 207 Multi-Status response.
//
// It streams rather than buffers because a Depth: infinity PROPFIND over a
// large tree would otherwise have to be held in memory in full.
type multistatus struct {
	w   *bufio.Writer
	err error
}

func newMultistatus(w http.ResponseWriter) *multistatus {
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.Header().Set("DAV", ComplianceClasses)
	w.WriteHeader(http.StatusMultiStatus)

	m := &multistatus{w: bufio.NewWriter(w)}
	m.print(xml.Header)
	m.print(`<d:multistatus xmlns:d="DAV:" xmlns:oc="http://owncloud.org/ns"` +
		` xmlns:nc="http://nextcloud.org/ns" xmlns:s="http://sabredav.org/ns">` + "\n")
	return m
}

// writeResponse emits one <d:response>, splitting resolved properties from
// those the resource does not have.
//
// The 404 propstat is not optional padding: clients compare what they asked for
// against what came back, and silently omitting a property is not the same
// answer as reporting it absent.
func (m *multistatus) writeResponse(href string, found []prop, missing []PropName) {
	m.print("  <d:response>\n")
	m.print("    <d:href>" + escapeText(href) + "</d:href>\n")

	if len(found) > 0 {
		m.print("    <d:propstat>\n      <d:prop>\n")
		for _, p := range found {
			tag, ns := p.Name.tag()
			if p.Value == "" {
				m.print("        <" + tag + ns + "/>\n")
				continue
			}
			m.print("        <" + tag + ns + ">" + p.Value + "</" + tag + ">\n")
		}
		m.print("      </d:prop>\n      <d:status>HTTP/1.1 200 OK</d:status>\n    </d:propstat>\n")
	}

	if len(missing) > 0 {
		m.print("    <d:propstat>\n      <d:prop>\n")
		for _, name := range missing {
			tag, ns := name.tag()
			m.print("        <" + tag + ns + "/>\n")
		}
		m.print("      </d:prop>\n      <d:status>HTTP/1.1 404 Not Found</d:status>\n    </d:propstat>\n")
	}

	m.print("  </d:response>\n")
}

// writeStatusResponse emits a response whose properties all share one status.
// It is what PROPPATCH needs: the properties carry no values, only an outcome.
func (m *multistatus) writeStatusResponse(href string, names []PropName, status string) {
	m.print("  <d:response>\n")
	m.print("    <d:href>" + escapeText(href) + "</d:href>\n")
	if len(names) > 0 {
		m.print("    <d:propstat>\n      <d:prop>\n")
		for _, name := range names {
			tag, ns := name.tag()
			m.print("        <" + tag + ns + "/>\n")
		}
		m.print("      </d:prop>\n      <d:status>" + escapeText(status) + "</d:status>\n    </d:propstat>\n")
	}
	m.print("  </d:response>\n")
}

func (m *multistatus) close() error {
	m.print("</d:multistatus>\n")
	if m.err != nil {
		return m.err
	}
	return m.w.Flush()
}

func (m *multistatus) print(s string) {
	if m.err != nil {
		return
	}
	if _, err := m.w.WriteString(s); err != nil {
		m.err = err
	}
}

// escapeText escapes character data for inclusion in an element body.
//
// encoding/xml's EscapeText is not used here because it also escapes quotes,
// which are legal in character data. ETags are quoted values, so that would
// render every one of them as &#34;...&#34; - correct, but visibly unlike what
// clients receive from Nextcloud, and this package exists to match that.
//
// Characters XML 1.0 cannot represent at all are dropped rather than escaped.
// A filename may contain them, and no encoding would make such a document
// parseable; losing a control character from a displayed name is far better
// than handing the client a response it must reject in full.
func escapeText(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch r {
		case '&':
			b.WriteString("&amp;")
		case '<':
			b.WriteString("&lt;")
		case '>':
			b.WriteString("&gt;")
		default:
			if isXMLChar(r) {
				b.WriteRune(r)
			}
		}
	}
	return b.String()
}

// isXMLChar reports whether r may appear in a well-formed XML 1.0 document.
func isXMLChar(r rune) bool {
	return r == 0x09 || r == 0x0A || r == 0x0D ||
		(r >= 0x20 && r <= 0xD7FF) ||
		(r >= 0xE000 && r <= 0xFFFD) ||
		(r >= 0x10000 && r <= 0x10FFFF)
}

// escapeAttr escapes a value for inclusion in a double-quoted attribute.
func escapeAttr(s string) string {
	r := strings.NewReplacer(
		"&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&apos;")
	return r.Replace(s)
}
