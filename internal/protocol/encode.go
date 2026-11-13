package protocol

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
)

// EncodeCollectorMessage encodes one collector-to-hub message and validates the
// output by running the strict decoder over it. Senders must use this function
// rather than json.Marshal directly: marshalling alone cannot reject a typed-nil
// interface (for example a nil *CpuAvailable stored in MetricsPayload.CPU), and
// it does not enforce the wire size limits.
func EncodeCollectorMessage(message CollectorMessage) ([]byte, error) {
	if isNilValue(message) {
		return nil, errors.New("protocol: cannot encode a nil collector message")
	}
	data, err := json.Marshal(message)
	if err != nil {
		return nil, err
	}
	if _, err := DecodeCollectorMessage(data); err != nil {
		return nil, fmt.Errorf("protocol: encoded collector message fails validation: %w", err)
	}
	return data, nil
}

// EncodeHubMessage encodes one hub-to-collector message and validates the output
// by running the strict decoder over it. Senders must use this function rather
// than json.Marshal directly, for the reasons on EncodeCollectorMessage.
func EncodeHubMessage(message HubMessage) ([]byte, error) {
	if isNilValue(message) {
		return nil, errors.New("protocol: cannot encode a nil hub message")
	}
	data, err := json.Marshal(message)
	if err != nil {
		return nil, err
	}
	if _, err := DecodeHubMessage(data); err != nil {
		return nil, fmt.Errorf("protocol: encoded hub message fails validation: %w", err)
	}
	return data, nil
}

// isNilValue reports whether v is nil or a nil pointer, map, slice, channel or
// function behind a non-nil interface. Go's interface comparison treats a typed
// nil as non-nil, which would otherwise slip past the encoder's presence guards
// and emit JSON null.
func isNilValue(v any) bool {
	if v == nil {
		return true
	}
	value := reflect.ValueOf(v)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Ptr, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

// checkNoNilElements rejects a list that contains nil or typed-nil members.
// Lists of interface values would otherwise encode a nil member as JSON null.
func checkNoNilElements[T any](name string, values []T) error {
	for i, value := range values {
		if isNilValue(value) {
			return fmt.Errorf("protocol: %s[%d] is nil", name, i)
		}
	}
	return nil
}
