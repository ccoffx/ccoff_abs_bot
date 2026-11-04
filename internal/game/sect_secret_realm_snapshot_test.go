package game

import (
	"encoding/json"
	"testing"
)

func TestSecretRealmSnapshotRequiresCurrentFields(t *testing.T) {
	profile := DefaultSectSecretRealmConfig().Profiles[0]
	snapshot := sectSecretRealmProfileSnapshot(profile)
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(snapshot), &fields); err != nil {
		t.Fatal(err)
	}
	delete(fields, "guardian_bonuses")
	missingField, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, key, snapshot string
		valid               bool
	}{
		{"complete", profile.Key, snapshot, true},
		{"empty", profile.Key, "", false},
		{"empty object", profile.Key, "{}", false},
		{"incomplete", profile.Key, string(missingField), false},
		{"different profile", sectSecretRealmProfileHigh, snapshot, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := sectSecretRealmProfileFromSnapshotChecked(tc.key, tc.snapshot)
			if (err == nil) != tc.valid {
				t.Fatalf("snapshot accepted=%t, want %t: %v", err == nil, tc.valid, err)
			}
		})
	}
}
