package mcpconfig

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// JSONError describes configuration/management failures without echoing input.
// Parser errors can contain submitted values, so only structured details are used.
func JSONError(source string, err error) error {
	var syntax *json.SyntaxError
	var mismatch *json.UnmarshalTypeError
	switch {
	case errors.As(err, &syntax):
		return fmt.Errorf("%s: invalid JSON syntax at byte %d", source, syntax.Offset)
	case errors.As(err, &mismatch):
		return fmt.Errorf("%s: incorrect JSON type at byte %d; expected %s", source, mismatch.Offset, mismatch.Type)
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return fmt.Errorf("%s: empty or incomplete JSON", source)
	case strings.HasPrefix(err.Error(), "json: unknown field "):
		return fmt.Errorf("%s: unknown JSON field; check supported configuration fields", source)
	default:
		return fmt.Errorf("%s: JSON encoding or decoding failed", source)
	}
}
