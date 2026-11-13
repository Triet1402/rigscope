package protocol

import "fmt"

// DecodeCollectorMessage decodes and strictly validates one collector-to-hub
// message. The size limits are enforced here: every message except
// detail.result may be at most MaxMessageBytes, and detail.result may be at
// most MaxDetailBytes.
func DecodeCollectorMessage(data []byte) (CollectorMessage, error) {
	if len(data) > MaxDetailBytes {
		return nil, fmt.Errorf("collector message: %d bytes exceeds maximum %d", len(data), MaxDetailBytes)
	}
	v, err := decodeJSONValue(data)
	if err != nil {
		return nil, err
	}
	message, err := parseCollectorMessageValue(v)
	if err != nil {
		return nil, err
	}
	if len(data) > MaxMessageBytes {
		if _, ok := message.(DetailResult); !ok {
			return nil, fmt.Errorf(
				"collector message: %d bytes exceeds maximum %d for %T",
				len(data), MaxMessageBytes, message,
			)
		}
	}
	return message, nil
}

// DecodeHubMessage decodes and strictly validates one hub-to-collector message.
// Every hub message is bounded by MaxMessageBytes.
func DecodeHubMessage(data []byte) (HubMessage, error) {
	if len(data) > MaxMessageBytes {
		return nil, fmt.Errorf("hub message: %d bytes exceeds maximum %d", len(data), MaxMessageBytes)
	}
	v, err := decodeJSONValue(data)
	if err != nil {
		return nil, err
	}
	return parseHubMessageValue(v)
}

// ParseTelemetryRecord decodes one standalone telemetry record.
func ParseTelemetryRecord(data []byte) (TelemetryRecord, error) {
	v, err := decodeJSONValue(data)
	if err != nil {
		return nil, err
	}
	return newParser().child("record").parseTelemetryRecord(v)
}

// ParseMetricsPayload decodes one standalone metrics payload.
func ParseMetricsPayload(data []byte) (MetricsPayload, error) {
	v, err := decodeJSONValue(data)
	if err != nil {
		return MetricsPayload{}, err
	}
	return newParser().child("metrics payload").parseMetricsPayload(v)
}

// ParseInventory decodes one standalone inventory object.
func ParseInventory(data []byte) (Inventory, error) {
	v, err := decodeJSONValue(data)
	if err != nil {
		return Inventory{}, err
	}
	return newParser().child("inventory").parseInventory(v)
}

// ParseDeliveryStatus decodes one standalone delivery status object.
func ParseDeliveryStatus(data []byte) (DeliveryStatus, error) {
	v, err := decodeJSONValue(data)
	if err != nil {
		return DeliveryStatus{}, err
	}
	return newParser().child("delivery").parseDeliveryStatus(v)
}

// ParseProcessSnapshot decodes one standalone process snapshot object.
func ParseProcessSnapshot(data []byte) (ProcessSnapshot, error) {
	v, err := decodeJSONValue(data)
	if err != nil {
		return ProcessSnapshot{}, err
	}
	return newParser().child("process snapshot").parseProcessSnapshot(v)
}
