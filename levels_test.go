package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestLevelManifestHasAllStages(t *testing.T) {
	data, err := os.ReadFile("levels.json")
	if err != nil {
		t.Fatal(err)
	}
	var levels []Level
	if err := json.Unmarshal(data, &levels); err != nil {
		t.Fatal(err)
	}
	if len(levels) != 35 {
		t.Fatalf("got %d levels, want 35", len(levels))
	}
	for i, level := range levels {
		if level.ID != i || level.Code == "" || level.Title == "" || level.Goal == "" || len(level.Commands) == 0 || !strings.HasPrefix(level.SourceURL, "https://overthewire.org/") {
			t.Errorf("level %d is incomplete: %+v", i, level)
		}
	}
}

func TestPublicLevelDataContainsNoSecrets(t *testing.T) {
	data, err := os.ReadFile("levels.json")
	if err != nil {
		t.Fatal(err)
	}
	var levels []map[string]any
	if err := json.Unmarshal(data, &levels); err != nil {
		t.Fatal(err)
	}
	for i, level := range levels {
		for key := range level {
			if strings.EqualFold(key, "solution") || strings.EqualFold(key, "password") {
				t.Errorf("level %d exposes field %q", i, key)
			}
		}
	}
	if strings.Contains(strings.ToLower(string(data)), "bandit0\"\n") && strings.Contains(string(data), "ZjLjTmM6FvvyRnrb2rfNWOZOTa6ip5If") {
		t.Fatal("level manifest contains a password from the pasted source")
	}
}
