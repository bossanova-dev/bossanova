package session

import (
	"encoding/json"
	"testing"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

func TestNewSessionJSONCarriesEffectiveModel(t *testing.T) {
	payload, err := json.Marshal(newSessionJSON(&pb.Session{Id: "created-session", EffectiveModel: "effective-model"}))
	if err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		Session struct {
			Model string `json:"model"`
		} `json:"session"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Session.Model != "effective-model" {
		t.Fatalf("model = %q, want effective-model", envelope.Session.Model)
	}
}
