// Package goxml returns the root element of XML documents, with encoding/xml, which its contract's
// //plg:guest-packages adds to the Go guest.
package goxml

import (
	"context"
	"encoding/xml"

	"example.com/directives/rules"
)

type root struct{}

// Create checks that encoding/xml decodes in this guest.
func Create() (rules.XMLParser, error) {
	var doc struct {
		XMLName xml.Name
	}

	err := xml.Unmarshal([]byte("<campaign/>"), &doc)

	return root{}, err
}

func (root) Root(_ context.Context, doc string) (string, error) {
	var parsed struct {
		XMLName xml.Name
	}

	err := xml.Unmarshal([]byte(doc), &parsed)

	return parsed.XMLName.Local, err
}
