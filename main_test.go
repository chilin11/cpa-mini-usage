package main

import (
	"encoding/json"
	"testing"
)

// CPA 8.0.22 sdk/pluginapi.Metadata has exported fields without JSON tags.
// In particular, github_repository does NOT decode into GitHubRepository.
func TestRegistrationMatchesCPA8022(t *testing.T) {
	// Avoid background collection; this test checks only the registration wire format.
	initOnce.Do(func() {})
	for _, method := range []string{"plugin.register", "plugin.reconfigure"} {
		t.Run(method, func(t *testing.T) {
			var response struct {
				OK     bool `json:"ok"`
				Result struct {
					SchemaVersion uint32 `json:"schema_version"`
					Metadata      struct {
						Name, Version, Author, GitHubRepository string
					} `json:"metadata"`
					Capabilities struct {
						UsagePlugin   bool `json:"usage_plugin"`
						ManagementAPI bool `json:"management_api"`
					} `json:"capabilities"`
				} `json:"result"`
			}
			if err := json.Unmarshal(dispatch(method, []byte(`{}`)), &response); err != nil {
				t.Fatal(err)
			}
			meta := response.Result.Metadata
			if !response.OK || meta.Name == "" || meta.Author == "" || meta.Version != pluginVersion || meta.GitHubRepository != "https://github.com/chilin11/cpa-mini-usage" {
				t.Fatalf("CPA would reject registration: %+v", response)
			}
			if response.Result.SchemaVersion != 2 || !response.Result.Capabilities.UsagePlugin || !response.Result.Capabilities.ManagementAPI {
				t.Fatalf("unexpected schema/capabilities: %+v", response.Result)
			}
		})
	}
}
