package api

import "fmt"

// HealthMode identifies the process that serves the API. The 0.2 Node
// reference hub reported "runtime-spike"; the 1.0 Go hub reports "go", and the
// captured fixtures are normalized to that value.
type HealthMode string

// HealthModeGo is the only 1.0 health mode.
const HealthModeGo HealthMode = "go"

// HealthResponse is GET /api/health. It is unauthenticated and used by the CLI,
// installers and reverse proxies, not by the browser UI.
//
//api:response
type HealthResponse struct {
	Status   string     `json:"status"`
	Version  string     `json:"version"` //api:length=1..64
	Mode     HealthMode `json:"mode"`
	HubRunID string     `json:"hubRunId"`
}

func (h HealthResponse) validate() error {
	if h.Status != "ok" {
		return fmt.Errorf("status must be %q", "ok")
	}
	if h.Mode != HealthModeGo {
		return fmt.Errorf("mode must be %q", HealthModeGo)
	}
	if h.Version == "" || utf16Len(h.Version) > 64 {
		return fmt.Errorf("version must be 1..64 characters")
	}
	if err := validateID(h.HubRunID, "hubRunId"); err != nil {
		return err
	}
	return nil
}
