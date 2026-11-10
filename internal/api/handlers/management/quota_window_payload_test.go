package management

import (
	"encoding/json"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestQuotaObservationPayloadIncludesWindows(t *testing.T) {
	future := time.Now().Add(48 * time.Hour).UTC().Truncate(time.Second)
	payload := quotaObservationPayloadForProvider("claude", coreauth.QuotaState{
		Windows: []coreauth.QuotaWindow{
			{Key: "five_hour", Kind: "5h", UsedPercent: 12.5, ObservedAt: time.Unix(10, 0), Source: "headers"},
			{Key: "seven_day_fable", Kind: "7d", Model: "fable", UsedPercent: 64, ResetsAt: future, ObservedAt: time.Unix(20, 0), Source: "poll"},
		},
	})
	raw, errMarshal := json.Marshal(payload)
	if errMarshal != nil {
		t.Fatalf("marshal payload: %v", errMarshal)
	}
	var decoded struct {
		Windows []struct {
			Key              string  `json:"key"`
			Kind             string  `json:"kind"`
			Model            string  `json:"model"`
			UsedPercent      float64 `json:"used_percent"`
			RemainingPercent float64 `json:"remaining_percent"`
			ResetsAt         *string `json:"resets_at"`
			ObservedAt       *string `json:"observed_at"`
			Source           string  `json:"source"`
			Stale            bool    `json:"stale"`
		} `json:"windows"`
	}
	if errUnmarshal := json.Unmarshal(raw, &decoded); errUnmarshal != nil {
		t.Fatalf("unmarshal payload: %v", errUnmarshal)
	}
	if len(decoded.Windows) != 2 {
		t.Fatalf("windows = %s", raw)
	}
	fiveHour, fable := decoded.Windows[0], decoded.Windows[1]
	if fiveHour.ResetsAt != nil || !fiveHour.Stale || fiveHour.RemainingPercent != 87.5 || fiveHour.Source != "headers" {
		t.Fatalf("five_hour window = %+v (raw %s)", fiveHour, raw)
	}
	if fable.Model != "fable" || fable.ResetsAt == nil || *fable.ResetsAt != future.Format(time.RFC3339) || fable.Stale || fable.Kind != "7d" {
		t.Fatalf("fable window = %+v (raw %s)", fable, raw)
	}

	if _, ok := quotaObservationPayloadForProvider("gemini", coreauth.QuotaState{Windows: []coreauth.QuotaWindow{{Key: "seven_day"}}})["windows"]; ok {
		t.Fatal("unsupported provider exposed windows")
	}
	if _, ok := quotaObservationPayload(coreauth.QuotaState{})["windows"]; ok {
		t.Fatal("empty quota exposed a windows key")
	}
}
