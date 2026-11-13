package protocol

import "encoding/json"

// Optional is a wire field that may be absent, an explicit JSON null, or a
// value. The field is absent when the containing pointer is nil. A non-nil
// Optional with Valid == false is an explicit null ("not readable", as opposed
// to "not applicable" which is simply absent).
type Optional[T any] struct {
	Valid bool
	Value T
}

// Some returns a present Optional holding v.
func Some[T any](v T) *Optional[T] {
	return &Optional[T]{Valid: true, Value: v}
}

// Null returns an explicit JSON null Optional.
func Null[T any]() *Optional[T] {
	return &Optional[T]{}
}

// Ptr returns a pointer to v, for required nullable and optional fields.
func Ptr[T any](v T) *T {
	return &v
}

// MarshalJSON encodes a present value or an explicit null.
func (o *Optional[T]) MarshalJSON() ([]byte, error) {
	if o == nil || !o.Valid {
		return []byte("null"), nil
	}
	return json.Marshal(o.Value)
}

// UnmarshalJSON accepts null or a value of T.
func (o *Optional[T]) UnmarshalJSON(data []byte) error {
	if string(data) == "null" {
		o.Valid = false
		var zero T
		o.Value = zero
		return nil
	}
	var v T
	if err := json.Unmarshal(data, &v); err != nil {
		return err
	}
	o.Valid = true
	o.Value = v
	return nil
}
