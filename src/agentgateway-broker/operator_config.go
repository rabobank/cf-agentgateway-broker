package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
)

func strictJSON(data []byte, out any) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	d.DisallowUnknownFields()
	if err := d.Decode(out); err != nil {
		return errors.New("invalid configuration fields or JSON types")
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return errors.New("unexpected trailing JSON")
	}
	return nil
}
