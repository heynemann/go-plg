// Package goxmlplain is goxml for a contract without //plg:guest-packages: the default Go guest has no
// encoding/xml.
package goxmlplain

import (
	"context"
	"encoding/xml"

	"example.com/directives/rules"
)

type root struct{}

// Create checks that encoding/xml decodes in this guest.
func Create() (rules.Parser, error) {
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
